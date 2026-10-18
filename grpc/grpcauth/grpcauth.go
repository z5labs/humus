// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcauth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/internal/authn"
)

// loggerName is the name of the logger the interceptors log through.
const loggerName = "github.com/z5labs/humus/grpc/grpcauth"

// schemeKey is the record attribute naming the scheme whose verifier
// failed.
const schemeKey = attribute.Key("humus.auth.scheme")

// The fixed errors of an RPC that is refused, or whose verifier failed
// with an error that has no gRPC status of its own. No verifier's text
// reaches the client through them.
var (
	errUnauthenticated = status.Error(codes.Unauthenticated, "unauthenticated")
	errForbidden       = status.Error(codes.PermissionDenied, "forbidden")
	errFailed          = status.Error(codes.Internal, "authentication failed")
)

// Rule is the requirement of the methods Default or Method names. The zero
// Rule is a mistake that Interceptors reports.
type Rule struct {
	r *rule
}

// rule is a Rule's declaration.
type rule struct {
	isDefault  bool
	fullMethod string // Method's
	reqs       []auth.Requirement
}

// Default is the requirement of every method no Method names: one of
// reqs. Default(auth.Disabled()) makes those methods public.
func Default(reqs ...auth.Requirement) Rule {
	return Rule{&rule{isDefault: true, reqs: slices.Clone(reqs)}}
}

// Method is the requirement of the method fullMethod, such as a generated
// Service_Method_FullMethodName constant: one of reqs.
// Method(m, auth.Disabled()) makes m public.
func Method(fullMethod string, reqs ...auth.Requirement) Rule {
	return Rule{&rule{fullMethod: fullMethod, reqs: slices.Clone(reqs)}}
}

// Interceptors returns a unary and a stream interceptor, as server options
// for grpcserver.NewService, which serve an RPC only when it meets its
// method's requirement, with its principals in the handler's context, or
// the stream's. Both are needed: the unary one sees only unary methods,
// and the stream one only streaming methods.
//
// It returns one error that names every mistake:
//   - a nil s, and the zero Rule;
//   - no Default, or two;
//   - a method given twice, or a name that isn't /package.Service/Method;
//   - a Default or a Method with no requirement;
//   - an APIKeyQuery or APIKeyCookie scheme, which an RPC doesn't have;
//   - two different schemes with one name, in one rule or in two;
//   - every mistake in the requirements that auth.NewHandler reports:
//     the zero Scheme or Requirement, a nil verify function, Disabled
//     beside another requirement or inside an All, and the rest.
//
// It logs through s.Telemetry's LoggerProvider, with the logger name
// "github.com/z5labs/humus/grpc/grpcauth".
func Interceptors(s *humus.Setup, rules ...Rule) (unary, stream grpc.ServerOption, err error) {
	c := compiler{methods: make(map[string]*authn.List), schemes: make(map[string]*authn.Scheme), reported: make(map[string]bool)}
	if s == nil {
		c.fail("s is nil")
	}
	defaults := 0
	given := make(map[string]bool)
	for i, r := range rules {
		at := fmt.Sprintf("rules[%d]", i)
		if r.r == nil {
			c.fail("%s is the zero Rule", at)
			continue
		}
		if r.r.isDefault {
			defaults++
			at += ": Default"
			if defaults > 1 {
				c.fail("%s is a second Default: give one", at)
			}
		} else {
			at += fmt.Sprintf(": Method(%q)", r.r.fullMethod)
			if !validMethod(r.r.fullMethod) {
				c.fail("%s names no method: a method's full name is /package.Service/Method", at)
			} else if given[r.r.fullMethod] {
				c.fail("%s gives a method that an earlier rule gives", at)
			}
			given[r.r.fullMethod] = true
		}
		l := c.compile(at, r.r.reqs)
		switch {
		case l == nil:
		case r.r.isDefault:
			c.def = l
		default:
			c.methods[r.r.fullMethod] = l
		}
	}
	if defaults == 0 {
		c.fail("no Default: give one, such as Default(auth.Disabled()) for methods that are public unless listed")
	}
	if err := errors.Join(c.errs...); err != nil {
		return nil, nil, fmt.Errorf("grpcauth: Interceptors: %w", err)
	}
	i := &interceptor{
		def:     c.def,
		methods: c.methods,
		logger:  s.Telemetry.LoggerProvider.Logger(loggerName),
	}
	return grpc.ChainUnaryInterceptor(i.unary), grpc.ChainStreamInterceptor(i.stream), nil
}

// compiler collects Interceptors' mistakes, and the lists it compiled.
type compiler struct {
	errs     []error
	def      *authn.List
	methods  map[string]*authn.List
	schemes  map[string]*authn.Scheme // the first of each name, in any rule
	reported map[string]bool          // the names already reported as two schemes'
}

func (c *compiler) fail(format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf(format, args...))
}

// compile compiles one rule's requirements, reporting their mistakes after
// at, and returns nil when they have one.
func (c *compiler) compile(at string, reqs []auth.Requirement) *authn.List {
	if len(reqs) == 0 {
		c.fail("%s has no requirement: give it auth.Disabled() to make it public", at)
		return nil
	}
	l, errs := authn.Compile(reqs)
	for _, err := range errs {
		c.fail("%s: %w", at, err)
	}
	if l == nil {
		return nil
	}
	bad := false
	for _, s := range l.Schemes() {
		if s.Kind == authn.APIKeyQuery || s.Kind == authn.APIKeyCookie {
			bad = true
			c.fail("%s: the %s scheme %q reads a %s, which an RPC doesn't have: use APIKeyHeader, which reads metadata", at, s.Kind, s.Name, place(s.Kind))
		}
		first, ok := c.schemes[s.Name]
		switch {
		case !ok:
			c.schemes[s.Name] = s
		case !first.Same(s) && !c.reported[s.Name]:
			c.reported[s.Name] = true
			bad = true
			c.fail("%s: two different schemes are named %q", at, s.Name)
		}
	}
	if bad {
		return nil
	}
	return l
}

// place returns where an API key of kind k is read from.
func place(k authn.Kind) string {
	if k == authn.APIKeyQuery {
		return "query parameter"
	}
	return "cookie"
}

// validMethod reports whether name is a full method name,
// /package.Service/Method: a service qualified by its package, and a
// method, each made of identifiers.
func validMethod(name string) bool {
	rest, ok := strings.CutPrefix(name, "/")
	if !ok {
		return false
	}
	service, method, ok := strings.Cut(rest, "/")
	if !ok || !isIdent(method) {
		return false
	}
	parts := strings.Split(service, ".")
	if len(parts) < 2 {
		return false
	}
	return !slices.ContainsFunc(parts, func(p string) bool { return !isIdent(p) })
}

// isIdent reports whether s is a protobuf identifier: a letter, then
// letters, digits and underscores.
func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// interceptor authenticates every RPC against its method's list, or the
// Default's.
type interceptor struct {
	def     *authn.List
	methods map[string]*authn.List
	logger  log.Logger
}

// unary serves a unary RPC that meets its method's requirement, with its
// principals in the handler's context.
func (i *interceptor) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, err := i.authenticate(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// stream serves a streaming RPC that meets its method's requirement, with
// its principals in the stream's context.
func (i *interceptor) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := i.authenticate(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	if ctx != ss.Context() {
		ss = &serverStream{ServerStream: ss, ctx: ctx}
	}
	return handler(srv, ss)
}

// serverStream is a stream whose context carries its principals.
type serverStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *serverStream) Context() context.Context { return s.ctx }

// authenticate authenticates the RPC to fullMethod, whose context is ctx.
// It returns the context to serve it with, or the error to end it with.
//
// It recovers nothing: a verifier's panic passes to grpcserver's recovery.
func (i *interceptor) authenticate(ctx context.Context, fullMethod string) (context.Context, error) {
	l, ok := i.methods[fullMethod]
	if !ok {
		l = i.def
	}
	res := l.Authenticate(ctx, newSource(ctx))
	switch res.Outcome {
	case authn.Served:
		return res.Context, nil
	case authn.Unauthenticated:
		return nil, errUnauthenticated
	case authn.Forbidden:
		return nil, errForbidden
	}
	i.record(ctx, fullMethod, res.Scheme, res.Err)
	if st, ok := status.FromError(res.Err); ok && st.Code() != codes.OK {
		return nil, st.Err()
	}
	return nil, errFailed
}

// record records a verifier's error as an exception event on the RPC's
// span, and as one ERROR record, "authentication failed", in the RPC's
// context, so the record carries the span's trace and span IDs. When the
// RPC's context is done, because the client went away, err goes on the
// span alone. No principal, subject or key is recorded.
func (i *interceptor) record(ctx context.Context, fullMethod, scheme string, err error) {
	trace.SpanFromContext(ctx).RecordError(err)
	if ctx.Err() != nil {
		return
	}
	var rec log.Record
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(log.SeverityError)
	rec.SetSeverityText(log.SeverityError.String())
	rec.SetBody(attribute.StringValue("authentication failed"))
	// rpc.method is the method without its leading slash, as on the span.
	rec.AddAttributes(
		schemeKey.String(scheme),
		semconv.ErrorType(err),
		semconv.ExceptionMessage(err.Error()),
		semconv.RPCMethod(strings.TrimPrefix(fullMethod, "/")),
	)
	i.logger.Emit(ctx, rec)
}
