// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
)

// The tests in this file serve the files API of example_test.go, which the
// docs site quotes.

// memObjects is an Objects in memory. Its MarkDownloaded fails with
// markErr, when it isn't nil.
type memObjects struct {
	mu         sync.Mutex
	objects    map[string][]byte
	downloaded []string
	markErr    error
}

func newMemObjects() *memObjects {
	return &memObjects{objects: make(map[string][]byte)}
}

func (m *memObjects) Put(_ context.Context, name string, body io.Reader) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[name] = b
	return nil
}

func (m *memObjects) Get(_ context.Context, name string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memObjects) MarkDownloaded(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markErr != nil {
		return m.markErr
	}
	m.downloaded = append(m.downloaded, name)
	return nil
}

func (m *memObjects) downloads() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.downloaded...)
}

func TestFiles_streams_an_upload_and_a_download_then_records_it(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := newMemObjects()
		s := serveAPI(t, Files{Objects: objects}.Operations())

		r := s.do(http.MethodPut, "/files/report", "application/octet-stream", "the report's bytes")
		require.Equal(t, http.StatusNoContent, r.status, r.body)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443/files/report", nil)
		require.NoError(t, err)
		resp, err := s.client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
		require.Equal(t, int64(-1), resp.ContentLength, "a download has no Content-Length")
		require.Equal(t, "the report's bytes", string(body))

		s.stop()
		require.Equal(t, []string{"report"}, objects.downloads())
	})
}

func TestFiles_rejects_an_upload_that_isnt_an_octet_stream_or_a_bad_name(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := newMemObjects()
		s := serveAPI(t, Files{Objects: objects}.Operations())

		require.Equal(t, http.StatusUnsupportedMediaType, s.do(http.MethodPut, "/files/report", "text/plain", "x").status)
		require.Equal(t, http.StatusBadRequest, s.do(http.MethodPut, "/files/Not_A_Name", "application/octet-stream", "x").status)

		s.stop()
		require.Empty(t, objects.objects)
	})
}

func TestFiles_answers_a_missing_file_before_the_status(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t, Files{Objects: newMemObjects()}.Operations())

		r := s.get("/files/missing")
		require.Equal(t, http.StatusInternalServerError, r.status, "the catch-all answers an error the handler returned")
		require.Equal(t, "application/problem+json", r.contentType)

		s.stop()
		require.Len(t, logged(s, "request failed"), 1)
	})
}

func TestFiles_cuts_a_download_short_when_recording_it_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := newMemObjects()
		objects.markErr = errors.New("the database is down")
		s := serveAPI(t, Files{Objects: objects}.Operations())
		require.Equal(t, http.StatusNoContent, s.do(http.MethodPut, "/files/report", "application/octet-stream", "the report's bytes").status)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443/files/report", nil)
		require.NoError(t, err)
		resp, err := s.client.Do(req)
		require.NoError(t, err, "the status went out before the failure")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.ErrorContains(t, err, "stream error", "the client sees a failed response, not a complete one")
		require.True(t, strings.HasPrefix("the report's bytes", string(body)))

		s.stop()
		recs := logged(s, "response failed")
		require.Len(t, recs, 1)
		require.Equal(t, "the database is down", recordAttr(recs[0], "exception.message"))
		require.Equal(t, "/files/{name}", recordAttr(recs[0], "http.route"))
		var failed int
		for _, sp := range serverSpans(s.tel) {
			if sp.Status().Code == codes.Error {
				failed++
				require.Equal(t, []string{"the database is down"}, exceptionMessages(sp))
			}
		}
		require.Equal(t, 1, failed, "the download's span records the failure")
		require.Empty(t, objects.downloads())
	})
}

func TestFiles_documents_each_body_under_its_media_type_without_a_schema(t *testing.T) {
	doc := documentOf(t, Files{Objects: newMemObjects()}.Operations())
	require.JSONEq(t, `{"required":true,"content":{"application/octet-stream":{}}}`,
		field(t, doc, "paths", "/files/{name}", "put", "requestBody"))
	require.JSONEq(t, `{"description":"OK","content":{"application/octet-stream":{}}}`,
		field(t, doc, "paths", "/files/{name}", "get", "responses", "200"))
	require.JSONEq(t, `{"description":"No Content"}`,
		field(t, doc, "paths", "/files/{name}", "put", "responses", "204"))
}
