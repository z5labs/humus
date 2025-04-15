// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

type captureHandler struct {
	slog.Handler
	records []slog.Record
}

func (h *captureHandler) Handle(ctx context.Context, record slog.Record) error {
	h.records = append(h.records, record)
	return nil
}

func TestRun(t *testing.T) {
	t.Run("will handle error", func(t *testing.T) {
		t.Run("if it fails to build the App", func(t *testing.T) {
			r := strings.NewReader(``)

			buildErr := errors.New("failed to build app")
			b := func(ctx context.Context, cfg Config) (*App, error) {
				return nil, buildErr
			}

			logHandler := &captureHandler{
				Handler: slog.Default().Handler(),
			}

			Run(r, b, LogHandler(logHandler))

			records := logHandler.records
			if !assert.Len(t, records, 1) {
				return
			}

			record := records[0]
			var caughtErr error
			record.Attrs(func(a slog.Attr) bool {
				if a.Key != "error" {
					return true
				}

				v := a.Value.Any()
				err, ok := v.(error)
				if !ok {
					caughtErr = fmt.Errorf("expected attr to be error: %v", a.Value)
					return false
				}
				caughtErr = err
				return false
			})
			if !assert.ErrorIs(t, caughtErr, buildErr) {
				return
			}
		})

		t.Run("if the job handler returns an error while running", func(t *testing.T) {
			r := strings.NewReader(``)

			handleErr := errors.New("failed to run job handler")
			h := HandlerFunc(func(ctx context.Context) error {
				return handleErr
			})

			a := NewApp(h)

			b := func(ctx context.Context, cfg Config) (*App, error) {
				return a, nil
			}

			logHandler := &captureHandler{
				Handler: slog.Default().Handler(),
			}

			Run(r, b, LogHandler(logHandler))

			records := logHandler.records
			if !assert.Len(t, records, 1) {
				return
			}

			record := records[0]
			var caughtErr error
			record.Attrs(func(a slog.Attr) bool {
				if a.Key != "error" {
					return true
				}

				v := a.Value.Any()
				err, ok := v.(error)
				if !ok {
					caughtErr = fmt.Errorf("expected attr to be error: %v", a.Value)
					return false
				}
				caughtErr = err
				return false
			})
			if !assert.ErrorIs(t, caughtErr, handleErr) {
				return
			}
		})
	})
}
