// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/humustest"
)

// unreadBody is a response body that fails t if it is read.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("the default Classify read a body")
	return 0, io.EOF
}

func (unreadBody) Close() error { return nil }

func TestDefaultSettings_Classify(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		classify := httpclient.DefaultSettings().Classify
		now := time.Now()

		respond := func(code int, retryAfter string) *http.Response {
			resp := &http.Response{StatusCode: code, Header: http.Header{}, Body: unreadBody{t: t}}
			if retryAfter != "" {
				resp.Header.Set("Retry-After", retryAfter)
			}
			return resp
		}
		success := httpclient.Classification{Retry: retry.Success, Breaker: circuit.Success}
		stopFailure := httpclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}
		stopSuccess := httpclient.Classification{Retry: retry.Stop, Breaker: circuit.Success}
		retryFailure := func(after time.Duration) httpclient.Classification {
			return httpclient.Classification{Retry: retry.Retry, RetryAfter: after, Breaker: circuit.Failure}
		}
		retrySuccess := func(after time.Duration) httpclient.Classification {
			return httpclient.Classification{Retry: retry.Retry, RetryAfter: after, Breaker: circuit.Success}
		}

		for _, c := range []struct {
			name string
			resp *http.Response
			err  error
			want httpclient.Classification
		}{
			{name: "100", resp: respond(http.StatusContinue, ""), want: success},
			{name: "200", resp: respond(http.StatusOK, ""), want: success},
			{name: "204", resp: respond(http.StatusNoContent, ""), want: success},
			{name: "302", resp: respond(http.StatusFound, ""), want: success},
			{name: "400", resp: respond(http.StatusBadRequest, ""), want: success},
			{name: "404", resp: respond(http.StatusNotFound, ""), want: success},
			{name: "409", resp: respond(http.StatusConflict, ""), want: success},
			{name: "499", resp: respond(499, ""), want: success},
			{name: "a 200 with a Retry-After", resp: respond(http.StatusOK, "3"), want: success},

			{name: "408", resp: respond(http.StatusRequestTimeout, ""), want: retrySuccess(0)},
			{name: "408 ignores a Retry-After", resp: respond(http.StatusRequestTimeout, "3"), want: retrySuccess(0)},

			{name: "429", resp: respond(http.StatusTooManyRequests, ""), want: retrySuccess(0)},
			{name: "429 after a number of seconds", resp: respond(http.StatusTooManyRequests, "7"), want: retrySuccess(7 * time.Second)},
			{name: "429 after 0 seconds", resp: respond(http.StatusTooManyRequests, "0"), want: retrySuccess(0)},
			{name: "429 after an HTTP-date", resp: respond(http.StatusTooManyRequests, now.Add(30*time.Second).UTC().Format(http.TimeFormat)), want: retrySuccess(30 * time.Second)},
			{name: "429 after an HTTP-date in the past", resp: respond(http.StatusTooManyRequests, now.Add(-time.Hour).UTC().Format(http.TimeFormat)), want: retrySuccess(0)},
			{name: "429 after an RFC 850 date", resp: respond(http.StatusTooManyRequests, now.Add(time.Minute).UTC().Format(time.RFC850)), want: retrySuccess(time.Minute)},

			{name: "500", resp: respond(http.StatusInternalServerError, ""), want: stopFailure},
			{name: "500 ignores a Retry-After", resp: respond(http.StatusInternalServerError, "3"), want: stopFailure},
			{name: "502", resp: respond(http.StatusBadGateway, ""), want: retryFailure(0)},
			{name: "504", resp: respond(http.StatusGatewayTimeout, ""), want: retryFailure(0)},

			{name: "503", resp: respond(http.StatusServiceUnavailable, ""), want: retryFailure(0)},
			{name: "503 after a number of seconds", resp: respond(http.StatusServiceUnavailable, "2"), want: retryFailure(2 * time.Second)},
			{name: "503 after a number of seconds, padded", resp: respond(http.StatusServiceUnavailable, " 2 "), want: retryFailure(2 * time.Second)},
			{name: "503 after an HTTP-date", resp: respond(http.StatusServiceUnavailable, now.Add(4*time.Second).UTC().Format(http.TimeFormat)), want: retryFailure(4 * time.Second)},
			{name: "503 after more seconds than a Duration holds", resp: respond(http.StatusServiceUnavailable, "99999999999999999999"), want: retryFailure(math.MaxInt64)},
			{name: "503 after more seconds than a Duration holds, in range of an int64", resp: respond(http.StatusServiceUnavailable, strconv.FormatInt(math.MaxInt64/int64(time.Second)+1, 10)), want: retryFailure(math.MaxInt64)},
			{name: "503 with a malformed Retry-After", resp: respond(http.StatusServiceUnavailable, "soon"), want: retryFailure(0)},
			{name: "503 with a negative Retry-After", resp: respond(http.StatusServiceUnavailable, "-5"), want: retryFailure(0)},
			{name: "503 with a signed Retry-After", resp: respond(http.StatusServiceUnavailable, "+5"), want: retryFailure(0)},
			{name: "503 with a fractional Retry-After", resp: respond(http.StatusServiceUnavailable, "1.5"), want: retryFailure(0)},

			{name: "501", resp: respond(http.StatusNotImplemented, ""), want: stopSuccess},
			{name: "505", resp: respond(http.StatusHTTPVersionNotSupported, ""), want: stopSuccess},
			{name: "599", resp: respond(599, ""), want: stopSuccess},

			{name: "a certificate signed by an unknown authority", err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, want: stopFailure},
			{name: "a certificate for another host", err: fmt.Errorf("handshake: %w", x509.HostnameError{Host: "payments"}), want: stopFailure},
			{name: "an expired certificate", err: x509.CertificateInvalidError{Reason: x509.Expired}, want: stopFailure},
			{name: "an unknown authority", err: x509.UnknownAuthorityError{}, want: stopFailure},
			{name: "a refused connection", err: errors.New("connection refused"), want: retryFailure(0)},
			{name: "AttemptTimeout", err: fmt.Errorf("the attempt had no response: %w", context.DeadlineExceeded), want: retryFailure(0)},
			{name: "a reset", err: io.ErrUnexpectedEOF, want: retryFailure(0)},
		} {
			require.Equal(t, c.want, classify(c.resp, c.err), c.name)
		}
	})
}

func TestClient_counts_retries_and_redirects_as_resends(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		mux := http.NewServeMux()
		mux.HandleFunc("GET /old", func(w http.ResponseWriter, r *http.Request) {
			if c.record(r) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			http.Redirect(w, r, "/new", http.StatusFound)
		})
		mux.HandleFunc("GET /new", func(w http.ResponseWriter, r *http.Request) {
			c.record(r)
			_, _ = io.WriteString(w, "new")
		})
		stop := startPayments(t, h, h.Host(), ca, mux)
		defer stop()

		tel := humustest.NewTelemetry()
		require.NoError(t, callOnce(t, h, tel, ca, "https://payments:8443/old"))

		spans := clientSpans(tel)
		require.Len(t, spans, 3)
		resends := func(s sdktrace.ReadOnlySpan) attribute.Value { return attrs(s)["http.request.resend_count"] }
		require.Equal(t, "https://payments:8443/old", attrs(spans[0])["url.full"].AsString())
		require.Equal(t, attribute.Value{}, resends(spans[0]))
		require.Equal(t, "https://payments:8443/old", attrs(spans[1])["url.full"].AsString())
		require.EqualValues(t, 1, resends(spans[1]).AsInt64(), "the retry")
		require.Equal(t, "https://payments:8443/new", attrs(spans[2])["url.full"].AsString())
		require.EqualValues(t, 2, resends(spans[2]).AsInt64(), "the redirect, after a retry")
	})
}
