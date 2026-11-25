// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/http3/http3server/http3servertest"
	"github.com/z5labs/humus/humustest"
	openapi "github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
)

// The tests in this file are openapi's stream tests over HTTP/3: an API
// served by http3server in a whole App, inside a synctest bubble, on a
// humustest Host. openapi tells a write the server refused from one the
// connection lost, and aborts a response a WriterTo failed, the same way
// whatever protocol carries it.

// openapiScope is the instrumentation scope of openapi's logger.
const openapiScope = "github.com/z5labs/humus/openapi/v3"

// rows is the path the tests serve.
var rows = openapi.Root().Lit("rows")

// apiServer is an App serving an API through http3server.
type apiServer struct {
	t      *testing.T
	tel    *humustest.Telemetry
	client *http.Client
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

// serveAPI starts an App whose only Service serves an API of ops over
// HTTP/3, recording its telemetry.
func serveAPI(t *testing.T, ops ...openapi.Operation) *apiServer {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	tel := humustest.NewTelemetry()
	settings := settingsFor(t, ca)

	p, status := http3servertest.Start(t, h.Host(), func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
		api, err := openapi.NewAPI(s,
			openapi.Info{Title: "Rows", Version: "1.0.0"},
			openapi.Unmatched{NotFound: http.NotFoundHandler(), MethodNotAllowed: http.NotFoundHandler()},
			openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem[problem.Details]),
			openapi.CatchAll(http.StatusInternalServerError, func(error) problem.Details {
				return problem.Details{Status: http.StatusInternalServerError, Title: "Internal Server Error"}
			}, jsoncodec.Problem[problem.Details]),
			ops,
		)
		if err != nil {
			return nil, err
		}
		svc, err := http3server.NewService(ctx, s, h, settings, api)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithTelemetry(tel.Builder())))
	synctest.Wait()

	return &apiServer{t: t, tel: tel, client: http3servertest.NewClient(t, h, ca.Pool()), p: p, status: status}
}

// stop stops the App, so every span has ended, and checks that it exited
// cleanly, having written nothing to stderr.
func (s *apiServer) stop() {
	s.t.Helper()
	s.p.Signal(syscall.SIGTERM)
	st := <-s.status
	require.Zero(s.t, st.Code(), "cause: %v", st.Cause())
	require.Empty(s.t, s.p.Stderr())
}

// start sends a GET to path, and returns its response once its headers
// arrive, with ctx as the request's context.
func (s *apiServer) start(ctx context.Context, path string) (*http.Response, error) {
	s.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://localhost:8443"+path, nil)
	require.NoError(s.t, err)
	return s.client.Do(req)
}

// serverSpan returns the one server span otelhttp recorded.
func (s *apiServer) serverSpan() sdktrace.ReadOnlySpan {
	s.t.Helper()
	var spans []sdktrace.ReadOnlySpan
	for _, span := range s.tel.Spans() {
		if span.InstrumentationScope().Name == otelhttp.ScopeName {
			spans = append(spans, span)
		}
	}
	require.Len(s.t, spans, 1)
	return spans[0]
}

// openapiRecords returns the records openapi's logger emitted.
func (s *apiServer) openapiRecords() []sdklog.Record {
	var recs []sdklog.Record
	for _, r := range s.tel.Logs() {
		if r.InstrumentationScope().Name == openapiScope {
			recs = append(recs, r)
		}
	}
	return recs
}

// exceptionMessages returns the exception.message of each exception event
// on span.
func exceptionMessages(span sdktrace.ReadOnlySpan) []string {
	var msgs []string
	for _, e := range span.Events() {
		if e.Name != "exception" {
			continue
		}
		for _, kv := range e.Attributes {
			if kv.Key == "exception.message" {
				msgs = append(msgs, kv.Value.AsString())
			}
		}
	}
	return msgs
}

// writerFunc is a WriterTo that is a function.
type writerFunc func(w io.Writer) (int64, error)

func (f writerFunc) WriteTo(w io.Writer) (int64, error) { return f(w) }

// writerFuncCodec is writerFunc's Encoder, as plain text.
type writerFuncCodec struct{ mediaType string }

func (c writerFuncCodec) MediaType() string                      { return c.mediaType }
func (writerFuncCodec) Schema() (openapi.Schema, error)          { return nil, nil }
func (writerFuncCodec) Encode(v writerFunc) (io.WriterTo, error) { return v, nil }

// plain is writerFunc's Encoder as text/plain.
func plain() openapi.Encoder[writerFunc] { return writerFuncCodec{mediaType: "text/plain"} }

// noContent is writerFunc's Encoder with no media type, so its response is
// a 204, which has no body.
func noContent() openapi.Encoder[writerFunc] { return writerFuncCodec{} }

// lenBody is a body that writes body, whose Len is n.
type lenBody struct {
	body string
	n    int
}

func (b lenBody) Len() int { return b.n }

func (b lenBody) WriteTo(w io.Writer) (int64, error) {
	n, err := io.WriteString(w, b.body)
	return int64(n), err
}

// lenBodyCodec is lenBody's Encoder.
type lenBodyCodec struct{}

func lenBodyEncoder() openapi.Encoder[lenBody] { return lenBodyCodec{} }

func (lenBodyCodec) MediaType() string                     { return "text/plain" }
func (lenBodyCodec) Schema() (openapi.Schema, error)       { return nil, nil }
func (lenBodyCodec) Encode(v lenBody) (io.WriterTo, error) { return v, nil }

// servesRows returns a GET of rows/path whose WriterTo is f.
func servesRows(path string, f writerFunc, enc func() openapi.Encoder[writerFunc]) openapi.Operation {
	p := rows
	if path != "" {
		p = rows.Lit(path)
	}
	return openapi.Get(p, func(context.Context, openapi.Request[openapi.EmptyBody]) (writerFunc, error) {
		return f, nil
	}, enc)
}

// untilAWriteFails writes rows until a write fails.
func untilAWriteFails(w io.Writer) (int64, error) {
	var n int64
	for i := 0; i < 1<<20; i++ {
		m, err := fmt.Fprintf(w, "row %d\n", i)
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
	return n, errors.New("the client never went away")
}

func TestOpenAPI_over_HTTP3_logs_a_write_the_server_refuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"more than its Len", "/rows"},
		{"a Len of 0 and a body", "/rows/zero"},
		{"a body for a 204", "/rows/none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serveAPI(t,
					openapi.Get(rows, func(context.Context, openapi.Request[openapi.EmptyBody]) (lenBody, error) {
						return lenBody{body: "row 1\nrow 1\n", n: len("row 1\n")}, nil
					}, lenBodyEncoder),
					openapi.Get(rows.Lit("zero"), func(context.Context, openapi.Request[openapi.EmptyBody]) (lenBody, error) {
						return lenBody{body: "row 1\n", n: 0}, nil
					}, lenBodyEncoder),
					servesRows("none", func(w io.Writer) (int64, error) {
						n, err := io.WriteString(w, "row 1\n")
						return int64(n), err
					}, noContent),
				)

				resp, err := s.start(t.Context(), tc.path)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}

				s.stop()
				span := s.serverSpan()
				require.Len(t, exceptionMessages(span), 1)
				require.Equal(t, codes.Error, span.Status().Code)
				recs := s.openapiRecords()
				require.Len(t, recs, 1, "a write the server refused is the server's failure")
				require.Equal(t, "response failed", recs[0].Body().AsString())
			})
		})
	}
}

func TestOpenAPI_over_HTTP3_records_a_write_the_connection_lost_on_the_span_only(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t, servesRows("", untilAWriteFails, plain))

		ctx, cancel := context.WithCancel(t.Context())
		resp, err := s.start(ctx, "/rows")
		require.NoError(t, err)
		first, err := bufio.NewReader(resp.Body).ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "row 0\n", first)
		cancel()
		_ = resp.Body.Close()

		s.stop()
		span := s.serverSpan()
		msgs := exceptionMessages(span)
		require.Len(t, msgs, 1)
		require.NotEqual(t, "the client never went away", msgs[0])
		require.Equal(t, codes.Error, span.Status().Code)
		require.Empty(t, s.openapiRecords(), "a write that failed because the client went away isn't logged")
	})
}

func TestOpenAPI_over_HTTP3_aborts_a_response_its_WriterTo_failed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errS3 := errors.New("S3 failed partway")
		s := serveAPI(t, servesRows("", func(w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "row 1\n")
			if err != nil {
				return int64(n), err
			}
			return int64(n), errS3
		}, plain))

		// The reset may overtake the status and the data the client hasn't
		// read, as it may over HTTP/2, so the client sees the reset either
		// in place of the response or in place of the rest of its body.
		resp, err := s.start(t.Context(), "/rows")
		if err == nil {
			require.Equal(t, http.StatusOK, resp.StatusCode)
			var body []byte
			body, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.True(t, strings.HasPrefix("row 1\n", string(body)), "body %q is not a prefix of what was written", body)
		}
		var h3err *http3.Error
		require.ErrorAs(t, err, &h3err, "want the stream reset")
		require.Equal(t, http3.ErrCodeInternalError, h3err.ErrorCode)

		s.stop()
		span := s.serverSpan()
		require.Equal(t, []string{errS3.Error()}, exceptionMessages(span))
		recs := s.openapiRecords()
		require.Len(t, recs, 1)
		require.Equal(t, "response failed", recs[0].Body().AsString())
		for _, rec := range s.tel.Logs() {
			require.NotEqual(t, "handler panicked", rec.Body().AsString(), "the abort is passed through")
		}
	})
}

func TestOpenAPI_over_HTTP3_cuts_short_a_body_shorter_than_its_Len(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t, openapi.Get(rows, func(context.Context, openapi.Request[openapi.EmptyBody]) (lenBody, error) {
			return lenBody{body: "row 1\n", n: 2 * len("row 1\n")}, nil
		}, lenBodyEncoder))

		resp, err := s.start(t.Context(), "/rows")
		require.NoError(t, err)
		require.Equal(t, int64(2*len("row 1\n")), resp.ContentLength)
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Error(t, err, "the client sees a failed response rather than one that looks complete")

		s.stop()
	})
}
