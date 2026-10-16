// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"fmt"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
)

// The docs site quotes the code in this file, and TestDocs_code_samples
// checks that every quote still matches it.

// exampleTB stands in for the *testing.T an Example does not have. In a
// test, pass t to bedrocktest.NewProcess instead.
type exampleTB struct {
	testing.TB
	cleanups []func()
}

func (*exampleTB) Helper() {}

func (*exampleTB) Errorf(format string, args ...any) {
	fmt.Printf("test failed: "+format+"\n", args...)
}

func (tb *exampleTB) Cleanup(f func()) {
	tb.cleanups = append(tb.cleanups, f)
}

func (tb *exampleTB) runCleanups() {
	for i := len(tb.cleanups) - 1; i >= 0; i-- {
		tb.cleanups[i]()
	}
}

// server is a Service that keeps serving after Serve's context is done and
// stops only when Shutdown is called, as an HTTP or gRPC server does.
type server struct {
	out     io.Writer
	ready   chan struct{}
	stopped chan struct{}
}

func newServer(out io.Writer) *server {
	return &server{
		out:     out,
		ready:   make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func (s *server) Serve(ctx context.Context) error {
	_, err := fmt.Fprintln(s.out, "serve: serving")
	close(s.ready)
	if err != nil {
		return err
	}
	<-s.stopped
	return nil
}

func (s *server) Shutdown(ctx context.Context) error {
	close(s.stopped)
	_, err := fmt.Fprintln(s.out, "shutdown: stopped serving")
	return err
}

// Init is the Init of the App in Example_main. It registers the release of
// each dependency with OnStop as soon as it has built it, so that a failure
// later in Init still releases everything built before it.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	out := s.Process.Stdout
	s.OnStop(func(context.Context) error {
		_, err := fmt.Fprintln(out, "on stop: close the cache")
		return err
	})
	s.OnStop(func(context.Context) error {
		_, err := fmt.Fprintln(out, "on stop: close the queue")
		return err
	})

	if _, err := config.Env(s.Process.Environment, "DATABASE_URL").Read(ctx); err != nil {
		return nil, err // exit 78: init: config DATABASE_URL: not found
	}
	return []humus.Service{newServer(out)}, nil
}

// The main function of a humus App is one call to bedrock.Main. This example
// has no Output comment, so it is compiled but never run: running it would
// exit the test binary.
func Example_main() {
	bedrock.Main(humus.App{
		Host:       humus.OSHost(),
		DrainDelay: 5 * time.Second, // humus has no default
		Init:       Init,
	})
}

// An App runs the Services its Init returns until a signal arrives, then
// shuts them down and runs the functions Init registered with OnStop. Here
// the App runs on a bedrocktest Process, which delivers a fake SIGTERM.
func ExampleApp() {
	srv := newServer(nil)
	app := humus.App{
		Host: humus.OSHost(), // the Service never uses it
		Init: func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			srv.out = s.Process.Stdout
			s.OnStop(func(context.Context) error {
				_, err := fmt.Fprintln(s.Process.Stdout, "on stop: released")
				return err
			})
			return []humus.Service{srv}, nil
		},
	}

	t := new(exampleTB)
	defer t.runCleanups()

	fp := bedrocktest.NewProcess(t)
	statusCh := make(chan bedrock.ExitStatus, 1)
	go func() {
		statusCh <- app.Run(context.Background(), fp.Process())
	}()

	// Wait until the Service is serving, then send a fake SIGTERM.
	<-srv.ready
	fp.Signal(syscall.SIGTERM)
	status := <-statusCh

	fmt.Print(fp.Stdout())
	fmt.Println("exit code:", status.Code())
	fmt.Println("cause:", status.Cause())
	// Output:
	// serve: serving
	// shutdown: stopped serving
	// on stop: released
	// exit code: 0
	// cause: terminated signal received
}

// OnStop works like t.Cleanup: the functions run in reverse order of
// registration, and they run when Init fails partway through too. A
// setting Init cannot read exits ExitConfig.
func ExampleSetup_OnStop() {
	app := humus.App{
		Host: humus.OSHost(),
		Init: Init,
	}

	t := new(exampleTB)
	defer t.runCleanups()

	fp := bedrocktest.NewProcess(t) // DATABASE_URL is not set
	status := app.Run(context.Background(), fp.Process())

	fmt.Print(fp.Stdout())
	fmt.Print("stderr: ", fp.Stderr())
	fmt.Println("exit code:", status.Code())
	// Output:
	// on stop: close the queue
	// on stop: close the cache
	// stderr: init: config DATABASE_URL: not found
	// exit code: 78
}
