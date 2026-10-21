// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	openapi "github.com/z5labs/humus/openapi/v3"
)

// rows is the path the streaming tests serve.
var rows = openapi.Root().Lit("rows")

// readerCodec is a Decoder[io.Reader] that streams: it hands the handler
// the request's body unread.
type readerCodec struct{}

func readerDecoder() openapi.Decoder[io.Reader] { return readerCodec{} }

func (readerCodec) MediaType() string { return "text/plain" }

func (readerCodec) Schema() (openapi.Schema, error) { return nil, nil }

func (readerCodec) Decode(r *http.Request) (io.Reader, error) { return r.Body, nil }

// rowsCodec is an Encoder[iter.Seq[T]] that streams: it writes each item
// as a line of JSON as the iterator yields it.
type rowsCodec[T any] struct{}

func rowsEncoder[T any]() openapi.Encoder[iter.Seq[T]] { return rowsCodec[T]{} }

func (rowsCodec[T]) MediaType() string { return "application/jsonl" }

func (rowsCodec[T]) Schema() (openapi.Schema, error) { return nil, nil }

func (rowsCodec[T]) Encode(seq iter.Seq[T]) (io.WriterTo, error) { return rowsWriter[T](seq), nil }

// rowsWriter writes each item of its iterator as a line of JSON.
type rowsWriter[T any] iter.Seq[T]

func (rw rowsWriter[T]) WriteTo(w io.Writer) (int64, error) {
	var n int64
	var err error
	for v := range rw {
		var b []byte
		b, err = json.Marshal(v)
		if err != nil {
			break
		}
		var m int
		m, err = w.Write(append(b, '\n'))
		n += int64(m)
		if err != nil {
			break
		}
	}
	return n, err
}

// writerFunc is a WriterTo that is a function, so a handler can return the
// body it writes, as the end user's download does.
type writerFunc func(w io.Writer) (int64, error)

func (f writerFunc) WriteTo(w io.Writer) (int64, error) { return f(w) }

// writerFuncCodec is the Encoder of a handler that returns its own
// writerFunc. A writerFunc has no Len.
type writerFuncCodec struct{}

func writerFuncEncoder() openapi.Encoder[writerFunc] { return writerFuncCodec{} }

func (writerFuncCodec) MediaType() string { return "text/plain" }

func (writerFuncCodec) Schema() (openapi.Schema, error) { return nil, nil }

func (writerFuncCodec) Encode(v writerFunc) (io.WriterTo, error) { return v, nil }

// emptyReaderCodec is an Encoder[openapi.EmptyBody] with no media type,
// whose WriterTo is an empty *bytes.Reader, which has a Len.
type emptyReaderCodec struct{}

func emptyReaderEncoder() openapi.Encoder[openapi.EmptyBody] { return emptyReaderCodec{} }

func (emptyReaderCodec) MediaType() string { return "" }

func (emptyReaderCodec) Schema() (openapi.Schema, error) { return nil, nil }

func (emptyReaderCodec) Encode(openapi.EmptyBody) (io.WriterTo, error) {
	return bytes.NewReader(nil), nil
}

// result is a response's headers, or the error that came instead.
type result struct {
	resp *http.Response
	err  error
}

// start sends req with c, and returns its result once its headers arrive.
func start(c *http.Client, req *http.Request) <-chan result {
	ch := make(chan result, 1)
	go func() {
		resp, err := c.Do(req)
		ch <- result{resp: resp, err: err}
	}()
	return ch
}

func TestAPI_streams_an_upload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stored := make(chan string)
		s := serveAPI(t, []openapi.Operation{
			openapi.Post(rows, func(_ context.Context, req openapi.Request[io.Reader]) (int, error) {
				sc := bufio.NewScanner(req.Body())
				n := 0
				for sc.Scan() {
					if n == 0 {
						stored <- sc.Text()
					}
					n++
				}
				return n, sc.Err()
			}, readerDecoder, jsonEncoder),
		})

		pr, pw := io.Pipe()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://localhost:8443/rows", pr)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "text/plain")
		got := start(s.client, req)

		_, err = io.WriteString(pw, "row 1\n")
		require.NoError(t, err)
		// The handler stores the first row while the second is unsent: a
		// body read in full before the handler runs deadlocks here.
		require.Equal(t, "row 1", <-stored)
		_, err = io.WriteString(pw, "row 2\n")
		require.NoError(t, err)
		require.NoError(t, pw.Close())

		r := <-got
		require.NoError(t, r.err)
		defer func() { _ = r.resp.Body.Close() }()
		body, err := io.ReadAll(r.resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, r.resp.StatusCode)
		require.Equal(t, "2", string(body))

		s.stop()
	})
}

func TestAPI_streams_a_download(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client func(*server) *http.Client
	}{
		{"over HTTP/2", func(s *server) *http.Client { return s.client }},
		{"over HTTP/1.1", func(s *server) *http.Client { return s.http1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				read := make(chan struct{})
				s := serveAPI(t, []openapi.Operation{
					openapi.Get(rows, func(context.Context, openapi.Request[openapi.EmptyBody]) (iter.Seq[item], error) {
						return func(yield func(item) bool) {
							if !yield(item{Name: "apple", Quantity: 1}) {
								return
							}
							<-read
							yield(item{Name: "pear", Quantity: 2})
						}, nil
					}, rowsEncoder),
				})

				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443/rows", nil)
				require.NoError(t, err)
				r := <-start(tc.client(s), req)
				require.NoError(t, r.err)
				defer func() { _ = r.resp.Body.Close() }()
				require.Equal(t, http.StatusOK, r.resp.StatusCode)
				require.Equal(t, "application/jsonl", r.resp.Header.Get("Content-Type"))

				// The client reads the first row before the iterator yields
				// the second: a response buffered until the end deadlocks
				// here.
				br := bufio.NewReader(r.resp.Body)
				first, err := br.ReadString('\n')
				require.NoError(t, err)
				require.JSONEq(t, `{"name":"apple","quantity":1}`, first)
				close(read)
				rest, err := io.ReadAll(br)
				require.NoError(t, err)
				require.JSONEq(t, `{"name":"pear","quantity":2}`, string(rest))

				s.stop()
			})
		})
	}
}

func TestAPI_runs_the_WriterTo_with_the_request_context_live(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t, []openapi.Operation{
			openapi.Get(rows, func(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (writerFunc, error) {
				return func(w io.Writer) (int64, error) {
					spanID := trace.SpanFromContext(ctx).SpanContext().SpanID()
					n, err := fmt.Fprintf(w, "%v %s", ctx.Err(), spanID)
					return int64(n), err
				}, nil
			}, writerFuncEncoder),
		})

		r := s.get("/rows")
		require.Equal(t, http.StatusOK, r.status)

		s.stop()
		spans := serverSpans(s.tel)
		require.Len(t, spans, 1)
		require.Equal(t, "<nil> "+spans[0].SpanContext().SpanID().String(), r.body,
			"the handler's context is live, and is the request's, when the WriterTo runs")
	})
}

func TestAPI_cuts_a_response_short_when_its_WriterTo_fails(t *testing.T) {
	for _, p := range []struct {
		name    string
		client  func(*server) *http.Client
		aborted func(t *testing.T, written, body string, err error)
	}{
		{
			name:   "over HTTP/2",
			client: func(s *server) *http.Client { return s.client },
			aborted: func(t *testing.T, written, body string, err error) {
				require.ErrorContains(t, err, "stream error", "want the stream reset")
				// The reset may overtake the data the client hasn't read.
				require.True(t, strings.HasPrefix(written, body), "body %q is not a prefix of %q", body, written)
			},
		},
		{
			name:   "over HTTP/1.1",
			client: func(s *server) *http.Client { return s.http1 },
			aborted: func(t *testing.T, written, body string, err error) {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF, "want the response cut short before its final chunk")
				require.Equal(t, written, body, "each write was flushed before the failure")
			},
		},
	} {
		for _, tc := range []struct {
			name    string
			written string // what the WriterTo writes before it fails
			err     error
		}{
			{"partway through", "row 1\n", errors.New("S3 failed partway")},
			{"after the last byte", "row 1\nrow 2\n", errors.New("marking the download failed")},
		} {
			t.Run(p.name+", "+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := serveAPI(t, []openapi.Operation{
						openapi.Get(rows, func(context.Context, openapi.Request[openapi.EmptyBody]) (writerFunc, error) {
							return func(w io.Writer) (int64, error) {
								var n int64
								for _, line := range strings.SplitAfter(tc.written, "\n") {
									if line == "" {
										continue
									}
									m, err := io.WriteString(w, line)
									n += int64(m)
									if err != nil {
										return n, err
									}
								}
								return n, tc.err
							}, nil
						}, writerFuncEncoder),
					})

					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443/rows", nil)
					require.NoError(t, err)
					r := <-start(p.client(s), req)
					require.NoError(t, r.err, "the status went out before the failure")
					require.Equal(t, http.StatusOK, r.resp.StatusCode)
					require.Empty(t, r.resp.Header.Get("Content-Length"))
					body, err := io.ReadAll(r.resp.Body)
					_ = r.resp.Body.Close()
					p.aborted(t, tc.written, string(body), err)

					s.stop()
					spans := serverSpans(s.tel)
					require.Len(t, spans, 1)
					require.Equal(t, []string{tc.err.Error()}, exceptionMessages(spans[0]))
					require.Equal(t, codes.Error, spans[0].Status().Code)

					recs := records(s.tel)
					require.Len(t, recs, 1)
					require.Equal(t, "response failed", recs[0].Body().AsString())
					require.Equal(t, log.SeverityError, recs[0].Severity())
					require.Equal(t, spans[0].SpanContext().TraceID(), recs[0].TraceID())
					require.Equal(t, spans[0].SpanContext().SpanID(), recs[0].SpanID())
					require.Equal(t, map[string]string{
						"http.request.method": "GET",
						"http.route":          "/rows",
						"error.type":          "*errors.errorString",
						"exception.message":   tc.err.Error(),
					}, attrs(recs[0]))

					// The abort is passed through httpserver, which records
					// no "handler panicked" for it.
					for _, rec := range s.tel.Logs() {
						require.NotEqual(t, "handler panicked", rec.Body().AsString())
					}
				})
			})
		}
	}
}

func TestAPI_records_a_WriterTo_failure_after_the_client_went_away_on_the_span_only(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t, []openapi.Operation{
			openapi.Get(rows, func(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (writerFunc, error) {
				return func(w io.Writer) (int64, error) {
					n, err := io.WriteString(w, "row 1\n")
					if err != nil {
						return int64(n), err
					}
					<-ctx.Done()
					return int64(n), ctx.Err()
				}, nil
			}, writerFuncEncoder),
		})

		ctx, cancel := context.WithCancel(t.Context())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://localhost:8443/rows", nil)
		require.NoError(t, err)
		r := <-start(s.client, req)
		require.NoError(t, r.err)
		first, err := bufio.NewReader(r.resp.Body).ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "row 1\n", first)
		cancel()
		_ = r.resp.Body.Close()

		s.stop()
		spans := serverSpans(s.tel)
		require.Len(t, spans, 1)
		require.Equal(t, []string{"context canceled"}, exceptionMessages(spans[0]))
		require.Equal(t, codes.Error, spans[0].Status().Code)
		require.Empty(t, records(s.tel))
		for _, rec := range s.tel.Logs() {
			require.NotEqual(t, "handler panicked", rec.Body().AsString())
		}
	})
}

func TestAPI_sets_Content_Length_from_the_WriterTos_Len(t *testing.T) {
	for _, p := range []struct {
		name   string
		client func(*server) *http.Client
		proto  int
	}{
		{"over HTTP/2", func(s *server) *http.Client { return s.client }, 2},
		{"over HTTP/1.1", func(s *server) *http.Client { return s.http1 }, 1},
	} {
		t.Run(p.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				payload := []byte("a body of known length")
				s := serveAPI(t, []openapi.Operation{
					// rawEncoder returns a *bytes.Reader, which has a Len.
					openapi.Get(rows, func(context.Context, openapi.Request[openapi.EmptyBody]) ([]byte, error) {
						return payload, nil
					}, rawEncoder),
					// A writerFunc has none.
					openapi.Get(rows.Lit("streamed"), func(context.Context, openapi.Request[openapi.EmptyBody]) (writerFunc, error) {
						return func(w io.Writer) (int64, error) {
							n, err := w.Write(payload)
							return int64(n), err
						}, nil
					}, writerFuncEncoder),
					// A *bytes.Reader for a 204, which has no body.
					openapi.Delete(rows, func(context.Context, openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
						return openapi.EmptyBody{}, nil
					}, emptyReaderEncoder),
				})
				c := p.client(s)

				do := func(method, path string) (*http.Response, string) {
					req, err := http.NewRequestWithContext(t.Context(), method, "https://localhost:8443"+path, nil)
					require.NoError(t, err)
					r := <-start(c, req)
					require.NoError(t, r.err)
					defer func() { _ = r.resp.Body.Close() }()
					b, err := io.ReadAll(r.resp.Body)
					require.NoError(t, err)
					require.Equal(t, p.proto, r.resp.ProtoMajor)
					return r.resp, string(b)
				}
				get := func(path string) (*http.Response, string) { return do(http.MethodGet, path) }

				resp, body := get("/rows")
				require.Equal(t, string(payload), body)
				require.Equal(t, strconv.Itoa(len(payload)), resp.Header.Get("Content-Length"))
				require.Equal(t, int64(len(payload)), resp.ContentLength)
				require.Empty(t, resp.TransferEncoding)

				resp, body = get("/rows/streamed")
				require.Equal(t, string(payload), body)
				require.Empty(t, resp.Header.Get("Content-Length"))
				require.Equal(t, int64(-1), resp.ContentLength)
				if p.proto == 1 {
					require.Equal(t, []string{"chunked"}, resp.TransferEncoding)
				}

				resp, body = do(http.MethodDelete, "/rows")
				require.Equal(t, http.StatusNoContent, resp.StatusCode)
				require.Empty(t, body)
				require.Empty(t, resp.Header.Get("Content-Length"), "a 204 has no body, and no Content-Length")

				s.stop()
			})
		})
	}
}

// untilAWriteFails is an operation whose WriterTo keeps writing until a
// write fails.
func untilAWriteFails() openapi.Operation {
	return openapi.Get(rows, func(context.Context, openapi.Request[openapi.EmptyBody]) (writerFunc, error) {
		return func(w io.Writer) (int64, error) {
			var n int64
			for i := 0; i < 1<<20; i++ {
				m, err := fmt.Fprintf(w, "row %d\n", i)
				n += int64(m)
				if err != nil {
					return n, err
				}
			}
			return n, errors.New("the client never went away")
		}, nil
	}, writerFuncEncoder)
}

// requireOnTheSpanOnly checks that the request's failed write went on its
// span, with the span's status set to Error, and that nothing was logged.
func requireOnTheSpanOnly(t *testing.T, s *server) {
	t.Helper()
	spans := serverSpans(s.tel)
	require.Len(t, spans, 1)
	msgs := exceptionMessages(spans[0])
	require.Len(t, msgs, 1)
	require.NotEqual(t, "the client never went away", msgs[0])
	require.Equal(t, codes.Error, spans[0].Status().Code)
	require.Empty(t, records(s.tel), "a write that failed because the client went away isn't logged")
	for _, rec := range s.tel.Logs() {
		require.NotEqual(t, "handler panicked", rec.Body().AsString())
	}
}

func TestAPI_records_a_failed_write_after_the_client_went_away_on_the_span_only(t *testing.T) {
	for _, p := range []struct {
		name   string
		client func(*server) *http.Client
	}{
		// httpservertest.NewClient keeps no idle connection, so on cancel
		// it resets the stream and closes the connection. Whether the
		// server cancels the request's context before the write fails
		// depends on which of its goroutines runs first.
		{"over HTTP/2", func(s *server) *http.Client { return s.client }},
		{"over HTTP/1.1", func(s *server) *http.Client { return s.http1 }},
	} {
		t.Run(p.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serveAPI(t, []openapi.Operation{untilAWriteFails()})

				ctx, cancel := context.WithCancel(t.Context())
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://localhost:8443/rows", nil)
				require.NoError(t, err)
				r := <-start(p.client(s), req)
				require.NoError(t, r.err)
				first, err := bufio.NewReader(r.resp.Body).ReadString('\n')
				require.NoError(t, err)
				require.Equal(t, "row 0\n", first)
				cancel()
				_ = r.resp.Body.Close()

				s.stop()
				requireOnTheSpanOnly(t, s)
			})
		})
	}
}

// heldReads holds the server's reads: once held is set, every read the
// server makes on a connection it accepted waits for release before it
// returns what it read. So the server sees nothing the client sent after
// held was set, such as its RST_STREAM or its close, until release.
type heldReads struct {
	held    atomic.Bool
	release chan struct{}
}

func (g *heldReads) host(h humus.Host) humus.Host {
	listen := h.Listen
	h.Listen = func(network, address string) (net.Listener, error) {
		l, err := listen(network, address)
		if err != nil {
			return nil, err
		}
		return heldListener{Listener: l, g: g}, nil
	}
	return h
}

type heldListener struct {
	net.Listener
	g *heldReads
}

func (l heldListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return heldConn{Conn: c, g: l.g}, nil
}

type heldConn struct {
	net.Conn
	g *heldReads
}

func (c heldConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.g.held.Load() {
		<-c.g.release
	}
	return n, err
}

func TestAPI_records_a_failed_write_on_a_closed_connection_on_the_span_only(t *testing.T) {
	for _, p := range []struct {
		name   string
		client func(*server) *http.Client
	}{
		// Over HTTP/2 the write fails on the closed connection while the
		// request's context is live: the server cancels it only once it
		// reads the client's RST_STREAM or sees the connection end.
		{"over HTTP/2", func(s *server) *http.Client { return s.client }},
		{"over HTTP/1.1", func(s *server) *http.Client { return s.http1 }},
	} {
		t.Run(p.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := &heldReads{release: make(chan struct{})}
				s := serveOn(t, g.host, func(s *humus.Setup) (http.Handler, error) {
					return newAPI(s, []openapi.Operation{untilAWriteFails()})
				})

				ctx, cancel := context.WithCancel(t.Context())
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://localhost:8443/rows", nil)
				require.NoError(t, err)
				r := <-start(p.client(s), req)
				require.NoError(t, r.err)
				first, err := bufio.NewReader(r.resp.Body).ReadString('\n')
				require.NoError(t, err)
				require.Equal(t, "row 0\n", first)
				g.held.Store(true)
				cancel()
				_ = r.resp.Body.Close()

				// The write has failed, and the failure been recorded,
				// before the server reads what the client sent.
				synctest.Wait()
				held := records(s.tel)
				close(g.release)

				s.stop()
				require.Empty(t, held, "a write that failed because the client went away isn't logged")
				requireOnTheSpanOnly(t, s)
			})
		})
	}
}

func TestAPI_logs_a_write_the_server_refuses(t *testing.T) {
	for _, p := range []struct {
		name   string
		client func(*server) *http.Client
	}{
		{"over HTTP/2", func(s *server) *http.Client { return s.client }},
		{"over HTTP/1.1", func(s *server) *http.Client { return s.http1 }},
	} {
		for _, tc := range []struct {
			name string
			path string
		}{
			{"more than its Len", "/rows"},
			{"a body for a 204", "/rows/none"},
		} {
			t.Run(p.name+", "+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := serveAPI(t, []openapi.Operation{
						openapi.Get(rows, func(context.Context, openapi.Request[openapi.EmptyBody]) (shortLen, error) {
							return shortLen("row 1\n"), nil
						}, shortLenEncoder),
						openapi.Get(rows.Lit("none"), func(context.Context, openapi.Request[openapi.EmptyBody]) (writerFunc, error) {
							return func(w io.Writer) (int64, error) {
								n, err := io.WriteString(w, "row 1\n")
								return int64(n), err
							}, nil
						}, noContentEncoder),
					})

					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443"+tc.path, nil)
					require.NoError(t, err)
					r := <-start(p.client(s), req)
					if r.err == nil {
						_, _ = io.Copy(io.Discard, r.resp.Body)
						_ = r.resp.Body.Close()
					}

					s.stop()
					spans := serverSpans(s.tel)
					require.Len(t, spans, 1)
					require.Len(t, exceptionMessages(spans[0]), 1)
					require.Equal(t, codes.Error, spans[0].Status().Code)
					recs := records(s.tel)
					require.Len(t, recs, 1, "a write the server refused is the server's failure")
					require.Equal(t, "response failed", recs[0].Body().AsString())
				})
			})
		}
	}
}

// shortLen is a body its WriteTo writes twice, whose Len counts it once.
type shortLen string

func (b shortLen) Len() int { return len(b) }

func (b shortLen) WriteTo(w io.Writer) (int64, error) {
	var n int64
	for range 2 {
		m, err := io.WriteString(w, string(b))
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// shortLenCodec is shortLen's Encoder.
type shortLenCodec struct{}

func shortLenEncoder() openapi.Encoder[shortLen] { return shortLenCodec{} }

func (shortLenCodec) MediaType() string { return "text/plain" }

func (shortLenCodec) Schema() (openapi.Schema, error) { return nil, nil }

func (shortLenCodec) Encode(v shortLen) (io.WriterTo, error) { return v, nil }

// noContentCodec is a writerFunc's Encoder with no media type, so its
// response is a 204, which has no body.
type noContentCodec struct{}

func noContentEncoder() openapi.Encoder[writerFunc] { return noContentCodec{} }

func (noContentCodec) MediaType() string { return "" }

func (noContentCodec) Schema() (openapi.Schema, error) { return nil, nil }

func (noContentCodec) Encode(v writerFunc) (io.WriterTo, error) { return v, nil }
