// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/http3/http3server/http3servertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run as service_test.go's do: in synctest bubbles,
// on humustest Hosts.

// startWith starts an App whose only Service serves handler on settings,
// with a certificate from ca, and returns a client for it and a function
// that stops the App, and fails t unless it exits 0.
func startWith(t *testing.T, h *humustest.Host, ca *humustest.CA, settings http3server.Settings, handler http.Handler) (*http.Client, func()) {
	t.Helper()
	p, status := humustest.Start(t, h.Host(), serves(settings, handler),
		humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
	)
	synctest.Wait()
	stop := func() {
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
	return http3servertest.NewClient(t, h, ca.Pool()), stop
}

// requireReset fails t unless err is the server resetting the request's
// stream with H3_INTERNAL_ERROR.
func requireReset(t *testing.T, err error) {
	t.Helper()
	var h3err *http3.Error
	require.ErrorAs(t, err, &h3err)
	require.True(t, h3err.Remote, "the server reset the stream")
	require.Equal(t, http3.ErrCodeInternalError, h3err.ErrorCode)
}

// upload sends a POST to url with c, whose body is n bytes, one a second,
// as do does. Its body ends early once ctx is done.
func upload(t *testing.T, ctx context.Context, c *http.Client, url string, n int) <-chan response {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		for range n {
			select {
			case <-ctx.Done():
				pw.CloseWithError(ctx.Err())
				return
			case <-time.After(time.Second):
			}
			if _, err := pw.Write([]byte("x")); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, pr)
	require.NoError(t, err)
	return do(c, req)
}

// ended is how a handler ended: its request's context's error, and how long
// after it began.
type ended struct {
	err   error
	after time.Duration
}

// waits returns a handler that writes nothing, and waits for its request's
// context, which it reports on ends.
func waits(ends chan<- ended) http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		begun := time.Now()
		<-r.Context().Done()
		ends <- ended{err: r.Context().Err(), after: time.Since(begun)}
	})
}

// reads returns a handler that reads its request's body, reports the read
// on ends, and answers with the number of bytes it read.
func reads(ends chan<- ended) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		begun := time.Now()
		b, err := io.ReadAll(r.Body)
		ends <- ended{err: err, after: time.Since(begun)}
		_, _ = io.WriteString(w, strconv.Itoa(len(b)))
	})
}

func TestService_bounds_each_request_stream_with_WriteTimeout(t *testing.T) {
	t.Run("for a handler that writes nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.WriteTimeout = 3 * time.Second
			ends := make(chan ended, 1)
			c, stop := startWith(t, h, ca, settings, waits(ends))
			defer stop()

			sent := time.Now()
			r := <-get(t, c, "https://localhost:8443/")
			requireReset(t, r.err)
			require.Equal(t, 3*time.Second, time.Since(sent))

			e := <-ends
			require.ErrorIs(t, e.err, context.Canceled)
			require.Equal(t, 3*time.Second, e.after)
		})
	})

	t.Run("for a handler that writes slowly", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.WriteTimeout = 2500 * time.Millisecond
			ends := make(chan ended, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				begun := time.Now()
				rc := http.NewResponseController(w)
				for {
					_, _ = io.WriteString(w, "x")
					_ = rc.Flush()
					select {
					case <-r.Context().Done():
						ends <- ended{err: r.Context().Err(), after: time.Since(begun)}
						return
					case <-time.After(time.Second):
					}
				}
			})
			c, stop := startWith(t, h, ca, settings, handler)
			defer stop()

			r := <-get(t, c, "https://localhost:8443/")
			requireReset(t, r.err)
			require.Equal(t, http.StatusOK, r.code)
			require.Equal(t, "xxx", r.body, "the writes before WriteTimeout reached the client")

			e := <-ends
			require.ErrorIs(t, e.err, context.Canceled)
			require.Equal(t, 2500*time.Millisecond, e.after)
		})
	})

	t.Run("and a handler can't move it once it has passed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.WriteTimeout = time.Second
			moved := make(chan [2]error, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
				rc := http.NewResponseController(w)
				later := time.Now().Add(time.Hour)
				moved <- [2]error{rc.SetWriteDeadline(later), rc.SetReadDeadline(later)}
				_, _ = io.WriteString(w, "late")
			})
			c, stop := startWith(t, h, ca, settings, handler)
			defer stop()

			r := <-get(t, c, "https://localhost:8443/")
			requireReset(t, r.err)
			errs := <-moved
			require.Error(t, errs[0], "SetWriteDeadline")
			require.Error(t, errs[1], "SetReadDeadline")
		})
	})
}

func TestService_bounds_each_request_stream_with_ReadTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		settings := settingsFor(t, ca)
		settings.ReadTimeout = 3 * time.Second
		settings.WriteTimeout = 0
		ends := make(chan ended, 1)
		c, stop := startWith(t, h, ca, settings, reads(ends))
		defer stop()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		got := upload(t, ctx, c, "https://localhost:8443/", 10)

		e := <-ends
		require.Error(t, e.err, "the slow upload ended")
		require.Equal(t, 3*time.Second, e.after)
		cancel()
		<-got
	})
}

func TestService_lets_a_handler_move_its_deadlines(t *testing.T) {
	t.Run("SetWriteDeadline extends WriteTimeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.WriteTimeout = 2 * time.Second
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				select {
				case <-r.Context().Done():
				case <-time.After(4 * time.Second):
					_, _ = io.WriteString(w, "on time")
				}
			})
			c, stop := startWith(t, h, ca, settings, handler)
			defer stop()

			r := <-get(t, c, "https://localhost:8443/")
			require.NoError(t, r.err)
			require.Equal(t, http.StatusOK, r.code)
			require.Equal(t, "on time", r.body)
		})
	})

	t.Run("the extended deadline is honoured", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.WriteTimeout = 2 * time.Second
			ends := make(chan ended, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				waits(ends).ServeHTTP(w, r)
			})
			c, stop := startWith(t, h, ca, settings, handler)
			defer stop()

			r := <-get(t, c, "https://localhost:8443/")
			requireReset(t, r.err)
			e := <-ends
			require.ErrorIs(t, e.err, context.Canceled)
			require.Equal(t, 5*time.Second, e.after)
		})
	})

	t.Run("SetWriteDeadline arms a timer when WriteTimeout is zero", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.WriteTimeout = 0
			ends := make(chan ended, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				waits(ends).ServeHTTP(w, r)
			})
			c, stop := startWith(t, h, ca, settings, handler)
			defer stop()

			r := <-get(t, c, "https://localhost:8443/")
			requireReset(t, r.err)
			require.Equal(t, time.Second, (<-ends).after)
		})
	})

	t.Run("SetWriteDeadline with the zero time stops the timer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.WriteTimeout = time.Second
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				time.Sleep(5 * time.Second)
				_, _ = io.WriteString(w, "late")
			})
			c, stop := startWith(t, h, ca, settings, handler)
			defer stop()

			r := <-get(t, c, "https://localhost:8443/")
			require.NoError(t, r.err)
			require.Equal(t, "late", r.body)
		})
	})

	t.Run("SetReadDeadline extends ReadTimeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.ReadTimeout = 2 * time.Second
			settings.WriteTimeout = 0
			ends := make(chan ended, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				reads(ends).ServeHTTP(w, r)
			})
			c, stop := startWith(t, h, ca, settings, handler)
			defer stop()

			r := <-upload(t, t.Context(), c, "https://localhost:8443/", 5)
			require.NoError(t, r.err)
			require.Equal(t, http.StatusOK, r.code)
			require.Equal(t, "5", r.body)
			require.NoError(t, (<-ends).err)
		})
	})
}

func TestService_zero_timeouts_bound_nothing(t *testing.T) {
	t.Run("WriteTimeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.WriteTimeout = 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(20 * time.Second)
				_, _ = io.WriteString(w, "late")
			})
			c, stop := startWith(t, h, ca, settings, handler)
			defer stop()

			r := <-get(t, c, "https://localhost:8443/")
			require.NoError(t, r.err)
			require.Equal(t, http.StatusOK, r.code)
			require.Equal(t, "late", r.body)
		})
	})

	t.Run("ReadTimeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.ReadTimeout = 0
			settings.WriteTimeout = 0
			ends := make(chan ended, 1)
			c, stop := startWith(t, h, ca, settings, reads(ends))
			defer stop()

			r := <-upload(t, t.Context(), c, "https://localhost:8443/", 20)
			require.NoError(t, r.err)
			require.Equal(t, http.StatusOK, r.code)
			require.Equal(t, "20", r.body)
			e := <-ends
			require.NoError(t, e.err)
			require.Equal(t, 20*time.Second, e.after)
		})
	})
}
