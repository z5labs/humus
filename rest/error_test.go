// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/z5labs/humus/humuspb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type noopHandler struct{}

func (h noopHandler) Handle(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

type errorEndpointHandler struct {
	err error
}

func (h errorEndpointHandler) Handle(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	return nil, h.err
}

func TestErrHandler_HandleError(t *testing.T) {
	t.Run("will return HTTP 500 with no body", func(t *testing.T) {
		t.Run("if it fails to marshal the status to proto", func(t *testing.T) {
			marshalErr := errors.New("failed to marshal")
			h := errHandler{
				marshal: func(m proto.Message) ([]byte, error) {
					return nil, marshalErr
				},
			}

			w := httptest.NewRecorder()

			h.HandleError(context.Background(), w, errors.New("request failed"))

			resp := w.Result()
			if !assert.Equal(t, http.StatusInternalServerError, resp.StatusCode) {
				return
			}
		})
	})

	t.Run("will return HTTP 500 with a body", func(t *testing.T) {
		t.Run("if a non humuspb.Status error is returned by the endpoint", func(t *testing.T) {
			h := errorEndpointHandler{
				err: errors.New("request failed"),
			}

			app := New(
				ListenOn(0),
				RegisterEndpoint(NewEndpoint(
					http.MethodGet,
					"/",
					h,
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

				req, err := http.NewRequestWithContext(
					egctx,
					http.MethodGet,
					fmt.Sprintf("http://%s", addr),
					nil,
				)
				if err != nil {
					return err
				}

				resp, err := http.DefaultClient.Do(req)
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
			defer resp.Body.Close()

			if !assert.Equal(t, http.StatusInternalServerError, resp.StatusCode) {
				return
			}

			headers := resp.Header
			if !assert.Contains(t, headers, "Content-Type") {
				return
			}
			if !assert.Equal(t, ProtobufContentType, headers.Get("Content-Type")) {
				return
			}

			b, err := io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}

			var status humuspb.Status
			err = proto.Unmarshal(b, &status)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, humuspb.Code_INTERNAL, status.Code) {
				return
			}
		})

		t.Run("if the status code is not recognized by humuspb", func(t *testing.T) {
			h := errorEndpointHandler{
				err: &humuspb.Status{Code: -1},
			}

			app := New(
				ListenOn(0),
				RegisterEndpoint(NewEndpoint(
					http.MethodGet,
					"/",
					h,
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

				req, err := http.NewRequestWithContext(
					egctx,
					http.MethodGet,
					fmt.Sprintf("http://%s", addr),
					nil,
				)
				if err != nil {
					return err
				}

				resp, err := http.DefaultClient.Do(req)
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
			defer resp.Body.Close()

			if !assert.Equal(t, http.StatusInternalServerError, resp.StatusCode) {
				return
			}

			headers := resp.Header
			if !assert.Contains(t, headers, "Content-Type") {
				return
			}
			if !assert.Equal(t, ProtobufContentType, headers.Get("Content-Type")) {
				return
			}

			b, err := io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}

			var status humuspb.Status
			err = proto.Unmarshal(b, &status)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, humuspb.Code(-1), status.Code) {
				return
			}
		})
	})

	t.Run("will return HTTP 400 with a body", func(t *testing.T) {
		t.Run("if a InvalidHeaderError is returned", func(t *testing.T) {
			h := noopHandler{}

			app := New(
				ListenOn(0),
				RegisterEndpoint(NewEndpoint(
					http.MethodGet,
					"/",
					h,
					Headers(
						Header{
							Name:    "id",
							Pattern: "^[a-zA-Z]$",
						},
					),
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

				req, err := http.NewRequestWithContext(
					egctx,
					http.MethodGet,
					fmt.Sprintf("http://%s", addr),
					nil,
				)
				if err != nil {
					return err
				}

				req.Header.Set("id", "123")

				resp, err := http.DefaultClient.Do(req)
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
			defer resp.Body.Close()

			if !assert.Equal(t, http.StatusBadRequest, resp.StatusCode) {
				return
			}

			headers := resp.Header
			if !assert.Contains(t, headers, "Content-Type") {
				return
			}
			if !assert.Equal(t, ProtobufContentType, headers.Get("Content-Type")) {
				return
			}

			b, err := io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}

			var status humuspb.Status
			err = proto.Unmarshal(b, &status)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, humuspb.Code_INVALID_ARGUMENT, status.Code) {
				return
			}
		})

		t.Run("if a InvalidQueryParamError is returned", func(t *testing.T) {
			h := noopHandler{}

			app := New(
				ListenOn(0),
				RegisterEndpoint(NewEndpoint(
					http.MethodGet,
					"/",
					h,
					QueryParams(
						QueryParam{
							Name:    "id",
							Pattern: "^[a-zA-Z]$",
						},
					),
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

				req, err := http.NewRequestWithContext(
					egctx,
					http.MethodGet,
					fmt.Sprintf("http://%s?id=123", addr),
					nil,
				)
				if err != nil {
					return err
				}

				resp, err := http.DefaultClient.Do(req)
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
			defer resp.Body.Close()

			if !assert.Equal(t, http.StatusBadRequest, resp.StatusCode) {
				return
			}

			headers := resp.Header
			if !assert.Contains(t, headers, "Content-Type") {
				return
			}
			if !assert.Equal(t, ProtobufContentType, headers.Get("Content-Type")) {
				return
			}

			b, err := io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}

			var status humuspb.Status
			err = proto.Unmarshal(b, &status)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, humuspb.Code_INVALID_ARGUMENT, status.Code) {
				return
			}
		})
	})
}
