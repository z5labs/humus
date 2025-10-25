// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
)

// HttpResponseWriter is an interface for errors that can write their own HTTP responses.
// When an error implementing this interface is returned from an operation handler,
// its WriteHttpResponse method is called to generate the HTTP response.
//
// This allows custom error types to control status codes and response bodies.
type HttpResponseWriter interface {
	WriteHttpResponse(context.Context, http.ResponseWriter)
}

// ErrorHandler handles errors that occur during request processing.
// The default error handler logs errors and returns appropriate HTTP status codes.
//
// Custom error handlers can be configured per-operation using [OnError].
type ErrorHandler interface {
	OnError(context.Context, http.ResponseWriter, error)
}

// ErrorHandlerFunc is a function adapter that implements [ErrorHandler].
// It allows regular functions to be used as error handlers.
//
// Example:
//
//	handler := rest.ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
//	    log.Printf("Error: %v", err)
//	    w.WriteHeader(http.StatusInternalServerError)
//	})
type ErrorHandlerFunc func(context.Context, http.ResponseWriter, error)

func (f ErrorHandlerFunc) OnError(ctx context.Context, w http.ResponseWriter, err error) {
	f(ctx, w, err)
}

func defaultErrorHandler(h slog.Handler) ErrorHandlerFunc {
	log := slog.New(h)

	return func(ctx context.Context, w http.ResponseWriter, err error) {
		log.ErrorContext(ctx, "sending error response", slog.Any("error", err))

		hrw, ok := err.(HttpResponseWriter)
		if ok {
			hrw.WriteHttpResponse(ctx, w)
			return
		}

		w.WriteHeader(http.StatusInternalServerError)
	}
}

// BadRequestError represents a 400 Bad Request error.
// It wraps an underlying cause (typically parameter validation errors)
// and implements [HttpResponseWriter] to return a 400 status code.
//
// This error is automatically used by parameter validators like [Required] and [Regex]
// when validation fails.
type BadRequestError struct {
	Cause error
}

func (e BadRequestError) Error() string {
	return fmt.Sprintf("bad request error: %v", e.Cause)
}

// Unwrap returns the underlying cause of the bad request.
// This allows errors.Is and errors.As to work with the wrapped error.
func (e BadRequestError) Unwrap() error {
	return e.Cause
}

// WriteHttpResponse implements [HttpResponseWriter].
// It writes a 400 Bad Request status code to the response.
func (e BadRequestError) WriteHttpResponse(ctx context.Context, rw http.ResponseWriter) {
	rw.WriteHeader(http.StatusBadRequest)
}
