// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcauth_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/grpc/grpcauth"
	"github.com/z5labs/humus/humustest"
)

// recv returns the payload of the first message of a StreamingOutputCall
// made with ctx, or the error the stream ended with.
func recv(t *testing.T, c testpb.TestServiceClient, ctx context.Context) (string, error) {
	t.Helper()
	stream, err := c.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{})
	require.NoError(t, err)
	msg, err := stream.Recv()
	if err != nil {
		return "", err
	}
	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF)
	return string(msg.GetPayload().GetBody()), nil
}

func TestInterceptors_unary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		secrets, creds := newSecrets(user{name: "alice"})
		b := auth.Bearer("bearer", secrets.verify)
		a := serve(t, map[string]auth.Scheme[user]{"bearer": b}, []grpcauth.Rule{
			grpcauth.Default(auth.Require(b)),
		}, nil)
		c := a.tests()

		_, err := c.UnaryCall(t.Context(), &testpb.SimpleRequest{})
		requireStatus(t, err, codes.Unauthenticated, "unauthenticated")
		require.Zero(t, secrets.calls.Load(), "a call without credentials reached the verifier")

		resp, err := c.UnaryCall(bearer(t, creds["alice"]), &testpb.SimpleRequest{})
		require.NoError(t, err)
		require.Equal(t, "bearer:alice", resp.GetUsername(), "the principal in the handler's context")

		resp, err = c.UnaryCall(with(t, "authorization", "bearer "+creds["alice"]), &testpb.SimpleRequest{})
		require.NoError(t, err, "the scheme name matches whatever its case")
		require.Equal(t, "bearer:alice", resp.GetUsername())

		_, err = c.UnaryCall(bearer(t, "forged"), &testpb.SimpleRequest{})
		requireStatus(t, err, codes.Unauthenticated, "unauthenticated")

		_, err = c.UnaryCall(with(t, "authorization", "Bearer "+creds["alice"], "authorization", "Bearer "+creds["alice"]), &testpb.SimpleRequest{})
		requireStatus(t, err, codes.Unauthenticated, "unauthenticated")

		a.stop()
		require.Empty(t, records(a.tel), "a refusal isn't recorded")
	})
}

func TestInterceptors_streaming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		secrets, creds := newSecrets(user{name: "alice"}, user{name: "admin", scopes: []string{"carts:write"}})
		b := auth.Bearer("bearer", secrets.verify)
		a := serve(t, map[string]auth.Scheme[user]{"bearer": b}, []grpcauth.Rule{
			grpcauth.Default(auth.Require(b)),
			grpcauth.Method(testpb.TestService_StreamingOutputCall_FullMethodName, auth.RequireScopes(b, "carts:write")),
		}, nil)
		c := a.tests()

		_, err := recv(t, c, t.Context())
		requireStatus(t, err, codes.Unauthenticated, "unauthenticated")

		_, err = recv(t, c, bearer(t, creds["alice"]))
		requireStatus(t, err, codes.PermissionDenied, "forbidden")

		got, err := recv(t, c, bearer(t, creds["admin"]))
		require.NoError(t, err)
		require.Equal(t, "bearer:admin", got, "the principal in the stream's context")

		resp, err := c.UnaryCall(bearer(t, creds["alice"]), &testpb.SimpleRequest{})
		require.NoError(t, err, "the Default covers the methods no Method names")
		require.Equal(t, "bearer:alice", resp.GetUsername())

		a.stop()
		require.Empty(t, records(a.tel), "a refusal isn't recorded")
	})
}

func TestInterceptors_Disabled_opens_a_method(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		secrets, creds := newSecrets(user{name: "alice"})
		b := auth.Bearer("bearer", secrets.verify)
		a := serve(t, map[string]auth.Scheme[user]{"bearer": b}, []grpcauth.Rule{
			grpcauth.Default(auth.Require(b)),
			grpcauth.Method(testpb.TestService_UnaryCall_FullMethodName, auth.Disabled()),
		}, nil)
		c := a.tests()

		for _, ctx := range []context.Context{t.Context(), bearer(t, "forged"), bearer(t, creds["alice"])} {
			resp, err := c.UnaryCall(ctx, &testpb.SimpleRequest{})
			require.NoError(t, err)
			require.Equal(t, "none", resp.GetUsername(), "From returns false under Disabled")
		}
		require.Zero(t, secrets.calls.Load(), "Disabled reads no credential")

		_, err := c.EmptyCall(t.Context(), &testpb.Empty{})
		requireStatus(t, err, codes.Unauthenticated, "unauthenticated")
		a.stop()
	})
}

func TestInterceptors_public_unless_listed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		secrets, creds := newSecrets(user{name: "alice"})
		key := auth.APIKeyHeader("apiKey", "X-API-Key", secrets.verify)
		a := serve(t, map[string]auth.Scheme[user]{"apiKey": key}, []grpcauth.Rule{
			grpcauth.Default(auth.Disabled()),
			grpcauth.Method(testpb.TestService_UnaryCall_FullMethodName, auth.Require(key)),
		}, nil)
		c := a.tests()

		_, err := c.EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err, "a method no Method names is public")

		_, err = c.UnaryCall(t.Context(), &testpb.SimpleRequest{})
		requireStatus(t, err, codes.Unauthenticated, "unauthenticated")

		resp, err := c.UnaryCall(with(t, "x-api-key", creds["alice"]), &testpb.SimpleRequest{})
		require.NoError(t, err, "the API key is read from the metadata key, lowercased")
		require.Equal(t, "apiKey:alice", resp.GetUsername())
		a.stop()
	})
}

func TestInterceptors_a_verifiers_error(t *testing.T) {
	errDirectory := errors.New("the directory can't be reached")
	failing := func(err error) func(context.Context, string) (user, error) {
		return func(context.Context, string) (user, error) { return user{}, err }
	}

	t.Run("is Internal, recorded once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := auth.Bearer("directory", failing(errDirectory))
			a := serve(t, nil, []grpcauth.Rule{grpcauth.Default(auth.Require(b))}, nil)

			_, err := a.tests().UnaryCall(bearer(t, "token"), &testpb.SimpleRequest{})
			requireStatus(t, err, codes.Internal, "authentication failed")
			a.stop()

			var span []string
			for _, s := range serverSpans(a.tel) {
				if s.Name() == "grpc.testing.TestService/UnaryCall" {
					span = exceptionMessages(s)
					recs := records(a.tel)
					require.Len(t, recs, 1)
					rec := recs[0]
					require.Equal(t, "authentication failed", rec.Body().AsString())
					require.Equal(t, "ERROR", rec.SeverityText())
					require.Equal(t, s.SpanContext().TraceID(), rec.TraceID(), "the record is in the RPC's context")
					got := attrs(rec)
					require.Equal(t, "directory", got["humus.auth.scheme"])
					require.Equal(t, "*errors.errorString", got["error.type"])
					require.Equal(t, errDirectory.Error(), got["exception.message"])
					require.Equal(t, "grpc.testing.TestService/UnaryCall", got["rpc.method"])
					require.NotContains(t, got, "auth.scheme")
					require.Len(t, got, 4, "no principal, subject or key is recorded: %v", got)
				}
			}
			require.Equal(t, []string{errDirectory.Error()}, span, "one exception event on the RPC's span")
		})
	})

	t.Run("answers with its own gRPC status", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := auth.Bearer("directory", failing(status.Error(codes.Unavailable, "the directory is down")))
			a := serve(t, nil, []grpcauth.Rule{grpcauth.Default(auth.Require(b))}, nil)

			_, err := a.tests().UnaryCall(bearer(t, "token"), &testpb.SimpleRequest{})
			requireStatus(t, err, codes.Unavailable, "the directory is down")
			_, err = recv(t, a.tests(), bearer(t, "token"))
			requireStatus(t, err, codes.Unavailable, "the directory is down")
			a.stop()
			require.Len(t, records(a.tel), 2)
		})
	})

	t.Run("goes on the span alone when the client went away", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			entered := make(chan struct{})
			b := auth.Bearer("directory", func(ctx context.Context, _ string) (user, error) {
				close(entered)
				<-ctx.Done()
				return user{}, context.Cause(ctx)
			})
			a := serve(t, nil, []grpcauth.Rule{grpcauth.Default(auth.Require(b))}, nil)

			ctx, cancel := context.WithCancel(bearer(t, "token"))
			done := make(chan error, 1)
			go func() {
				_, err := a.tests().UnaryCall(ctx, &testpb.SimpleRequest{})
				done <- err
			}()
			<-entered
			cancel()
			require.Equal(t, codes.Canceled, status.Code(<-done))
			synctest.Wait()
			a.stop()

			require.Empty(t, records(a.tel))
			var exceptions int
			for _, s := range serverSpans(a.tel) {
				exceptions += len(exceptionMessages(s))
			}
			require.Equal(t, 1, exceptions)
		})
	})

	t.Run("passes a panic to grpcserver's recovery", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := auth.Bearer("directory", func(context.Context, string) (user, error) { panic("boom") })
			a := serve(t, nil, []grpcauth.Rule{grpcauth.Default(auth.Require(b))}, nil)

			_, err := a.tests().UnaryCall(bearer(t, "token"), &testpb.SimpleRequest{})
			requireStatus(t, err, codes.Internal, "internal error")
			_, err = a.tests().EmptyCall(t.Context(), &testpb.Empty{})
			requireStatus(t, err, codes.Unauthenticated, "unauthenticated")
			a.stop()
			require.Empty(t, records(a.tel), "grpcserver records the panic, not grpcauth")
		})
	})
}

func TestInterceptors_health_opened_by_name(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		secrets, _ := newSecrets(user{name: "alice"})
		b := auth.Bearer("bearer", secrets.verify)
		a := serve(t, nil, []grpcauth.Rule{
			grpcauth.Default(auth.Require(b)),
			grpcauth.Method(healthpb.Health_Check_FullMethodName, auth.Disabled()),
			grpcauth.Method(healthpb.Health_Watch_FullMethodName, auth.Disabled()),
			grpcauth.Method(healthpb.Health_List_FullMethodName, auth.Disabled()),
		}, nil)
		hc := healthpb.NewHealthClient(a.conn)

		resp, err := hc.Check(t.Context(), &healthpb.HealthCheckRequest{})
		require.NoError(t, err)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())

		list, err := hc.List(t.Context(), &healthpb.HealthListRequest{})
		require.NoError(t, err)
		require.NotEmpty(t, list.GetStatuses())

		watch, err := hc.Watch(t.Context(), &healthpb.HealthCheckRequest{})
		require.NoError(t, err)
		w, err := watch.Recv()
		require.NoError(t, err)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, w.GetStatus())

		_, err = a.tests().EmptyCall(t.Context(), &testpb.Empty{})
		requireStatus(t, err, codes.Unauthenticated, "unauthenticated")
		a.stop()
	})
}

func TestInterceptors_mutual_TLS(t *testing.T) {
	commonName := func(_ context.Context, chains [][]*x509.Certificate) (user, error) {
		return user{name: chains[0][0].Subject.CommonName}, nil
	}

	t.Run("authenticates from the peer's verified chains", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cert := auth.MutualTLS("cert", commonName)
			a := serve(t, map[string]auth.Scheme[user]{"cert": cert}, []grpcauth.Rule{
				grpcauth.Default(auth.Require(cert)),
			}, func(c *tls.Config, ca *humustest.CA) {
				c.ClientAuth = tls.VerifyClientCertIfGiven
				c.ClientCAs = ca.Pool()
			})

			resp, err := testpb.NewTestServiceClient(a.clientWith(a.ca.Issue(t, "alice"))).UnaryCall(t.Context(), &testpb.SimpleRequest{})
			require.NoError(t, err)
			require.Equal(t, "cert:alice", resp.GetUsername())

			_, err = a.tests().UnaryCall(t.Context(), &testpb.SimpleRequest{})
			requireStatus(t, err, codes.Unauthenticated, "unauthenticated")
			a.stop()
		})
	})

	t.Run("takes a requested but unverified certificate for no credential", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cert := auth.MutualTLS("cert", commonName)
			a := serve(t, map[string]auth.Scheme[user]{"cert": cert}, []grpcauth.Rule{
				grpcauth.Default(auth.Require(cert)),
			}, func(c *tls.Config, ca *humustest.CA) {
				c.ClientAuth = tls.RequestClientCert
				c.ClientCAs = ca.Pool()
			})

			_, err := testpb.NewTestServiceClient(a.clientWith(a.ca.Issue(t, "alice"))).UnaryCall(t.Context(), &testpb.SimpleRequest{})
			requireStatus(t, err, codes.Unauthenticated, "unauthenticated")
			a.stop()
		})
	})
}
