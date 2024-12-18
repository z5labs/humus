// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

type acceptFunc func() (net.Conn, error)

func (f acceptFunc) Accept() (net.Conn, error) {
	return f()
}

func (acceptFunc) Close() error {
	return nil
}

func (acceptFunc) Addr() net.Addr {
	return nil
}

func TestApp_Run(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if the given net.Listener fails to accept a connection", func(t *testing.T) {
			acceptErr := errors.New("failed to accept conn")
			ls := acceptFunc(func() (net.Conn, error) {
				return nil, acceptErr
			})

			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

			a := NewApp(ls, h)
			err := a.Run(context.Background())
			if !assert.ErrorIs(t, err, acceptErr) {
				return
			}
		})
	})

	t.Run("will not return an error", func(t *testing.T) {
		t.Run("if the context is cancelled before running", func(t *testing.T) {
			ls, err := net.Listen("tcp", ":0")
			if !assert.Nil(t, err) {
				return
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

			a := NewApp(ls, h)

			err = a.Run(ctx)
			if !assert.Nil(t, err) {
				return
			}
		})

		t.Run("if the context is cancelled while running", func(t *testing.T) {
			ls, err := net.Listen("tcp", ":0")
			if !assert.Nil(t, err) {
				return
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer cancel()

				w.WriteHeader(http.StatusOK)
			})

			a := NewApp(ls, h)

			errCh := make(chan error, 1)
			go func() {
				defer close(errCh)
				errCh <- a.Run(ctx)
			}()

			resp, err := http.DefaultClient.Get(fmt.Sprintf("http://%s/", ls.Addr()))
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, http.StatusOK, resp.StatusCode) {
				return
			}

			err = <-errCh
			if !assert.Nil(t, err) {
				return
			}
		})
	})
}
