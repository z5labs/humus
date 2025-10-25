// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBadRequestError_Error(t *testing.T) {
	t.Run("formats error message with cause", func(t *testing.T) {
		cause := errors.New("missing parameter")
		err := BadRequestError{Cause: cause}

		assert.Equal(t, "bad request error: missing parameter", err.Error())
	})

	t.Run("handles nil cause", func(t *testing.T) {
		err := BadRequestError{Cause: nil}

		assert.Equal(t, "bad request error: <nil>", err.Error())
	})
}

func TestBadRequestError_Unwrap(t *testing.T) {
	t.Run("returns the underlying cause", func(t *testing.T) {
		cause := errors.New("validation failed")
		err := BadRequestError{Cause: cause}

		unwrapped := err.Unwrap()
		assert.Equal(t, cause, unwrapped)
	})

	t.Run("works with errors.Is", func(t *testing.T) {
		cause := errors.New("specific error")
		err := BadRequestError{Cause: cause}

		assert.True(t, errors.Is(err, cause))
	})

	t.Run("works with errors.As", func(t *testing.T) {
		paramErr := MissingRequiredParameterError{Parameter: "id", In: "query"}
		badReqErr := BadRequestError{Cause: paramErr}

		var target MissingRequiredParameterError
		assert.True(t, errors.As(badReqErr, &target))
		assert.Equal(t, "id", target.Parameter)
		assert.Equal(t, "query", target.In)
	})
}

func TestBadRequestError_WriteHttpResponse(t *testing.T) {
	t.Run("writes 400 status code", func(t *testing.T) {
		err := BadRequestError{Cause: errors.New("test")}
		w := httptest.NewRecorder()

		err.WriteHttpResponse(context.Background(), w)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("does not write body", func(t *testing.T) {
		err := BadRequestError{Cause: errors.New("test")}
		w := httptest.NewRecorder()

		err.WriteHttpResponse(context.Background(), w)

		assert.Empty(t, w.Body.String())
	})
}

func TestErrorHandlerFunc(t *testing.T) {
	t.Run("implements ErrorHandler interface", func(t *testing.T) {
		var _ ErrorHandler = ErrorHandlerFunc(nil)
	})

	t.Run("calls the function", func(t *testing.T) {
		called := false
		var capturedCtx context.Context
		var capturedErr error

		handler := ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
			called = true
			capturedCtx = ctx
			capturedErr = err
		})

		testCtx := context.Background()
		testErr := errors.New("test error")
		w := httptest.NewRecorder()

		handler.OnError(testCtx, w, testErr)

		assert.True(t, called)
		assert.Equal(t, testCtx, capturedCtx)
		assert.Equal(t, testErr, capturedErr)
	})

	t.Run("can write custom response", func(t *testing.T) {
		handler := ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
			w.WriteHeader(http.StatusTeapot)
			w.Write([]byte("custom error"))
		})

		w := httptest.NewRecorder()
		handler.OnError(context.Background(), w, errors.New("test"))

		assert.Equal(t, http.StatusTeapot, w.Code)
		assert.Equal(t, "custom error", w.Body.String())
	})
}

func Test_defaultErrorHandler(t *testing.T) {
	t.Run("logs error and returns 500 for generic errors", func(t *testing.T) {
		// defaultErrorHandler requires a non-nil slog.Handler
		handler := defaultErrorHandler(slog.NewTextHandler(io.Discard, nil))
		w := httptest.NewRecorder()

		err := errors.New("generic error")
		handler.OnError(context.Background(), w, err)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})

	t.Run("uses custom response for HttpResponseWriter errors", func(t *testing.T) {
		handler := defaultErrorHandler(slog.NewTextHandler(io.Discard, nil))
		w := httptest.NewRecorder()

		err := BadRequestError{Cause: errors.New("bad request")}
		handler.OnError(context.Background(), w, err)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("handles nil error", func(t *testing.T) {
		handler := defaultErrorHandler(slog.NewTextHandler(io.Discard, nil))
		w := httptest.NewRecorder()

		// Should not panic
		require.NotPanics(t, func() {
			handler.OnError(context.Background(), w, nil)
		})
	})
}

type customHttpResponseError struct {
	statusCode int
	message    string
}

func (e customHttpResponseError) Error() string {
	return e.message
}

func (e customHttpResponseError) WriteHttpResponse(ctx context.Context, w http.ResponseWriter) {
	w.WriteHeader(e.statusCode)
	w.Write([]byte(e.message))
}

func TestHttpResponseWriter_interface(t *testing.T) {
	t.Run("custom error implements HttpResponseWriter", func(t *testing.T) {
		var _ HttpResponseWriter = customHttpResponseError{}
	})

	t.Run("default handler uses WriteHttpResponse", func(t *testing.T) {
		handler := defaultErrorHandler(slog.NewTextHandler(io.Discard, nil))
		w := httptest.NewRecorder()

		err := customHttpResponseError{
			statusCode: http.StatusConflict,
			message:    "conflict occurred",
		}

		handler.OnError(context.Background(), w, err)

		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Equal(t, "conflict occurred", w.Body.String())
	})
}
