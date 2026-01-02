// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
)

func TestTCPListener_Read(t *testing.T) {
	t.Run("will use default address when none provided", func(t *testing.T) {
		tcpLn := NewTCPListener()

		val, err := tcpLn.Read(context.Background())
		require.NoError(t, err)

		ln, ok := val.Value()
		require.True(t, ok)
		require.NotNil(t, ln)
		defer ln.Close()

		addr := ln.Addr().String()
		require.Contains(t, addr, ":8080")
	})

	t.Run("will use custom address when provided", func(t *testing.T) {
		tcpLn := NewTCPListener(Addr(config.ReaderOf(":9090")))

		val, err := tcpLn.Read(context.Background())
		require.NoError(t, err)

		ln, ok := val.Value()
		require.True(t, ok)
		require.NotNil(t, ln)
		defer ln.Close()

		addr := ln.Addr().String()
		require.Contains(t, addr, ":9090")
	})

	t.Run("will return error for invalid address", func(t *testing.T) {
		tcpLn := NewTCPListener(Addr(config.ReaderOf("invalid-address")))

		_, err := tcpLn.Read(context.Background())
		require.Error(t, err)
	})
}

func TestAddr(t *testing.T) {
	t.Run("will set address on TCPListener", func(t *testing.T) {
		addr := ":7070"
		tcpLn := NewTCPListener(Addr(config.ReaderOf(addr)))

		val, err := tcpLn.Read(context.Background())
		require.NoError(t, err)

		ln, ok := val.Value()
		require.True(t, ok)
		defer ln.Close()

		require.Contains(t, ln.Addr().String(), addr)
	})
}

func TestTLSListener(t *testing.T) {
	t.Run("will wrap base listener with TLS", func(t *testing.T) {
		baseLn, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer baseLn.Close()

		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}

		tlsLnReader := TLSListener(
			config.ReaderOf(baseLn),
			config.ReaderOf(tlsCfg),
		)

		val, err := tlsLnReader.Read(context.Background())
		require.NoError(t, err)

		ln, ok := val.Value()
		require.True(t, ok)
		require.NotNil(t, ln)
	})
}

func TestNewServer(t *testing.T) {
	t.Run("will create server with default values", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(config.ReaderOf(ln))

		require.NotNil(t, srv.Listener)
		require.NotNil(t, srv.DisableGeneralOptionsHandler)
		require.NotNil(t, srv.ReadTimeout)
		require.NotNil(t, srv.ReadHeaderTimeout)
		require.NotNil(t, srv.WriteTimeout)
		require.NotNil(t, srv.IdleTimeout)
		require.NotNil(t, srv.MaxHeaderBytes)
	})

	t.Run("will apply options", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(
			config.ReaderOf(ln),
			DisableGeneralOptionsHandler(config.ReaderOf(true)),
			ReadTimeout(config.ReaderOf(10*time.Second)),
			ReadHeaderTimeout(config.ReaderOf(5*time.Second)),
			WriteTimeout(config.ReaderOf(15*time.Second)),
			IdleTimeout(config.ReaderOf(60*time.Second)),
			MaxHeaderBytes(config.ReaderOf(2048)),
		)

		ctx := context.Background()
		require.Equal(t, true, config.Must(ctx, srv.DisableGeneralOptionsHandler))
		require.Equal(t, 10*time.Second, config.Must(ctx, srv.ReadTimeout))
		require.Equal(t, 5*time.Second, config.Must(ctx, srv.ReadHeaderTimeout))
		require.Equal(t, 15*time.Second, config.Must(ctx, srv.WriteTimeout))
		require.Equal(t, 60*time.Second, config.Must(ctx, srv.IdleTimeout))
		require.Equal(t, 2048, config.Must(ctx, srv.MaxHeaderBytes))
	})
}

func TestDisableGeneralOptionsHandler(t *testing.T) {
	t.Run("will set DisableGeneralOptionsHandler option", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(
			config.ReaderOf(ln),
			DisableGeneralOptionsHandler(config.ReaderOf(true)),
		)

		ctx := context.Background()
		require.Equal(t, true, config.Must(ctx, srv.DisableGeneralOptionsHandler))
	})
}

func TestReadTimeout(t *testing.T) {
	t.Run("will set ReadTimeout option", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(
			config.ReaderOf(ln),
			ReadTimeout(config.ReaderOf(20*time.Second)),
		)

		ctx := context.Background()
		require.Equal(t, 20*time.Second, config.Must(ctx, srv.ReadTimeout))
	})
}

func TestReadHeaderTimeout(t *testing.T) {
	t.Run("will set ReadHeaderTimeout option", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(
			config.ReaderOf(ln),
			ReadHeaderTimeout(config.ReaderOf(3*time.Second)),
		)

		ctx := context.Background()
		require.Equal(t, 3*time.Second, config.Must(ctx, srv.ReadHeaderTimeout))
	})
}

func TestWriteTimeout(t *testing.T) {
	t.Run("will set WriteTimeout option", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(
			config.ReaderOf(ln),
			WriteTimeout(config.ReaderOf(25*time.Second)),
		)

		ctx := context.Background()
		require.Equal(t, 25*time.Second, config.Must(ctx, srv.WriteTimeout))
	})
}

func TestIdleTimeout(t *testing.T) {
	t.Run("will set IdleTimeout option", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(
			config.ReaderOf(ln),
			IdleTimeout(config.ReaderOf(90*time.Second)),
		)

		ctx := context.Background()
		require.Equal(t, 90*time.Second, config.Must(ctx, srv.IdleTimeout))
	})
}

func TestMaxHeaderBytes(t *testing.T) {
	t.Run("will set MaxHeaderBytes option", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(
			config.ReaderOf(ln),
			MaxHeaderBytes(config.ReaderOf(4096)),
		)

		ctx := context.Background()
		require.Equal(t, 4096, config.Must(ctx, srv.MaxHeaderBytes))
	})
}

func TestBuild(t *testing.T) {
	t.Run("will build http server app with defaults", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(config.ReaderOf(ln))

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		builder := Build(srv, app.BuilderFunc[http.Handler](func(ctx context.Context) (http.Handler, error) {
			return handler, nil
		}))

		ctx := context.Background()
		httpApp, err := builder.Build(ctx)
		require.NoError(t, err)
		require.NotNil(t, httpApp)
		require.NotNil(t, httpApp.ls)
		require.NotNil(t, httpApp.srv)
		require.NotNil(t, httpApp.srv.Handler)
		require.Equal(t, false, httpApp.srv.DisableGeneralOptionsHandler)
		require.Equal(t, 5*time.Second, httpApp.srv.ReadTimeout)
		require.Equal(t, 2*time.Second, httpApp.srv.ReadHeaderTimeout)
		require.Equal(t, 10*time.Second, httpApp.srv.WriteTimeout)
		require.Equal(t, 120*time.Second, httpApp.srv.IdleTimeout)
		require.Equal(t, 1048576, httpApp.srv.MaxHeaderBytes)
	})

	t.Run("will build http server app with custom values", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(
			config.ReaderOf(ln),
			DisableGeneralOptionsHandler(config.ReaderOf(true)),
			ReadTimeout(config.ReaderOf(30*time.Second)),
			ReadHeaderTimeout(config.ReaderOf(10*time.Second)),
			WriteTimeout(config.ReaderOf(20*time.Second)),
			IdleTimeout(config.ReaderOf(180*time.Second)),
			MaxHeaderBytes(config.ReaderOf(2097152)),
		)

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		builder := Build(srv, app.BuilderFunc[http.Handler](func(ctx context.Context) (http.Handler, error) {
			return handler, nil
		}))

		ctx := context.Background()
		httpApp, err := builder.Build(ctx)
		require.NoError(t, err)
		require.NotNil(t, httpApp)
		require.Equal(t, true, httpApp.srv.DisableGeneralOptionsHandler)
		require.Equal(t, 30*time.Second, httpApp.srv.ReadTimeout)
		require.Equal(t, 10*time.Second, httpApp.srv.ReadHeaderTimeout)
		require.Equal(t, 20*time.Second, httpApp.srv.WriteTimeout)
		require.Equal(t, 180*time.Second, httpApp.srv.IdleTimeout)
		require.Equal(t, 2097152, httpApp.srv.MaxHeaderBytes)
	})

	t.Run("will return error when handler builder fails", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer ln.Close()

		srv := NewServer(config.ReaderOf(ln))

		buildErr := errors.New("handler build failed")
		builder := Build(srv, app.BuilderFunc[http.Handler](func(ctx context.Context) (http.Handler, error) {
			return nil, buildErr
		}))

		ctx := context.Background()
		_, err = builder.Build(ctx)
		require.ErrorIs(t, err, buildErr)
	})
}

func TestApp_Run(t *testing.T) {
	t.Run("will start and shutdown server gracefully", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		srv := NewServer(config.ReaderOf(ln))
		builder := Build(srv, app.BuilderFunc[http.Handler](func(ctx context.Context) (http.Handler, error) {
			return handler, nil
		}))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		httpApp, err := builder.Build(ctx)
		require.NoError(t, err)

		errCh := make(chan error, 1)
		go func() {
			errCh <- httpApp.Run(ctx)
		}()

		time.Sleep(100 * time.Millisecond)

		reqCtx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer reqCancel()

		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+ln.Addr().String(), nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()

		cancel()

		runErr := <-errCh
		require.NoError(t, runErr)
	})

	t.Run("will return error when server fails to serve", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		ln.Close()

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		srv := NewServer(config.ReaderOf(ln))
		builder := Build(srv, app.BuilderFunc[http.Handler](func(ctx context.Context) (http.Handler, error) {
			return handler, nil
		}))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		httpApp, err := builder.Build(ctx)
		require.NoError(t, err)

		err = httpApp.Run(ctx)
		require.Error(t, err)
	})
}
