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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

type listenerProvider struct {
	Config
	listener func(context.Context) (net.Listener, error)
}

func (lp listenerProvider) Listener(ctx context.Context) (net.Listener, error) {
	return lp.listener(ctx)
}

func TestBuilder_Build(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if the api fails to be created", func(t *testing.T) {
			buildErr := errors.New("failed to build api")
			b := Builder(func(ctx context.Context, cfg Config) (*Api, error) {
				return nil, buildErr
			})

			_, err := b.Build(context.Background(), Config{})
			if !assert.ErrorIs(t, err, buildErr) {
				return
			}
		})

		t.Run("if it fails to listen", func(t *testing.T) {
			b := Builder(func(ctx context.Context, cfg listenerProvider) (*Api, error) {
				return NewApi(), nil
			})

			listenErr := errors.New("failed to listen")
			lp := listenerProvider{
				listener: func(ctx context.Context) (net.Listener, error) {
					return nil, listenErr
				},
			}

			_, err := b.Build(context.Background(), lp)
			if !assert.ErrorIs(t, err, listenErr) {
				return
			}
		})
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

func TestRun(t *testing.T) {
	t.Run("will handle error", func(t *testing.T) {
		t.Run("if the http port is not a uint", func(t *testing.T) {
			r := strings.NewReader(`
http:
  port: -1`)

			b := func(ctx context.Context, cfg Config) (*Api, error) {
				return nil, nil
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
			if !assert.Error(t, caughtErr) {
				return
			}
		})
	})
}
