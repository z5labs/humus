// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/pkg/app"
	"github.com/z5labs/bedrock/pkg/health"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestApp(t *testing.T) {
	t.Run("will return unhealthy readiness", func(t *testing.T) {
		t.Run("if the readiness Metric is unhealthy", func(t *testing.T) {
			var m health.Binary
			m.Toggle()

			app := New(
				ListenOn(0),
				Readiness(&m),
			)

			addrCh := make(chan net.Addr)
			app.listen = func(network, addr string) (net.Listener, error) {
				defer close(addrCh)
				ls, err := net.Listen(network, addr)
				if err != nil {
					return nil, err
				}
				addrCh <- ls.Addr()
				return ls, nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			eg, egctx := errgroup.WithContext(ctx)
			eg.Go(func() error {
				return app.Run(egctx)
			})

			respCh := make(chan *http.Response, 1)
			eg.Go(func() error {
				defer cancel()
				defer close(respCh)

				addr := <-addrCh
				if addr == nil {
					return errors.New("received nil net.Addr")
				}

				resp, err := http.Get(fmt.Sprintf("http://%s/health/readiness", addr))
				if err != nil {
					return err
				}

				select {
				case <-egctx.Done():
				case respCh <- resp:
				}
				return nil
			})

			err := eg.Wait()
			if !assert.Nil(t, err) {
				return
			}

			resp := <-respCh
			if !assert.NotNil(t, resp) {
				return
			}
			if !assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode) {
				return
			}
		})
	})

	t.Run("will return healthy readiness", func(t *testing.T) {
		t.Run("if the readiness Metric is healthy", func(t *testing.T) {
			var m health.Binary

			app := New(
				ListenOn(0),
				Readiness(&m),
			)

			addrCh := make(chan net.Addr)
			app.listen = func(network, addr string) (net.Listener, error) {
				defer close(addrCh)
				ls, err := net.Listen(network, addr)
				if err != nil {
					return nil, err
				}
				addrCh <- ls.Addr()
				return ls, nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			eg, egctx := errgroup.WithContext(ctx)
			eg.Go(func() error {
				return app.Run(egctx)
			})

			respCh := make(chan *http.Response, 1)
			eg.Go(func() error {
				defer cancel()
				defer close(respCh)

				addr := <-addrCh
				if addr == nil {
					return errors.New("received nil net.Addr")
				}

				resp, err := http.Get(fmt.Sprintf("http://%s/health/liveness", addr))
				if err != nil {
					return err
				}

				select {
				case <-egctx.Done():
				case respCh <- resp:
				}
				return nil
			})

			err := eg.Wait()
			if !assert.Nil(t, err) {
				return
			}

			resp := <-respCh
			if !assert.NotNil(t, resp) {
				return
			}
			if !assert.Equal(t, http.StatusOK, resp.StatusCode) {
				return
			}
		})
	})

	t.Run("will return unhealthy liveness", func(t *testing.T) {
		t.Run("if the liveness Metric is unhealthy", func(t *testing.T) {
			var m health.Binary
			m.Toggle()

			app := New(
				ListenOn(0),
				Liveness(&m),
			)

			addrCh := make(chan net.Addr)
			app.listen = func(network, addr string) (net.Listener, error) {
				defer close(addrCh)
				ls, err := net.Listen(network, addr)
				if err != nil {
					return nil, err
				}
				addrCh <- ls.Addr()
				return ls, nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			eg, egctx := errgroup.WithContext(ctx)
			eg.Go(func() error {
				return app.Run(egctx)
			})

			respCh := make(chan *http.Response, 1)
			eg.Go(func() error {
				defer cancel()
				defer close(respCh)

				addr := <-addrCh
				if addr == nil {
					return errors.New("received nil net.Addr")
				}

				resp, err := http.Get(fmt.Sprintf("http://%s/health/liveness", addr))
				if err != nil {
					return err
				}

				select {
				case <-egctx.Done():
				case respCh <- resp:
				}
				return nil
			})

			err := eg.Wait()
			if !assert.Nil(t, err) {
				return
			}

			resp := <-respCh
			if !assert.NotNil(t, resp) {
				return
			}
			if !assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode) {
				return
			}
		})
	})

	t.Run("will return healthy liveness", func(t *testing.T) {
		t.Run("if the liveness Metric is healthy", func(t *testing.T) {
			var m health.Binary

			app := New(
				ListenOn(0),
				Liveness(&m),
			)

			addrCh := make(chan net.Addr)
			app.listen = func(network, addr string) (net.Listener, error) {
				defer close(addrCh)
				ls, err := net.Listen(network, addr)
				if err != nil {
					return nil, err
				}
				addrCh <- ls.Addr()
				return ls, nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			eg, egctx := errgroup.WithContext(ctx)
			eg.Go(func() error {
				return app.Run(egctx)
			})

			respCh := make(chan *http.Response, 1)
			eg.Go(func() error {
				defer cancel()
				defer close(respCh)

				addr := <-addrCh
				if addr == nil {
					return errors.New("received nil net.Addr")
				}

				resp, err := http.Get(fmt.Sprintf("http://%s/health/readiness", addr))
				if err != nil {
					return err
				}

				select {
				case <-egctx.Done():
				case respCh <- resp:
				}
				return nil
			})

			err := eg.Wait()
			if !assert.Nil(t, err) {
				return
			}

			resp := <-respCh
			if !assert.NotNil(t, resp) {
				return
			}
			if !assert.Equal(t, http.StatusOK, resp.StatusCode) {
				return
			}
		})
	})
}

type contextCancelHandler struct {
	cancel func()
}

func (h contextCancelHandler) Handle(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	h.cancel()
	return new(emptypb.Empty), nil
}

func TestApp_Run(t *testing.T) {
	t.Run("will not return an error", func(t *testing.T) {
		t.Run("if the context.Context is cancelled while handling requests", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())

			app := New(
				ListenOn(0),
				RegisterEndpoint(NewProtoEndpoint(
					http.MethodGet,
					"/",
					contextCancelHandler{
						cancel: cancel,
					},
				)),
			)

			addrCh := make(chan net.Addr)
			app.listen = func(network, addr string) (net.Listener, error) {
				defer close(addrCh)
				ls, err := net.Listen(network, addr)
				if err != nil {
					return nil, err
				}
				addrCh <- ls.Addr()
				return ls, nil
			}

			eg, egctx := errgroup.WithContext(ctx)
			eg.Go(func() error {
				return app.Run(egctx)
			})
			eg.Go(func() error {
				addr := <-addrCh
				if addr == nil {
					return errors.New("received nil net.Addr")
				}

				_, err := http.Get(fmt.Sprintf("http://%s", addr))
				return err
			})

			err := eg.Wait()
			if !assert.Nil(t, err) {
				return
			}
		})
	})

	t.Run("will return an error", func(t *testing.T) {
		t.Run("if it fails to create a net.Listener", func(t *testing.T) {
			app := New(ListenOn(0))

			listenErr := errors.New("failed to listen")
			app.listen = func(network, addr string) (net.Listener, error) {
				return nil, listenErr
			}

			err := app.Run(context.Background())
			if !assert.Equal(t, listenErr, err) {
				return
			}
		})

		t.Run("if a post run hooks returns an error", func(t *testing.T) {
			hookErr := errors.New("failed to run hook")
			hook := app.LifecycleHookFunc(func(ctx context.Context) error {
				return hookErr
			})

			app := New(
				ListenOn(0),
				PostRun(hook),
			)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := app.Run(ctx)
			if !assert.ErrorIs(t, err, hookErr) {
				return
			}
		})

		t.Run("if a post run hook panics", func(t *testing.T) {
			hook := app.LifecycleHookFunc(func(ctx context.Context) error {
				panic("hello world")
				return nil
			})

			app := New(
				ListenOn(0),
				PostRun(hook),
			)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := app.Run(ctx)

			var perr bedrock.PanicError
			if !assert.ErrorAs(t, err, &perr) {
				return
			}
			if !assert.Equal(t, "hello world", perr.Value) {
				return
			}
		})
	})
}
