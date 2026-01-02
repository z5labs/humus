// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/z5labs/humus/app"
	httpserver "github.com/z5labs/humus/http"

	"github.com/stretchr/testify/require"
)

func TestBuild(t *testing.T) {
	t.Run("returns a valid builder", func(t *testing.T) {
		srv := httpserver.NewServer(httpserver.NewTCPListener())
		api := NewApi("test", "1.0.0")

		builder := Build(srv, api)
		require.NotNil(t, builder)
	})
}

type captureHandler struct {
	slog.Handler
	records []slog.Record
}

func (h *captureHandler) Handle(ctx context.Context, record slog.Record) error {
	h.records = append(h.records, record)
	return nil
}

type failingBuilder struct{}

func (fb failingBuilder) Build(ctx context.Context) (httpserver.App, error) {
	return httpserver.App{}, errors.New("build failed")
}

func TestRun(t *testing.T) {
	t.Run("logs errors from builder", func(t *testing.T) {
		logHandler := &captureHandler{
			Handler: slog.Default().Handler(),
		}

		builder := failingBuilder{}
		err := Run(context.Background(), builder, LogHandler(logHandler))

		require.Error(t, err)
		require.Len(t, logHandler.records, 1)

		record := logHandler.records[0]
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
		require.Error(t, caughtErr)
		require.Equal(t, "build failed", caughtErr.Error())
	})

	t.Run("uses custom log handler when provided", func(t *testing.T) {
		logHandler := &captureHandler{
			Handler: slog.Default().Handler(),
		}

		builder := app.BuilderFunc[httpserver.App](func(ctx context.Context) (httpserver.App, error) {
			return httpserver.App{}, errors.New("test error")
		})

		Run(context.Background(), builder, LogHandler(logHandler))

		require.Len(t, logHandler.records, 1)
	})
}
