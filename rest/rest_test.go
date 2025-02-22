// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"errors"
	"net"
	"net/http"
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

type httpServerProvider struct {
	Config
	httpServer func(context.Context, http.Handler) (*http.Server, error)
}

func (hsp httpServerProvider) HttpServer(ctx context.Context, h http.Handler) (*http.Server, error) {
	return hsp.httpServer(ctx, h)
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
				return NewApi("", ""), nil
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

		t.Run("if it fails to initialize http server", func(t *testing.T) {
			b := Builder(func(ctx context.Context, cfg httpServerProvider) (*Api, error) {
				return NewApi("", ""), nil
			})

			serverErr := errors.New("failed to create server")
			hsp := httpServerProvider{
				httpServer: func(ctx context.Context, h http.Handler) (*http.Server, error) {
					return nil, serverErr
				},
			}

			_, err := b.Build(context.Background(), hsp)
			if !assert.ErrorIs(t, err, serverErr) {
				return
			}
		})
	})
}
