// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/z5labs/bedrock/pkg/health"
	"github.com/z5labs/humus/humuspb"
	"google.golang.org/protobuf/proto"
)

func TestReadinessEndpoint(t *testing.T) {
	t.Run("will return http 200 status code", func(t *testing.T) {
		t.Run("if the health metric is healthy", func(t *testing.T) {
			var m health.Binary
			if !assert.True(t, m.Healthy(context.Background())) {
				return
			}

			e := readinessEndpoint(&m)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/health/readiness", nil)

			e.ServeHTTP(w, r)

			resp := w.Result()
			if !assert.Equal(t, http.StatusOK, resp.StatusCode) {
				return
			}

			b, err := io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Empty(t, b) {
				return
			}
		})
	})

	t.Run("will return http 503 status code", func(t *testing.T) {
		t.Run("if the health metric is unhealthy", func(t *testing.T) {
			var m health.Binary
			m.Toggle()
			if !assert.False(t, m.Healthy(context.Background())) {
				return
			}

			e := readinessEndpoint(&m)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/health/readiness", nil)

			e.ServeHTTP(w, r)

			resp := w.Result()
			if !assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode) {
				return
			}
			if !assert.Equal(t, "application/x-protobuf", resp.Header.Get("Content-Type")) {
				return
			}

			b, err := io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}

			var status humuspb.Status
			err = proto.Unmarshal(b, &status)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, humuspb.Code_UNAVAILABLE, status.Code) {
				return
			}
			if !assert.NotEmpty(t, status.Message) {
				return
			}
			if !assert.Empty(t, status.Details) {
				return
			}
		})
	})
}
