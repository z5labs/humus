// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host/hosttest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/http3/http3server/http3servertest"
	"github.com/z5labs/humus/http3/internal/quicdial"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts,
// with certificates from a CA generated for each test. Whole Apps run with
// humustest.Start or http3servertest.Start, and HTTP/3 clients come from
// http3servertest.NewClient. A test that needs to see QUIC itself, a
// connection that sends no request or a stream that sends no headers, dials
// a bare QUIC connection through quicdial.
//
// humustest's Host.Listening waits for TCP listeners only, so a test waits
// for the Service's socket with synctest.Wait.

// scope is the instrumentation scope of the Service's logger.
const scope = "github.com/z5labs/humus/http3/http3server"

// settingsFor returns DefaultSettings on :8443, with a certificate for
// localhost from ca.
func settingsFor(t *testing.T, ca *humustest.CA) http3server.Settings {
	t.Helper()
	settings := http3server.DefaultSettings()
	settings.Addr = ":8443"
	settings.TLS = &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}
	return settings
}

// serves returns an Init whose only Service serves handler on settings.
func serves(settings http3server.Settings, handler http.Handler) func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error) {
	return func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
		svc, err := http3server.NewService(ctx, s, h, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
}

// response is what a client got back from a request.
type response struct {
	proto string
	code  int
	body  string
	err   error
}

// do sends req with c in a new goroutine, and returns the channel its
// response arrives on, once its body has been read.
func do(c *http.Client, req *http.Request) <-chan response {
	got := make(chan response, 1)
	go func() {
		resp, err := c.Do(req)
		if err != nil {
			got <- response{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- response{proto: resp.Proto, code: resp.StatusCode, body: string(b), err: err}
	}()
	return got
}

// get sends a GET to url with c, as do does.
func get(t *testing.T, c *http.Client, url string) <-chan response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	return do(c, req)
}

// hello answers every request with "hello".
var hello = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "hello")
})

// dialQUIC dials a bare QUIC connection to localhost:8443 through h,
// trusting only ca and offering h3, which never sends a request unless the
// test opens a stream on it. It is closed when t ends.
func dialQUIC(t *testing.T, h *humustest.Host, ca *humustest.CA) (*quic.Conn, error) {
	t.Helper()
	d := quicdial.New(h.DialUDP)
	t.Cleanup(d.Close)
	return d.Dial(t.Context(), "localhost:8443", &tls.Config{RootCAs: ca.Pool(), ServerName: "localhost", NextProtos: []string{http3.NextProtoH3}}, nil)
}

// records returns the records of tel emitted by the Service's logger.
func records(tel *humustest.Telemetry) []sdklog.Record {
	var rs []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == scope {
			rs = append(rs, r)
		}
	}
	return rs
}

// attr returns the value of r's attribute key, and whether it has one.
func attr(r sdklog.Record, key string) (attribute.Value, bool) {
	var v attribute.Value
	found := false
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == key {
			v, found = kv.Value, true
			return false
		}
		return true
	})
	return v, found
}

// requireStreamError fails t unless err is a stream reset by the server
// with code.
func requireStreamError(t *testing.T, err error, code http3.ErrCode) {
	t.Helper()
	var serr *quic.StreamError
	require.ErrorAs(t, err, &serr)
	require.True(t, serr.Remote, "the server reset the stream")
	require.Equal(t, quic.StreamErrorCode(code), serr.ErrorCode, "reset with %v, want %v", http3.ErrCode(serr.ErrorCode), code)
}

func TestService_meets_the_Service_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		svc, err := http3server.NewService(t.Context(), s, humustest.NewHost(t), settingsFor(t, humustest.NewCA(t)), http.NotFoundHandler())
		require.NoError(t, err)
		return svc
	})
}

func TestService_serves_HTTP3(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, h.Host(), serves(settingsFor(t, ca), hello),
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
		)
		synctest.Wait()

		r := <-get(t, http3servertest.NewClient(t, h, ca.Pool()), "https://localhost:8443/")
		require.NoError(t, r.err)
		require.Equal(t, "HTTP/3.0", r.proto)
		require.Equal(t, "hello", r.body)

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())

		recs := records(tel)
		require.NotEmpty(t, recs)
		require.Equal(t, "listening", recs[0].Body().AsString())
		require.Equal(t, log.SeverityInfo, recs[0].Severity())
		addr, ok := attr(recs[0], "server.address")
		require.True(t, ok)
		require.Equal(t, ":8443", addr.AsString())
	})
}

func TestNewService(t *testing.T) {
	t.Run("names every bad field in one error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			s := &humus.Setup{Telemetry: telemetry.Noop()}
			settings := http3server.Settings{
				Addr:              "nowhere",
				TLS:               &tls.Config{MaxVersion: tls.VersionTLS12},
				ReadHeaderTimeout: -1,
				ReadTimeout:       -1,
				WriteTimeout:      -1,
				IdleTimeout:       -1,
				MaxHeaderBytes:    0,
			}
			svc, err := http3server.NewService(t.Context(), s, h, settings, nil)
			require.Nil(t, svc)
			require.Error(t, err)
			for _, field := range []string{
				"Settings.Addr", "Settings.TLS", "Settings.ReadHeaderTimeout", "Settings.ReadTimeout",
				"Settings.WriteTimeout", "Settings.IdleTimeout", "Settings.MaxHeaderBytes", "the handler must not be nil",
			} {
				require.ErrorContains(t, err, field)
			}
		})
	})

	t.Run("binds through its Network, with its ctx", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			type ctxKey struct{}
			var binds []string
			var tags []any
			n := hosttest.Seams{Network: h, OnListenUDP: func(ctx context.Context, network, address string) (net.PacketConn, error) {
				binds = append(binds, network+" "+address)
				tags = append(tags, ctx.Value(ctxKey{}))
				return h.ListenUDP(ctx, network, address)
			}}
			s := &humus.Setup{Telemetry: telemetry.Noop()}
			ctx := context.WithValue(t.Context(), ctxKey{}, "NewService's ctx")

			_, err := http3server.NewService(ctx, s, n, settingsFor(t, ca), hello)
			require.NoError(t, err)
			require.Equal(t, []string{"udp :8443"}, binds, "NewService binds once, through n")
			require.Equal(t, []any{"NewService's ctx"}, tags)
		})
	})

	t.Run("fails when the Network does", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			errBind := errors.New("no UDP here")
			n := hosttest.Seams{Network: h, OnListenUDP: func(context.Context, string, string) (net.PacketConn, error) {
				return nil, errBind
			}}
			_, err := http3server.NewService(t.Context(), &humus.Setup{Telemetry: telemetry.Noop()}, n, settingsFor(t, humustest.NewCA(t)), hello)
			require.ErrorIs(t, err, errBind)
		})
	})
}

func TestService_Addr_reports_the_port_the_Network_picked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := settingsFor(t, humustest.NewCA(t))
		settings.Addr = "localhost:0"
		svc, err := http3server.NewService(t.Context(), &humus.Setup{Telemetry: telemetry.Noop()}, humustest.NewHost(t), settings, hello)
		require.NoError(t, err)
		require.Equal(t, "udp", svc.Addr().Network())
		host, port, err := net.SplitHostPort(svc.Addr().String())
		require.NoError(t, err)
		require.Equal(t, "localhost", host)
		require.NotEqual(t, "0", port, "the Network picked a port")
	})
}

func TestService_binds_UDP_apart_from_TCP(t *testing.T) {
	t.Run("a taken UDP port fails Init while TCP on it is free", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			taken, err := h.ListenUDP(t.Context(), "udp", ":8443")
			require.NoError(t, err)
			defer func() { _ = taken.Close() }()

			_, status := humustest.Start(t, h.Host(), serves(settingsFor(t, ca), hello),
				humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			st := <-status
			require.Equal(t, humus.ExitFailure, st.Code())
			require.ErrorIs(t, st.Cause(), syscall.EADDRINUSE)

			l, err := h.ListenTCP(t.Context(), "tcp", ":8443")
			require.NoError(t, err, "TCP's ports are apart from UDP's")
			require.NoError(t, l.Close())
		})
	})

	t.Run("an Init that fails after NewService closes the socket", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			errInit := errors.New("the database is unreachable")
			settings := settingsFor(t, ca)
			init := func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
				if _, err := http3server.NewService(ctx, s, h, settings, hello); err != nil {
					return nil, err
				}
				return nil, errInit
			}
			_, status := humustest.Start(t, h.Host(), init, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))
			st := <-status
			require.ErrorIs(t, st.Cause(), errInit)

			sock, err := h.ListenUDP(t.Context(), "udp", ":8443")
			require.NoError(t, err, "the socket was closed")
			require.NoError(t, sock.Close())
		})
	})
}

func TestService_Shutdown(t *testing.T) {
	t.Run("a 4 MiB response in flight is read whole", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			body := strings.Repeat("x", 4<<20)
			started := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(started)
				// The drain is 5s, so this writes 2s into Shutdown.
				time.Sleep(7 * time.Second)
				_, _ = io.WriteString(w, body)
			})
			p, status := http3servertest.Start(t, h.Host(), serves(settingsFor(t, ca), handler),
				humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			synctest.Wait()

			got := get(t, http3servertest.NewClient(t, h, ca.Pool()), "https://localhost:8443/")
			<-started
			p.Signal(syscall.SIGTERM)

			r := <-got
			require.NoError(t, r.err)
			require.Equal(t, len(body), len(r.body), "the whole body arrived")
			require.Equal(t, body, r.body)

			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
			require.Empty(t, p.Stderr())
		})
	})

	t.Run("an idle connection is closed a second into it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			p, status := http3servertest.Start(t, h.Host(), serves(settingsFor(t, ca), hello),
				humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			synctest.Wait()

			// A bare QUIC connection never closes on its own after the
			// GOAWAY, as no client of quic-go's ever has to.
			conn, err := dialQUIC(t, h, ca)
			require.NoError(t, err)

			<-p.Notified(syscall.SIGTERM)
			signalled := time.Now()
			p.Signal(syscall.SIGTERM)
			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
			require.Equal(t, 6*time.Second, time.Since(signalled), "the 5s drain, then the 1s close")

			<-conn.Context().Done()
			var aerr *quic.ApplicationError
			require.ErrorAs(t, context.Cause(conn.Context()), &aerr)
			require.True(t, aerr.Remote)
			require.Equal(t, quic.ApplicationErrorCode(http3.ErrCodeNoError), aerr.ErrorCode)
		})
	})

	t.Run("a handler blocked on its context sees it cancelled at the grace period", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			cancelled := make(chan time.Duration, 1)
			var signalled time.Time
			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
				cancelled <- time.Since(signalled)
			})
			// WriteTimeout would cancel the context first.
			settings := settingsFor(t, ca)
			settings.WriteTimeout = 0
			p, status := http3servertest.Start(t, h.Host(), serves(settings, handler),
				humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			synctest.Wait()

			got := get(t, http3servertest.NewClient(t, h, ca.Pool()), "https://localhost:8443/")
			synctest.Wait()
			signalled = time.Now()
			p.Signal(syscall.SIGTERM)

			st := <-status
			require.Equal(t, humus.ExitFailure, st.Code())
			gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
			require.True(t, ok, "cause = %v, want a *humus.GracePeriodError", st.Cause())
			require.NotEmpty(t, gerr.Error())
			require.Equal(t, 15*time.Second, <-cancelled, "the 5s drain, then the 10s grace period")
			require.Error(t, (<-got).err)
		})
	})

	t.Run("a stream opened after the GOAWAY is rejected", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			served := make(chan struct{}, 1)
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served <- struct{}{} })
			p, status := humustest.Start(t, h.Host(), serves(settingsFor(t, ca), handler),
				humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			synctest.Wait()
			conn, err := dialQUIC(t, h, ca)
			require.NoError(t, err)

			p.Signal(syscall.SIGTERM)
			synctest.Wait()

			str, err := conn.OpenStreamSync(t.Context())
			require.NoError(t, err)
			_, err = str.Write([]byte{0x01})
			require.NoError(t, err)
			_, err = io.ReadAll(str)
			requireStreamError(t, err, http3.ErrCodeRequestRejected)
			require.Empty(t, served, "no handler ran")

			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
		})
	})

	t.Run("a connection opened after it began gets no answer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			release := make(chan struct{})
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release })
			p, status := humustest.Start(t, h.Host(), serves(settingsFor(t, ca), handler),
				humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			synctest.Wait()

			// A request in flight keeps Shutdown waiting.
			got := get(t, http3servertest.NewClient(t, h, ca.Pool()), "https://localhost:8443/")
			synctest.Wait()
			p.Signal(syscall.SIGTERM)
			synctest.Wait()

			dialled := time.Now()
			_, err := dialQUIC(t, h, ca)
			var terr *quic.HandshakeTimeoutError
			var ierr *quic.IdleTimeoutError
			require.True(t, errors.As(err, &terr) || errors.As(err, &ierr), "err = %v, want the handshake to time out", err)
			require.Equal(t, 5*time.Second, time.Since(dialled), "the client waited out its handshake timeout")

			close(release)
			require.NoError(t, (<-got).err)
			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
		})
	})
}

func TestService_logs_quic_gos_errors_under_its_scope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "abc")
			_, _ = io.WriteString(w, "hello")
		})
		p, status := humustest.Start(t, h.Host(), serves(settingsFor(t, ca), handler),
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
		)
		synctest.Wait()

		r := <-get(t, http3servertest.NewClient(t, h, ca.Pool()), "https://localhost:8443/")
		require.NoError(t, r.err)
		require.Equal(t, "hello", r.body)

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())

		var errs []sdklog.Record
		for _, rec := range records(tel) {
			if rec.Severity() == log.SeverityError {
				errs = append(errs, rec)
			}
		}
		require.Len(t, errs, 1)
		require.Equal(t, "Malformed Content-Length", errs[0].Body().AsString())
		v, ok := attr(errs[0], "value")
		require.True(t, ok)
		require.Equal(t, "abc", v.AsString())
	})
}

func TestService_applies_its_Settings(t *testing.T) {
	t.Run("MaxHeaderBytes bounds a request's headers", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.MaxHeaderBytes = 1 << 10
			p, status := humustest.Start(t, h.Host(), serves(settings, hello),
				humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			synctest.Wait()
			c := http3servertest.NewClient(t, h, ca.Pool())

			small, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443/", nil)
			require.NoError(t, err)
			small.Header.Set("X-Padding", strings.Repeat("x", 512))
			r := <-do(c, small)
			require.NoError(t, r.err)
			require.Equal(t, http.StatusOK, r.code)

			large, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443/", nil)
			require.NoError(t, err)
			large.Header.Set("X-Padding", strings.Repeat("x", 2<<10))
			r = <-do(c, large)
			require.NoError(t, r.err)
			require.Equal(t, http.StatusRequestHeaderFieldsTooLarge, r.code)

			p.Signal(syscall.SIGTERM)
			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
		})
	})

	for _, tc := range []struct {
		name    string
		header  time.Duration
		read    time.Duration
		timeout time.Duration
	}{
		{"ReadHeaderTimeout bounds a request's headers", 2 * time.Second, 5 * time.Second, 2 * time.Second},
		{"ReadTimeout bounds them when ReadHeaderTimeout is zero", 0, 3 * time.Second, 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				settings := settingsFor(t, ca)
				settings.ReadHeaderTimeout = tc.header
				settings.ReadTimeout = tc.read
				served := make(chan struct{}, 1)
				handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served <- struct{}{} })
				p, status := humustest.Start(t, h.Host(), serves(settings, handler),
					humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
				)
				synctest.Wait()
				conn, err := dialQUIC(t, h, ca)
				require.NoError(t, err)

				// The stream sends one byte of a HEADERS frame, and no more.
				str, err := conn.OpenStreamSync(t.Context())
				require.NoError(t, err)
				opened := time.Now()
				_, err = str.Write([]byte{0x01})
				require.NoError(t, err)
				_, err = io.ReadAll(str)
				requireStreamError(t, err, http3.ErrCodeRequestIncomplete)
				require.Equal(t, tc.timeout, time.Since(opened))
				require.Empty(t, served, "no handler ran")

				p.Signal(syscall.SIGTERM)
				st := <-status
				require.Zero(t, st.Code(), "cause: %v", st.Cause())
			})
		})
	}

	for _, tc := range []struct {
		name    string
		idle    time.Duration
		read    time.Duration
		timeout time.Duration
	}{
		{"IdleTimeout closes an idle connection", 4 * time.Second, 5 * time.Second, 4 * time.Second},
		{"ReadTimeout closes it when IdleTimeout is zero", 0, 3 * time.Second, 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				settings := settingsFor(t, ca)
				settings.IdleTimeout = tc.idle
				settings.ReadTimeout = tc.read
				p, status := humustest.Start(t, h.Host(), serves(settings, hello),
					humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
				)
				synctest.Wait()

				dialled := time.Now()
				conn, err := dialQUIC(t, h, ca)
				require.NoError(t, err)
				<-conn.Context().Done()
				require.Equal(t, tc.timeout, time.Since(dialled))
				var aerr *quic.ApplicationError
				require.ErrorAs(t, context.Cause(conn.Context()), &aerr)
				require.True(t, aerr.Remote, "the server closed the connection")
				require.Equal(t, quic.ApplicationErrorCode(http3.ErrCodeNoError), aerr.ErrorCode)

				p.Signal(syscall.SIGTERM)
				st := <-status
				require.Zero(t, st.Code(), "cause: %v", st.Cause())
			})
		})
	}
}

func TestService_never_accepts_0RTT(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, status := humustest.Start(t, h.Host(), serves(settingsFor(t, ca), hello),
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		synctest.Wait()

		used := used0RTT(t, h, ca)
		require.False(t, used, "the second connection's 0-RTT was accepted")

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})

	// The same client against a quic-go server that allows 0-RTT has it
	// accepted, so the test above can tell.
	t.Run("and the test can tell", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			sock, err := h.ListenUDP(t.Context(), "udp", ":8443")
			require.NoError(t, err)
			srv := &http3.Server{
				Handler:    hello,
				TLSConfig:  &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}},
				QUICConfig: &quic.Config{Allow0RTT: true},
			}
			served := make(chan error, 1)
			go func() { served <- srv.Serve(sock) }()
			synctest.Wait()

			require.True(t, used0RTT(t, h, ca))

			require.NoError(t, srv.Close())
			require.ErrorIs(t, <-served, http.ErrServerClosed)
			require.NoError(t, sock.Close())
		})
	})
}

// used0RTT dials localhost:8443 twice through h, with one session cache,
// the second time early, and reports whether the second connection's
// 0-RTT was accepted.
func used0RTT(t *testing.T, h *humustest.Host, ca *humustest.CA) bool {
	t.Helper()
	d := quicdial.New(h.DialUDP)
	defer d.Close()
	cfg := &tls.Config{
		RootCAs:            ca.Pool(),
		ServerName:         "localhost",
		NextProtos:         []string{http3.NextProtoH3},
		ClientSessionCache: tls.NewLRUClientSessionCache(1),
	}

	first, err := d.Dial(t.Context(), "localhost:8443", cfg, nil)
	require.NoError(t, err)
	// The server sends its session ticket once the handshake is done.
	synctest.Wait()
	require.NoError(t, first.CloseWithError(0, ""))

	second, err := d.DialEarly(t.Context(), "localhost:8443", cfg, nil)
	require.NoError(t, err)
	<-second.HandshakeComplete()
	state := second.ConnectionState()
	require.True(t, state.TLS.DidResume, "the second connection resumed the first's session")
	return state.Used0RTT
}

func TestService_records_a_panic_and_resets_its_stream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
		p, status := humustest.Start(t, h.Host(), serves(settingsFor(t, ca), handler),
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
		)
		synctest.Wait()

		r := <-get(t, http3servertest.NewClient(t, h, ca.Pool()), "https://localhost:8443/")
		var h3err *http3.Error
		require.ErrorAs(t, r.err, &h3err)
		require.Equal(t, http3.ErrCodeInternalError, h3err.ErrorCode)
		require.True(t, h3err.Remote)

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())

		spans := tel.Spans()
		require.Len(t, spans, 1)
		var exceptions int
		for _, ev := range spans[0].Events() {
			if ev.Name == "exception" {
				exceptions++
			}
		}
		require.Equal(t, 1, exceptions)

		var panics []string
		for _, rec := range records(tel) {
			if rec.Severity() == log.SeverityError {
				panics = append(panics, rec.Body().AsString())
			}
		}
		require.Equal(t, []string{"handler panicked"}, panics, "recorded once, under the Service's scope, and not by quic-go")
	})
}

func TestService_Shutdown_closes_every_connection_before_it_returns(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func(t *testing.T) context.Context
	}{
		{"a ctx already done", func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		}},
		{"a ctx done during the drain", func(t *testing.T) context.Context {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			t.Cleanup(cancel)
			return ctx
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				reqCtx := make(chan context.Context, 1)
				handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					reqCtx <- r.Context()
					<-r.Context().Done()
				})
				// The Setup's OnStop functions never run here, so the test
				// closes the socket the Service bound.
				var sock net.PacketConn
				n := hosttest.Seams{Network: h, OnListenUDP: func(ctx context.Context, network, address string) (net.PacketConn, error) {
					pc, err := h.ListenUDP(ctx, network, address)
					sock = pc
					return pc, err
				}}
				svc, err := http3server.NewService(t.Context(), &humus.Setup{Telemetry: telemetry.Noop()}, n, settingsFor(t, ca), handler)
				require.NoError(t, err)
				served := make(chan error, 1)
				go func() { served <- svc.Serve(t.Context()) }()
				synctest.Wait()

				got := get(t, http3servertest.NewClient(t, h, ca.Pool()), "https://localhost:8443/")
				inFlight := <-reqCtx

				ctx := tc.ctx(t)
				err = svc.Shutdown(ctx)
				require.ErrorIs(t, err, ctx.Err())
				require.Error(t, inFlight.Err(), "the in-flight request's context is cancelled by the time Shutdown returns")
				require.NoError(t, svc.Shutdown(t.Context()), "a second call returns nil")

				require.Error(t, (<-got).err)
				require.NoError(t, <-served)
				synctest.Wait()
				require.NoError(t, sock.Close())
			})
		})
	}
}

// buffered is a socket that records the buffer sizes it is set to.
type buffered struct {
	net.PacketConn
	read, write chan int
}

func (b *buffered) SetReadBuffer(n int) error  { b.read <- n; return nil }
func (b *buffered) SetWriteBuffer(n int) error { b.write <- n; return nil }

func TestService_sets_its_sockets_buffers_so_quic_go_doesnt_warn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		sock := &buffered{read: make(chan int, 1), write: make(chan int, 1)}
		n := hosttest.Seams{Network: h, OnListenUDP: func(ctx context.Context, network, address string) (net.PacketConn, error) {
			pc, err := h.ListenUDP(ctx, network, address)
			sock.PacketConn = pc
			return sock, err
		}}
		svc, err := http3server.NewService(t.Context(), &humus.Setup{Telemetry: telemetry.Noop()}, n, settingsFor(t, ca), hello)
		require.NoError(t, err)
		served := make(chan error, 1)
		go func() { served <- svc.Serve(t.Context()) }()

		// quic-go writes its warning when it can't set either buffer.
		require.Equal(t, 7<<20, <-sock.read)
		require.Equal(t, 7<<20, <-sock.write)

		require.NoError(t, svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
		require.NoError(t, sock.Close())
	})
}
