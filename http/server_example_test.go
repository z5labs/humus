// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
)

func Example() {
	buildHandler := app.BuilderFunc[http.Handler](func(ctx context.Context) (http.Handler, error) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "hello from http handler")
		})
		return handler, nil
	})

	ls, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}

	builder := Build(
		NewServer(
			config.ReaderOf(ls),
		),
		buildHandler,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneCh := make(chan struct{}, 1)
	go func() {
		defer close(doneCh)

		app.LogError(
			slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
				AddSource: true,
			}),
			app.Run(ctx, builder),
		)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ls.Addr().String(), nil)
	if err != nil {
		fmt.Println(err)
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	_, err = io.Copy(os.Stdout, resp.Body)
	if err != nil {
		fmt.Println(err)
		return
	}

	cancel()
	<-doneCh

	// Output:
	// hello from http handler
}

func ExampleNewTCPListener() {
	listener := NewTCPListener(
		Addr(config.ReaderOf(":8080")),
	)

	ctx := context.Background()
	val, err := listener.Read(ctx)
	if err != nil {
		fmt.Println(err)
		return
	}

	ln, ok := val.Value()
	if !ok {
		fmt.Println("no listener value")
		return
	}
	defer ln.Close()

	fmt.Println("Listening on:", ln.Addr().Network())
	// Output: Listening on: tcp
}

func ExampleTCPListener_Read() {
	listener := NewTCPListener(Addr(config.ReaderOf(":0")))

	ctx := context.Background()
	val, err := listener.Read(ctx)
	if err != nil {
		fmt.Println(err)
		return
	}

	ln, ok := val.Value()
	if !ok {
		fmt.Println("no listener value")
		return
	}
	defer ln.Close()

	fmt.Println("Network:", ln.Addr().Network())
	// Output: Network: tcp
}

func ExampleTLSListener() {
	baseLn, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer baseLn.Close()

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	tlsListener := TLSListener(
		config.ReaderOf(baseLn),
		config.ReaderOf(tlsConfig),
	)

	ctx := context.Background()
	val, err := tlsListener.Read(ctx)
	if err != nil {
		fmt.Println(err)
		return
	}

	ln, ok := val.Value()
	if !ok {
		fmt.Println("no listener value")
		return
	}

	fmt.Println("TLS listener created:", ln != nil)
	// Output: TLS listener created: true
}

func ExampleNewServer() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer ln.Close()

	server := NewServer(
		config.ReaderOf(ln),
		ReadTimeout(config.ReaderOf(30*time.Second)),
		WriteTimeout(config.ReaderOf(30*time.Second)),
		IdleTimeout(config.ReaderOf(60*time.Second)),
	)

	ctx := context.Background()
	listener := config.Must(ctx, server.Listener)

	fmt.Println("Listener address network:", listener.Addr().Network())
	// Output: Listener address network: tcp
}

func ExampleNewServer_withAllOptions() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer ln.Close()

	server := NewServer(
		config.ReaderOf(ln),
		DisableGeneralOptionsHandler(config.ReaderOf(true)),
		ReadTimeout(config.ReaderOf(30*time.Second)),
		ReadHeaderTimeout(config.ReaderOf(10*time.Second)),
		WriteTimeout(config.ReaderOf(30*time.Second)),
		IdleTimeout(config.ReaderOf(60*time.Second)),
		MaxHeaderBytes(config.ReaderOf(2097152)),
	)

	ctx := context.Background()
	fmt.Println("Read timeout:", config.Must(ctx, server.ReadTimeout))
	// Output: Read timeout: 30s
}

func ExampleBuild() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer ln.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "hello")
	})

	server := NewServer(config.ReaderOf(ln))

	builder := Build(
		server,
		app.BuilderFunc[http.Handler](func(ctx context.Context) (http.Handler, error) {
			return handler, nil
		}),
	)

	ctx := context.Background()
	httpApp, err := builder.Build(ctx)
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println("App created successfully")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go httpApp.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Output: App created successfully
}

func ExampleApp_Run() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "hello")
	})

	server := NewServer(config.ReaderOf(ln))
	builder := Build(
		server,
		app.BuilderFunc[http.Handler](func(ctx context.Context) (http.Handler, error) {
			return handler, nil
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	httpApp, err := builder.Build(ctx)
	if err != nil {
		fmt.Println(err)
		return
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpApp.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get("http://" + ln.Addr().String())
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Print(string(body))

	cancel()
	<-errCh

	// Output: hello
}

func ExampleDisableGeneralOptionsHandler() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer ln.Close()

	server := NewServer(
		config.ReaderOf(ln),
		DisableGeneralOptionsHandler(config.ReaderOf(true)),
	)

	ctx := context.Background()
	disabled := config.Must(ctx, server.DisableGeneralOptionsHandler)

	fmt.Println("OPTIONS handler disabled:", disabled)
	// Output: OPTIONS handler disabled: true
}

func ExampleReadTimeout() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer ln.Close()

	server := NewServer(
		config.ReaderOf(ln),
		ReadTimeout(config.ReaderOf(15*time.Second)),
	)

	ctx := context.Background()
	timeout := config.Must(ctx, server.ReadTimeout)

	fmt.Println("Read timeout:", timeout)
	// Output: Read timeout: 15s
}

func ExampleWriteTimeout() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer ln.Close()

	server := NewServer(
		config.ReaderOf(ln),
		WriteTimeout(config.ReaderOf(20*time.Second)),
	)

	ctx := context.Background()
	timeout := config.Must(ctx, server.WriteTimeout)

	fmt.Println("Write timeout:", timeout)
	// Output: Write timeout: 20s
}

func ExampleIdleTimeout() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer ln.Close()

	server := NewServer(
		config.ReaderOf(ln),
		IdleTimeout(config.ReaderOf(90*time.Second)),
	)

	ctx := context.Background()
	timeout := config.Must(ctx, server.IdleTimeout)

	fmt.Println("Idle timeout:", timeout)
	// Output: Idle timeout: 1m30s
}

func ExampleMaxHeaderBytes() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer ln.Close()

	server := NewServer(
		config.ReaderOf(ln),
		MaxHeaderBytes(config.ReaderOf(4096)),
	)

	ctx := context.Background()
	maxBytes := config.Must(ctx, server.MaxHeaderBytes)

	fmt.Println("Max header bytes:", maxBytes)
	// Output: Max header bytes: 4096
}

