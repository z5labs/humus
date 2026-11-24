// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver_test

import (
	"context"
	"io"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	openapi "github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run whole Apps on Defaults, so Shutdown begins
// 5s after SIGTERM, and the grace period is 10s.

// overBoth is each protocol a client speaks to the Service.
var overBoth = []struct {
	name   string
	offers protocols
	proto  int
}{
	{"over HTTP/2", offersH2, 2},
	{"over HTTP/1.1", offersHTTP1, 1},
}

// streamSettings is settingsFor with no ReadTimeout and no WriteTimeout,
// which would otherwise end a long response on their own: only the
// shutdown ends one here.
func streamSettings(t *testing.T, ca *humustest.CA) httpserver.Settings {
	t.Helper()
	settings := settingsFor(t, ca)
	settings.ReadTimeout = 0
	settings.WriteTimeout = 0
	return settings
}

// startOnDefaults starts an App on Defaults, on h, whose only Service serves
// handler with settings, and waits for it to listen.
func startOnDefaults(t *testing.T, h *humustest.Host, settings httpserver.Settings, handler http.Handler) (signal func(), status func() bedrock.ExitStatus) {
	t.Helper()
	p, st := humustest.Start(t, h.Host(), serves(settings, handler),
		humustest.WithApp(httpserver.Defaults()...),
		humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
	)
	<-h.Listening("localhost:8443")
	signal = func() {
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
	}
	status = func() bedrock.ExitStatus {
		s := <-st
		require.Empty(t, p.Stderr())
		return s
	}
	return signal, status
}

// event writes one event of an event stream, and flushes it.
func event(w http.ResponseWriter, data string) {
	_, _ = io.WriteString(w, "data: "+data+"\n\n")
	_ = http.NewResponseController(w).Flush()
}

func TestShuttingDown_ends_a_feed_that_watches_it(t *testing.T) {
	for _, tc := range overBoth {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				started := make(chan struct{})
				type ended struct {
					at     time.Time
					ctxErr error
				}
				end := make(chan ended, 1)
				feed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					event(w, "first")
					close(started)
					<-httpserver.ShuttingDown(r.Context())
					end <- ended{at: time.Now(), ctxErr: r.Context().Err()}
					event(w, "last")
				})
				signal, status := startOnDefaults(t, h, streamSettings(t, ca), feed)

				got := get(client(h, ca, tc.offers), "https://localhost:8443/feed")
				<-started
				signal()
				sigterm := time.Now()

				e := <-end
				require.Equal(t, 5*time.Second, e.at.Sub(sigterm), "the channel closes when Shutdown begins, after the drain")
				require.NoError(t, e.ctxErr, "the request's context isn't done when its channel closes")

				r := <-got
				require.NoError(t, r.err)
				require.Equal(t, tc.proto, r.proto)
				require.Equal(t, "data: first\n\ndata: last\n\n", r.body)

				st := status()
				require.Zero(t, st.Code(), "cause: %v", st.Cause())
				require.Less(t, time.Since(sigterm), 6*time.Second, "the feed doesn't hold Shutdown")
			})
		})
	}
}

func TestShuttingDown_other_requests_finish_within_the_grace_period(t *testing.T) {
	for _, tc := range overBoth {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				started := make(chan struct{}, 2)
				mux := http.NewServeMux()
				// An event-stream action that doesn't watch ShuttingDown:
				// it sends its events and returns, 7s after it started.
				mux.HandleFunc("GET /action", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					event(w, "working")
					started <- struct{}{}
					time.Sleep(7 * time.Second)
					event(w, "done")
				})
				// A slow text/plain request, which answers 8s after it
				// started.
				mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
					started <- struct{}{}
					time.Sleep(8 * time.Second)
					w.Header().Set("Content-Type", "text/plain")
					_, _ = io.WriteString(w, "finished")
				})
				signal, status := startOnDefaults(t, h, streamSettings(t, ca), mux)

				c := client(h, ca, tc.offers)
				action := get(c, "https://localhost:8443/action")
				slow := get(c, "https://localhost:8443/slow")
				<-started
				<-started
				signal()
				sigterm := time.Now()

				a := <-action
				require.NoError(t, a.err)
				require.Equal(t, tc.proto, a.proto)
				require.Equal(t, "data: working\n\ndata: done\n\n", a.body)
				require.Equal(t, 7*time.Second, time.Since(sigterm))

				s := <-slow
				require.NoError(t, s.err)
				require.Equal(t, tc.proto, s.proto)
				require.Equal(t, "finished", s.body)
				require.Equal(t, 8*time.Second, time.Since(sigterm))

				st := status()
				require.Zero(t, st.Code(), "cause: %v", st.Cause())
				require.Less(t, time.Since(sigterm), 15*time.Second, "Shutdown waits for both, within the grace period")
			})
		})
	}
}

func TestShuttingDown_is_nil_for_a_context_no_Service_serves(t *testing.T) {
	require.Nil(t, httpserver.ShuttingDown(context.Background()))
	require.Nil(t, httpserver.ShuttingDown(t.Context()))
}

// feedBody is the body of the openapi feed: it writes an event, then waits
// for stop to close, and writes its last.
type feedBody struct {
	stop    <-chan struct{}
	started chan<- struct{}
	ended   chan<- time.Time
}

func (f feedBody) WriteTo(w io.Writer) (int64, error) {
	n, err := io.WriteString(w, "data: first\n\n")
	if err != nil {
		return int64(n), err
	}
	close(f.started)
	<-f.stop
	f.ended <- time.Now()
	m, err := io.WriteString(w, "data: last\n\n")
	return int64(n + m), err
}

// feedCodec is the streaming Encoder of the openapi feed.
type feedCodec struct{}

func (feedCodec) MediaType() string { return "text/event-stream" }

func (feedCodec) Schema() (openapi.Schema, error) { return nil, nil }

func (feedCodec) Encode(f feedBody) (io.WriterTo, error) { return f, nil }

// textError is the openapi API's error, written as plain text.
type textError struct{ msg string }

func (e textError) Error() string { return e.msg }

type textErrorCodec struct{}

func (textErrorCodec) MediaType() string { return "text/plain" }

func (textErrorCodec) Schema() (openapi.Schema, error) { return nil, nil }

func (textErrorCodec) Encode(e textError) (io.WriterTo, error) {
	return writerTo(e.msg), nil
}

// writerTo writes its string.
type writerTo string

func (s writerTo) WriteTo(w io.Writer) (int64, error) {
	n, err := io.WriteString(w, string(s))
	return int64(n), err
}

func TestShuttingDown_ends_an_openapi_feed_whose_encoder_watches_it(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		started := make(chan struct{})
		ended := make(chan time.Time, 1)
		settings := streamSettings(t, ca)
		textErrors := func() openapi.Encoder[textError] { return textErrorCodec{} }
		p, st := humustest.Start(t, h.Host(), func(_ context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
			api, err := openapi.NewAPI(s,
				openapi.Info{Title: "Feeds", Version: "1.0.0"},
				openapi.Unmatched{NotFound: http.NotFoundHandler(), MethodNotAllowed: http.NotFoundHandler()},
				openapi.RequestErrors(func(re *openapi.RequestError) textError { return textError{msg: re.Error()} }, textErrors),
				openapi.CatchAll(http.StatusInternalServerError, func(err error) textError { return textError{msg: err.Error()} }, textErrors),
				[]openapi.Operation{
					openapi.Get(openapi.Root().Lit("feed"), func(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (feedBody, error) {
						return feedBody{stop: httpserver.ShuttingDown(ctx), started: started, ended: ended}, nil
					}, func() openapi.Encoder[feedBody] { return feedCodec{} }),
				},
			)
			if err != nil {
				return nil, err
			}
			return serves(settings, api)(context.Background(), s, h)
		},
			humustest.WithApp(httpserver.Defaults()...),
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening("localhost:8443")

		got := get(client(h, ca, offersH2), "https://localhost:8443/feed")
		<-started
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		sigterm := time.Now()

		require.Equal(t, 5*time.Second, (<-ended).Sub(sigterm), "the feed ends when Shutdown begins")

		r := <-got
		require.NoError(t, r.err)
		require.Equal(t, http.StatusOK, r.code)
		require.Equal(t, "data: first\n\ndata: last\n\n", r.body)

		status := <-st
		require.Zero(t, status.Code(), "cause: %v", status.Cause())
		require.Empty(t, p.Stderr())
		require.Less(t, time.Since(sigterm), 6*time.Second, "the feed doesn't hold Shutdown")
	})
}
