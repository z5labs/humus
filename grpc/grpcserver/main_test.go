// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver_test

import (
	"context"
	"reflect"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// funcPointer returns the code pointer of f, a func value, so that two
// references to the same function compare equal.
func funcPointer(f any) uintptr {
	return reflect.ValueOf(f).Pointer()
}

// noServices is an Init that returns no Services.
func noServices(context.Context, *humus.Setup) ([]humus.Service, error) { return nil, nil }

func TestDefaults_leaves_the_rest_of_the_App_unchanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := humustest.NewHost(t).Host()
		a := humus.App{
			Host:      host,
			Telemetry: telemetry.NoopBuilder,
			Init:      noServices,
		}
		grpcserver.Defaults()(&a)

		require.Equal(t, funcPointer(host.Listen), funcPointer(a.Host.Listen), "Host.Listen")
		require.Equal(t, funcPointer(host.Dial), funcPointer(a.Host.Dial), "Host.Dial")
		require.Equal(t, host.FS, a.Host.FS, "Host.FS")
		require.Equal(t, funcPointer(telemetry.NoopBuilder), funcPointer(a.Telemetry), "Telemetry")
		require.Equal(t, funcPointer(noServices), funcPointer(a.Init), "Init")

		require.Equal(t, 15*time.Second, a.ShutdownGracePeriod)
		require.NotNil(t, a.Drain)
	})

	t.Run("including an App with none of them set", func(t *testing.T) {
		var a humus.App
		grpcserver.Defaults()(&a)
		require.Nil(t, a.Host.Listen)
		require.Nil(t, a.Host.Dial)
		require.Nil(t, a.Host.FS)
		require.Nil(t, a.Telemetry)
		require.Nil(t, a.Init)
	})
}

func TestMain_an_RPC_in_flight_finishes_during_shutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		ts := &testServer{entered: make(chan struct{}), release: make(chan struct{})}

		p, exit := grpcservertest.Start(t, serves(settingsFor(t, ca, ":9090"), ts),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening("carts:9090")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		got := call(t, client)
		<-ts.entered // the handler is running

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		time.Sleep(7 * time.Second) // past the 5s drain: Shutdown is waiting for the RPC
		close(ts.release)

		require.Equal(t, "done", <-got)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, 7*time.Second, time.Since(sigterm))
		requireRefused(t, h, "carts:9090")

		spans := serverSpans(tel)
		require.Len(t, spans, 1)
		require.Equal(t, "grpc.testing.TestService/UnaryCall", spans[0].Name())
	})
}

func TestInit_without_an_address_or_a_certificate_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, exit := grpcservertest.Start(t, Init)

		st := <-exit
		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorContains(t, st.Cause(), "config GRPC_ADDR: not found")
		require.ErrorContains(t, st.Cause(), "config GRPC_TLS_P12_FILE: not found")
	})
}

// p12Env returns a test Host holding a PKCS#12 bundle with a certificate
// for carts from ca, as tls.p12, and the environment that names it and its
// password, with addr as GRPC_ADDR.
func p12Env(t *testing.T, ca *humustest.CA, addr string) (*humustest.Host, map[string]string) {
	t.Helper()
	bundle, password := ca.Issue(t, "carts").PKCS12(t)
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{"tls.p12": bundle}))
	return h, map[string]string{
		"GRPC_ADDR":             addr,
		"GRPC_TLS_P12_FILE":     "tls.p12",
		"GRPC_TLS_P12_PASSWORD": password,
	}
}

func TestInit_with_a_bad_address_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, env := p12Env(t, humustest.NewCA(t), "nope")

		_, exit := grpcservertest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(env),
		)

		st := <-exit
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		require.ErrorContains(t, st.Cause(), "config GRPC_ADDR:")
		require.ErrorContains(t, st.Cause(), `"nope" is not host:port`)
		require.NotContains(t, st.Cause().Error(), "GRPC_TLS_P12")
	})
}

func TestInit_serves_with_a_certificate_from_a_PKCS12_file(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h, env := p12Env(t, ca, ":9090")

		p, exit := grpcservertest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(env),
		)
		<-h.Listening("carts:9090")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		resp, err := client.UnaryCall(t.Context(), &testpb.SimpleRequest{})
		require.NoError(t, err)
		require.Equal(t, "greeter", resp.GetUsername())

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, 5*time.Second, time.Since(sigterm), "the App exits after the drain")
	})

	t.Run("fails with exit 78 when the password is wrong", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h, env := p12Env(t, humustest.NewCA(t), ":9090")
			env["GRPC_TLS_P12_PASSWORD"] += "x"

			_, exit := grpcservertest.Start(t, Init,
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
				humustest.WithEnv(env),
			)
			st := <-exit
			require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
			require.ErrorContains(t, st.Cause(), "config GRPC_TLS_P12_PASSWORD:")
			require.NotContains(t, st.Cause().Error(), env["GRPC_TLS_P12_PASSWORD"], "the password stays out of the exit cause")
		})
	})
}
