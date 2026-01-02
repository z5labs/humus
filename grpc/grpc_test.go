// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"testing"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"

	"github.com/stretchr/testify/require"
)

func TestBuild(t *testing.T) {
	t.Run("returns a valid builder", func(t *testing.T) {
		listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
			ln, err := net.Listen("tcp", ":0")
			if err != nil {
				return config.Value[net.Listener]{}, err
			}
			return config.ValueOf(ln), nil
		})
		api := NewApi()

		builder := Build(listener, api)
		require.NotNil(t, builder)
	})

	t.Run("returns error when listener fails", func(t *testing.T) {
		expectedErr := errors.New("listener error")
		listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
			return config.Value[net.Listener]{}, expectedErr
		})
		api := NewApi()

		builder := Build(listener, api)
		_, err := builder.Build(context.Background())
		require.ErrorIs(t, err, expectedErr)
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

func (fb failingBuilder) Build(ctx context.Context) (Runtime, error) {
	return Runtime{}, errors.New("build failed")
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

		builder := app.BuilderFunc[Runtime](func(ctx context.Context) (Runtime, error) {
			return Runtime{}, errors.New("test error")
		})

		Run(context.Background(), builder, LogHandler(logHandler))

		require.Len(t, logHandler.records, 1)
	})
}
