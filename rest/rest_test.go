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
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/appbuilder"
)

type unableToListenConfig struct {
	Config `config:",squash"`
}

var errFailedToListen = errors.New("failed to listen")

func (cfg unableToListenConfig) Listener(ctx context.Context) (net.Listener, error) {
	return nil, errFailedToListen
}

type unableToHttpServerConfig struct {
	Config `config:",squash"`
}

var errFailedToHttpServer = errors.New("failed to http server")

func (cfg unableToHttpServerConfig) HttpServer(ctx context.Context, h http.Handler) (*http.Server, error) {
	return nil, errFailedToHttpServer
}

func TestBuilder_Build(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if the api fails to be created", func(t *testing.T) {
			buildErr := errors.New("failed to build api")
			b := appbuilder.FromConfig(Builder(func(ctx context.Context, cfg Config) (*Api, error) {
				return nil, buildErr
			}))

			_, err := b.Build(t.Context(), DefaultConfig())
			require.ErrorIs(t, err, buildErr)
		})

		t.Run("if it fails to listen", func(t *testing.T) {
			b := appbuilder.FromConfig(Builder(func(ctx context.Context, cfg unableToListenConfig) (*Api, error) {
				return NewApi("", ""), nil
			}))

			_, err := b.Build(t.Context(), DefaultConfig())
			require.ErrorIs(t, err, errFailedToListen)
		})

		t.Run("if it fails to initialize http server", func(t *testing.T) {
			b := appbuilder.FromConfig(Builder(func(ctx context.Context, cfg unableToHttpServerConfig) (*Api, error) {
				return NewApi("", ""), nil
			}))

			_, err := b.Build(t.Context(), DefaultConfig())
			require.ErrorIs(t, err, errFailedToHttpServer)
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
			require.Len(t, records, 1)

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
			require.Error(t, caughtErr)
		})
	})
}
