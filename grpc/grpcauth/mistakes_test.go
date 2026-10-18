// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcauth_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/grpc/grpcauth"
	"github.com/z5labs/humus/telemetry"
)

// verifyToken is the verify function of the declarations below, which is
// never called.
func verifyToken(context.Context, string) (user, error) { return user{}, nil }

// setup is a Setup whose telemetry discards everything.
func setup() *humus.Setup { return &humus.Setup{Telemetry: telemetry.Noop()} }

func TestInterceptors_reports_each_mistake(t *testing.T) {
	b := auth.Bearer("bearer", verifyToken)
	unary := testpb.TestService_UnaryCall_FullMethodName

	for _, tc := range []struct {
		name  string
		s     *humus.Setup
		rules []grpcauth.Rule
		want  string
	}{
		{name: "a nil s", rules: []grpcauth.Rule{grpcauth.Default(auth.Require(b))}, want: "s is nil"},
		{name: "no Default", rules: []grpcauth.Rule{grpcauth.Method(unary, auth.Require(b))}, want: "no Default"},
		{
			name:  "two Defaults",
			rules: []grpcauth.Rule{grpcauth.Default(auth.Require(b)), grpcauth.Default(auth.Disabled())},
			want:  "rules[1]: Default is a second Default: give only one",
		},
		{name: "the zero Rule", rules: []grpcauth.Rule{grpcauth.Default(auth.Require(b)), {}}, want: "rules[1] is the zero Rule"},
		{
			name:  "a method given twice",
			rules: []grpcauth.Rule{grpcauth.Default(auth.Require(b)), grpcauth.Method(unary, auth.Require(b)), grpcauth.Method(unary, auth.Disabled())},
			want:  `rules[2]: Method("/grpc.testing.TestService/UnaryCall") gives a method that an earlier rule gives`,
		},
		{
			name:  "Default with no requirement",
			rules: []grpcauth.Rule{grpcauth.Default()},
			want:  "rules[0]: Default has no requirement: give it auth.Disabled() to make it public",
		},
		{
			name:  "Method with no requirement",
			rules: []grpcauth.Rule{grpcauth.Default(auth.Require(b)), grpcauth.Method(unary)},
			want:  `rules[1]: Method("/grpc.testing.TestService/UnaryCall") has no requirement`,
		},
		{
			name:  "a query API key",
			rules: []grpcauth.Rule{grpcauth.Default(auth.Require(auth.APIKeyQuery("key", "key", verifyToken)))},
			want:  `rules[0]: Default: the APIKeyQuery scheme "key" reads a query parameter, which an RPC doesn't have`,
		},
		{
			name:  "a cookie API key",
			rules: []grpcauth.Rule{grpcauth.Default(auth.Require(b)), grpcauth.Method(unary, auth.Require(auth.APIKeyCookie("key", "key", verifyToken)))},
			want:  `the APIKeyCookie scheme "key" reads a cookie, which an RPC doesn't have`,
		},
		{
			name:  "Disabled beside another requirement",
			rules: []grpcauth.Rule{grpcauth.Default(auth.Require(b), auth.Disabled())},
			want:  "rules[0]: Default: reqs[1] is Disabled, which stands alone",
		},
		{
			name:  "Disabled in an All",
			rules: []grpcauth.Rule{grpcauth.Default(auth.All(auth.Require(b), auth.Disabled()))},
			want:  "never inside an All",
		},
		{
			name:  "the zero Requirement",
			rules: []grpcauth.Rule{grpcauth.Default(auth.Requirement{})},
			want:  "rules[0]: Default: reqs[0] is the zero Requirement",
		},
		{
			name:  "a nil verify function",
			rules: []grpcauth.Rule{grpcauth.Default(auth.Require(auth.Bearer[user]("bearer", nil)))},
			want:  `the Bearer scheme "bearer" has a nil verify function`,
		},
		{
			name: "two different schemes with one name, in two rules",
			rules: []grpcauth.Rule{
				grpcauth.Default(auth.Require(b)),
				grpcauth.Method(unary, auth.Require(auth.Bearer("bearer", verifyToken))),
			},
			want: `two different schemes are named "bearer"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.s == nil && tc.name != "a nil s" {
				tc.s = setup()
			}
			unary, stream, err := grpcauth.Interceptors(tc.s, tc.rules...)
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "grpcauth: Interceptors: ")
			require.Nil(t, unary)
			require.Nil(t, stream)
		})
	}
}

func TestInterceptors_rejects_a_name_that_isnt_a_full_method_name(t *testing.T) {
	b := auth.Bearer("bearer", verifyToken)
	for _, name := range []string{
		"",
		"grpc.testing.TestService/UnaryCall",
		"/grpc.testing.TestService",
		"/grpc.testing.TestService/",
		"/TestService/UnaryCall",
		"/grpc.testing.TestService/Unary/Call",
		"/grpc..TestService/UnaryCall",
		"/grpc.testing.TestService/1Call",
		"/grpc.testing.Test Service/UnaryCall",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := grpcauth.Interceptors(setup(), grpcauth.Default(auth.Require(b)), grpcauth.Method(name, auth.Require(b)))
			require.ErrorContains(t, err, "names no method: a method's full name is /package.Service/Method")
		})
	}
}

func TestInterceptors_reports_every_mistake_in_one_error(t *testing.T) {
	b := auth.Bearer("bearer", verifyToken)
	unary := testpb.TestService_UnaryCall_FullMethodName
	_, _, err := grpcauth.Interceptors(setup(),
		grpcauth.Method(unary, auth.Require(b)),
		grpcauth.Method(unary, auth.Require(b)),
		grpcauth.Method("UnaryCall", auth.Require(b)),
		grpcauth.Method(testpb.TestService_EmptyCall_FullMethodName),
		grpcauth.Method(testpb.TestService_StreamingOutputCall_FullMethodName, auth.Require(auth.APIKeyQuery("key", "key", verifyToken))),
	)
	require.Error(t, err)
	for _, want := range []string{
		"no Default",
		`rules[1]: Method("/grpc.testing.TestService/UnaryCall") gives a method that an earlier rule gives`,
		`rules[2]: Method("UnaryCall") names no method`,
		`rules[3]: Method("/grpc.testing.TestService/EmptyCall") has no requirement`,
		`rules[4]: Method("/grpc.testing.TestService/StreamingOutputCall"): the APIKeyQuery scheme "key" reads a query parameter`,
	} {
		require.ErrorContains(t, err, want)
	}
}
