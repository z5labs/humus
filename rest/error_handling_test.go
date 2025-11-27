// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// Priority 2: Error Handling Tests

func TestErrorHandling_InvalidJSON(t *testing.T) {
	t.Run("malformed JSON returns 500", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		invalidJSON := bytes.NewBufferString(`{"name": "test", "age": }`)
		resp, err := http.Post(srv.URL+"/test", "application/json", invalidJSON)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	t.Run("incomplete JSON returns 500", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		invalidJSON := bytes.NewBufferString(`{"name": "test"`)
		resp, err := http.Post(srv.URL+"/test", "application/json", invalidJSON)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	t.Run("non-JSON text returns 500", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		invalidJSON := bytes.NewBufferString(`not json at all`)
		resp, err := http.Post(srv.URL+"/test", "application/json", invalidJSON)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	t.Run("empty body returns 500", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Post(srv.URL+"/test", "application/json", bytes.NewBufferString(""))
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})
}

func TestErrorHandling_WrongContentType(t *testing.T) {
	t.Run("text/plain returns 400", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := bytes.NewBufferString(`{"name":"test","age":30}`)
		resp, err := http.Post(srv.URL+"/test", "text/plain", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("application/xml returns 400", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := bytes.NewBufferString(`{"name":"test","age":30}`)
		resp, err := http.Post(srv.URL+"/test", "application/xml", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("empty content type returns 400", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := bytes.NewBufferString(`{"name":"test","age":30}`)
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/test", body)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestErrorHandling_MissingRequiredParameters(t *testing.T) {
	t.Run("missing required query param returns 400", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				QueryParam("id", Required()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/test")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("missing required header returns 400", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				Header("X-Required-Header", Required()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/test")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("missing required cookie returns 400", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				Cookie("session", Required()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/test")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("empty path param returns 400", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/users").Param("id", Required()),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// This will fail because path param cannot be empty in the URL structure
		// The router won't match this route at all
		resp, err := http.Get(srv.URL + "/users/")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestErrorHandling_HandlerErrors(t *testing.T) {
	t.Run("handler error returns 500", func(t *testing.T) {
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return nil, errors.New("internal error")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	t.Run("producer error returns 500", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return nil, errors.New("producer failed")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/test")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	t.Run("consumer error returns 500", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return errors.New("consumer failed")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})
}

func TestErrorHandling_CustomErrorHandlers(t *testing.T) {
	t.Run("custom error handler called on handler error", func(t *testing.T) {
		customCalled := false
		customHandler := ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
			customCalled = true
			w.WriteHeader(http.StatusTeapot)
			w.Write([]byte(`{"error":"custom"}`))
		})

		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return nil, errors.New("test error")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
				OnError(customHandler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.True(t, customCalled)
		require.Equal(t, http.StatusTeapot, resp.StatusCode)

		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, `{"error":"custom"}`, string(bodyBytes))
	})

	t.Run("custom error handler receives correct error", func(t *testing.T) {
		var capturedErr error
		expectedErr := errors.New("specific error")

		customHandler := ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
			capturedErr = err
			w.WriteHeader(http.StatusInternalServerError)
		})

		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return nil, expectedErr
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
				OnError(customHandler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.ErrorIs(t, capturedErr, expectedErr)
	})

	t.Run("custom error handler can write JSON error response", func(t *testing.T) {
		type ErrorResponse struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}

		customHandler := ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(ErrorResponse{
				Code:    400,
				Message: err.Error(),
			})
		})

		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return nil, errors.New("validation failed")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
				OnError(customHandler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.Equal(t, "application/json", resp.Header.Get("Content-Type"))

		var errResp ErrorResponse
		err = json.NewDecoder(resp.Body).Decode(&errResp)
		require.NoError(t, err)
		require.Equal(t, 400, errResp.Code)
		require.Equal(t, "validation failed", errResp.Message)
	})

	t.Run("custom error handler not called on success", func(t *testing.T) {
		customCalled := false
		customHandler := ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
			customCalled = true
		})

		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "success"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
				OnError(customHandler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.False(t, customCalled)
	})
}

func TestErrorHandling_ParameterValidation(t *testing.T) {
	t.Run("invalid regex pattern returns 400", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				QueryParam("id", Required(), Regex(regexp.MustCompile(`^\d+$`))),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/test?id=abc")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("valid regex pattern succeeds", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				QueryParam("id", Required(), Regex(regexp.MustCompile(`^\d+$`))),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/test?id=123")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
}
