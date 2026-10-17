// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"reflect"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
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
		httpserver.Defaults()(&a)

		require.Equal(t, funcPointer(host.Listen), funcPointer(a.Host.Listen), "Host.Listen")
		require.Equal(t, funcPointer(host.Dial), funcPointer(a.Host.Dial), "Host.Dial")
		require.Equal(t, host.FS, a.Host.FS, "Host.FS")
		require.Equal(t, funcPointer(telemetry.NoopBuilder), funcPointer(a.Telemetry), "Telemetry")
		require.Equal(t, funcPointer(noServices), funcPointer(a.Init), "Init")

		require.Equal(t, 10*time.Second, a.ShutdownGracePeriod)
		require.NotNil(t, a.Drain)
	})

	t.Run("including an App with none of them set", func(t *testing.T) {
		var a humus.App
		httpserver.Defaults()(&a)
		require.Nil(t, a.Host.Listen)
		require.Nil(t, a.Host.Dial)
		require.Nil(t, a.Host.FS)
		require.Nil(t, a.Telemetry)
		require.Nil(t, a.Init)
	})
}

func TestDefaults_on_a_whole_App(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want time.Duration
	}{
		{"cancel a stuck request after the 5s drain and the 10s grace period", nil, 15 * time.Second},
		{"keep the grace period when HUMUS_SHUTDOWN_DRAIN_DELAY overrides the drain", map[string]string{"HUMUS_SHUTDOWN_DRAIN_DELAY": "1s"}, 11 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				started := make(chan struct{})
				cancelled := make(chan time.Time, 1)
				handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					close(started)
					<-r.Context().Done()
					cancelled <- time.Now()
				})
				// No WriteTimeout, which would otherwise cancel the request
				// 10s after it arrived: only the shutdown cancels it.
				settings := settingsFor(t, ca)
				settings.WriteTimeout = 0
				p, status := humustest.Start(t, serves(settings, handler),
					humustest.WithApp(httpserver.Defaults()),
					humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
					humustest.WithEnv(c.env),
				)
				<-h.Listening("localhost:8443")

				got := get(client(h, ca, offersH2), "https://localhost:8443/")
				<-started
				<-p.Notified(syscall.SIGTERM)
				sigterm := time.Now()
				p.Signal(syscall.SIGTERM)

				require.Equal(t, c.want, (<-cancelled).Sub(sigterm), "the request's context is cancelled after the drain and the grace period")
				require.Error(t, (<-got).err, "the request's connection is closed")

				st := <-status
				require.Equal(t, humus.ExitFailure, st.Code())
				gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
				require.True(t, ok, "cause = %v, want a *humus.GracePeriodError", st.Cause())
				require.Equal(t, 10*time.Second, gerr.GracePeriod)
			})
		})
	}
}

// pemFile encodes c, its chain and its private key as one PEM file.
func pemFile(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var b []byte
	for _, der := range cert.Certificate {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(b, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}

func TestInit_serves_over_TLS_with_the_defaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"tls.pem": pemFile(t, ca.Issue(t, "localhost")),
		}))
		p, status := humustest.Start(t, Init,
			humustest.WithApp(httpserver.Defaults()),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{"ADDR": ":8443", "TLS_PEM_FILE": "tls.pem"}),
		)
		<-h.Listening("localhost:8443")

		r := <-get(client(h, ca, offersH2), "https://localhost:8443/hello")
		require.NoError(t, r.err)
		require.Equal(t, http.StatusOK, r.code)
		require.Equal(t, 2, r.proto)
		require.Equal(t, "hello", r.body)

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, 5*time.Second, time.Since(sigterm), "the App exits after the drain")
	})

	t.Run("fails with exit 78 when TLS_PEM_FILE is not set", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			_, status := humustest.Start(t, Init,
				humustest.WithApp(httpserver.Defaults()),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
				humustest.WithEnv(map[string]string{"ADDR": ":8443"}),
			)
			st := <-status
			require.Equal(t, 78, int(st.Code()), "cause: %v", st.Cause())
			require.ErrorContains(t, st.Cause(), "TLS_PEM_FILE")
		})
	})
}
