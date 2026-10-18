// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
)

// Client is a grpc.ClientConnInterface, so generated code takes it:
//
//	cartpb.NewCartServiceClient(client)
//
// It is safe for concurrent use. One Client is one dependency: one
// connection and one retry budget. It has no Close: NewClient registers the
// connection's close with the Setup's OnStop.
type Client struct {
	cc      *grpc.ClientConn
	timeout time.Duration

	// retrier retries every method's calls, from every goroutine, with the
	// Client's one budget.
	retrier  *retry.Retrier
	classify func(method string, err error, trailer metadata.MD) Classification

	// connected is Connected's latch, set by the first check that finds
	// the connection ready. Every monitor Connected returns reads it.
	connected atomic.Bool
}

var _ grpc.ClientConnInterface = (*Client)(nil)

// NewClient runs every check on settings, and rejects a BackoffMax below
// BackoffBase and a nil Classify, reporting every bad field in one error
// that names each one. It also rejects a nil s, and an s whose Host has no
// Dial, rather than let grpc-go fall back to dialling the real network. It
// builds the Client, with one retry.Budget and one retry.Retrier, shared by
// every method and every goroutine. It opens no connection: the first RPC
// dials.
//
// The Client dials settings.Addr through s.Host, with grpc-go's passthrough
// resolver, so no name is looked up but by the Host.
//
// humus's first dial option is otelgrpc's client stats handler, given the
// TracerProvider, the MeterProvider and the Propagator of s.Telemetry, so
// each attempt is a client span and is measured, and nothing falls back to
// OpenTelemetry's globals. A stats handler in opts is a second one, which
// grpc-go calls as well.
//
// opts come after humus's own dial options, and the transport credentials
// built from settings.TLS come after opts, so a
// grpc.WithTransportCredentials in opts has no effect. An option that
// chains, such as grpc.WithChainUnaryInterceptor, runs inside the Client's
// retries, once per attempt.
//
// NewClient registers the connection's close with s.OnStop.
func NewClient(s *humus.Setup, settings Settings, opts ...grpc.DialOption) (*Client, error) {
	if s == nil {
		return nil, errors.New("grpcclient: NewClient: the Setup must not be nil")
	}
	err := checkSettings(settings)
	if s.Host.Dial == nil {
		err = errors.Join(errors.New("the Setup's Host.Dial must not be nil"), err)
	}
	if err != nil {
		return nil, fmt.Errorf("grpcclient: NewClient: %w", err)
	}
	retrier, err := newRetrier(settings)
	if err != nil {
		// checkSettings accepts only what retry accepts, so this is
		// humus's bug.
		return nil, fmt.Errorf("grpcclient: NewClient: %w", err)
	}

	dial := s.Host.Dial
	creds := insecure.NewCredentials()
	if settings.TLS != nil {
		// credentials.NewTLS clones the config, so the end user's is never
		// changed, and adds h2 to its ALPN.
		creds = credentials.NewTLS(settings.TLS)
	}

	// The stats handler comes first, so it is the first one grpc-go calls,
	// and a stats handler in opts is a second one. Every provider and the
	// propagator are passed, so otelgrpc never falls back to
	// OpenTelemetry's globals.
	dopts := make([]grpc.DialOption, 0, len(opts)+5)
	dopts = append(dopts,
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(s.Telemetry.TracerProvider),
			otelgrpc.WithMeterProvider(s.Telemetry.MeterProvider),
			otelgrpc.WithPropagators(s.Telemetry.Propagator),
		)),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return dial(ctx, "tcp", addr)
		}),
		grpc.WithNoProxy(),
		grpc.WithDisableRetry(),
	)
	dopts = append(dopts, opts...)
	dopts = append(dopts, grpc.WithTransportCredentials(creds))

	cc, err := grpc.NewClient("passthrough:///"+settings.Addr, dopts...)
	if err != nil {
		return nil, fmt.Errorf("grpcclient: NewClient: %w", err)
	}
	s.OnStop(func(context.Context) error {
		return cc.Close()
	})
	return &Client{
		cc:       cc,
		timeout:  settings.Timeout,
		retrier:  retrier,
		classify: settings.Classify,
	}, nil
}

// Invoke makes a unary RPC, retrying it as the package doc describes, and
// returns the last attempt's error. Settings.Timeout bounds the whole call,
// its retries and the waits between them included, unless ctx has an
// earlier deadline, which then wins. It is called by generated code.
func (c *Client) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var err error
	for a := range c.retrier.Attempts(ctx) {
		var trailer metadata.MD
		// Clip, so that the trailer's option is never appended into the
		// caller's array, which another call may share.
		callOpts := append(slices.Clip(opts), grpc.Trailer(&trailer))
		r := c.attempt(ctx, method, &trailer, func(ctx context.Context) error {
			return c.cc.Invoke(ctx, method, args, reply, callOpts...)
		})
		a.Done(r)
		err = r.err
	}
	return err
}

// NewStream opens a stream, retrying the open as the package doc
// describes. Settings.Timeout doesn't apply to it: a stream lives as long
// as ctx, which its caller ends. The stream is never retried once open. It
// is called by generated code.
func (c *Client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	var (
		stream grpc.ClientStream
		err    error
	)
	for a := range c.retrier.Attempts(ctx) {
		var r *result
		stream, r = c.open(ctx, desc, method, opts)
		a.Done(r)
		err = r.err
	}
	return stream, err
}

// Connected returns a monitor that is healthy once the Client has
// connected to Addr: unhealthy until its connection is first ready, its
// TLS handshake done, and healthy from then until the connection is
// closed. A later outage doesn't make it unhealthy. It never returns an
// error. A service is made ready once its dependency is reachable with
//
//	health.Readiness(client.Connected())
//
// Until the connection is first ready, each check starts it, if no RPC
// has, and waits until it is ready or the check's context is done. It
// makes no RPC. It is for readiness only: restarting the pod doesn't bring
// a dependency back.
func (c *Client) Connected() health.Monitor {
	return health.MonitorFunc(c.checkConnected)
}

// checkConnected is the check of every monitor Connected returns.
func (c *Client) checkConnected(ctx context.Context) (bool, error) {
	if c.connected.Load() {
		return c.cc.GetState() != connectivity.Shutdown, nil
	}
	for {
		state := c.cc.GetState()
		switch state {
		case connectivity.Ready:
			c.connected.Store(true)
			return true, nil
		case connectivity.Shutdown:
			return false, nil
		case connectivity.Idle:
			// Connect only starts the connection, and returns at once. The
			// state then changes from Idle, which WaitForStateChange sees.
			c.cc.Connect()
		}
		if !c.cc.WaitForStateChange(ctx, state) {
			return false, nil
		}
	}
}
