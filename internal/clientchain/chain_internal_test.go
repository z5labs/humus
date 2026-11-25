// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package clientchain

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestOriginOf(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"https://payments":            "https://payments:443",
		"https://payments:443/v1":     "https://payments:443",
		"HTTPS://Payments:8443/a?b=c": "https://payments:8443",
		"http://payments":             "http://payments:80",
		"http://payments:8080":        "http://payments:8080",
		"https://[::1]:8443":          "https://[::1]:8443",
		"https://user:pw@payments":    "https://payments:443",
	} {
		require.Equal(t, want, originOf(mustParse(t, raw)).String(), raw)
	}
	require.Equal(t, originOf(mustParse(t, "https://payments")), originOf(mustParse(t, "https://PAYMENTS:443/x")))
	require.NotEqual(t, originOf(mustParse(t, "https://payments")), originOf(mustParse(t, "http://payments:443")))
}

func TestSpanName(t *testing.T) {
	t.Parallel()

	for method, want := range map[string]string{
		"":                 "GET",
		http.MethodGet:     "GET",
		http.MethodPost:    "POST",
		http.MethodPatch:   "PATCH",
		http.MethodConnect: "CONNECT",
	} {
		require.Equal(t, want, spanName("", &http.Request{Method: method}), "method %q", method)
	}
}
