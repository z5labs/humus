// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
)

// Exit codes of an App. Every other failure is told apart by the cause of
// the bedrock.ExitStatus, not by its code.
const (
	// ExitFailure is the code of an App whose Init, a Service, a
	// Service's Shutdown or an OnStop function failed, or whose shutdown
	// ran past the grace period.
	ExitFailure = 1

	// ExitConfig is the code of an App with a setting that is missing or
	// malformed, including a config value read by Init: EX_CONFIG from
	// sysexits.h.
	ExitConfig = 78
)

// Service is a long-running part of an App: an HTTP server, a gRPC server,
// a queue consumer or a job.
type Service interface {
	// Serve runs the Service until Shutdown stops it, until it fails, or,
	// for a Service that finishes on its own such as a job, until it is
	// done. It returns nil when Shutdown stopped it or it finished.
	//
	// ctx is done as soon as the App starts shutting down, before the
	// App's DrainDelay. A Service may stop on it, as a job or a queue
	// consumer does, or keep serving until Shutdown, as an HTTP, gRPC or
	// health Service does.
	Serve(ctx context.Context) error

	// Shutdown stops the Service gracefully. The App calls it on every
	// Service after the App's DrainDelay, including one whose Serve has
	// already returned. If ctx is done before the Service has stopped,
	// Shutdown stops it at once and returns an error that wraps ctx's
	// error.
	Shutdown(ctx context.Context) error
}

// App is a bedrock.App that runs Services with humus's lifecycle. Host and
// Init are required.
type App struct {
	// Host is the network and filesystem the App runs on: OSHost() in
	// main, and a Host the test controls in a test.
	Host Host

	// DrainDelay is the pause between shutdown starting and Shutdown being
	// called on the Services: Serve's context is done at once, and Shutdown
	// is called DrainDelay later. It gives a load balancer, such as a
	// Kubernetes Service removing the pod from its endpoints, time to stop
	// sending new connections before the Services stop accepting them. The
	// grace period starts after it.
	//
	// The zero value means no pause, and so does a negative one. humus reads
	// no variable for DrainDelay and has no default; a runtime package's App
	// sets its own.
	DrainDelay time.Duration

	// Init builds the App's dependencies and returns the Services to run.
	// Its ctx is done when the App receives an interrupt or SIGTERM, so a
	// slow Init, such as one retrying a database connection, is cancelled
	// by a signal.
	Init func(ctx context.Context, s *Setup) ([]Service, error)
}

// Setup is what Init is given.
type Setup struct {
	// Process is the bedrock.Process the App is run with.
	Process bedrock.Process

	// Host is the App's Host.
	Host Host

	mu     sync.Mutex
	onStop []func(context.Context) error
}

// OnStop registers f to run once every Service has stopped, or once Init
// has failed. Functions run in reverse order of registration, like deferred
// calls, within the grace period: the context each is given is done when
// the grace period runs out. OnStop is safe for concurrent use. It panics if
// f is nil.
func (s *Setup) OnStop(f func(context.Context) error) {
	if f == nil {
		panic("humus: Setup.OnStop called with a nil function")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onStop = append(s.onStop, f)
}

// stopFuncs returns the registered OnStop functions in the order they run:
// the reverse of their registration.
func (s *Setup) stopFuncs() []func(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs := slices.Clone(s.onStop)
	slices.Reverse(fs)
	return fs
}

// GracePeriodError is the cause of an App whose Services or OnStop
// functions were still running when the grace period ran out. The App
// exits ExitFailure.
type GracePeriodError struct {
	// GracePeriod is the grace period that ran out.
	GracePeriod time.Duration

	// Err is every error the shutdown returned, joined.
	Err error
}

// Error implements the error interface.
func (e *GracePeriodError) Error() string {
	msg := fmt.Sprintf("shutdown ran past the grace period of %s", e.GracePeriod)
	if e.Err == nil {
		return msg
	}
	return msg + ": " + e.Err.Error()
}

// Unwrap returns Err.
func (e *GracePeriodError) Unwrap() error {
	return e.Err
}

// Run implements bedrock.App. In order, it:
//
//  1. reads the lifecycle's Settings from p.Environment, exiting
//     ExitConfig if one is malformed;
//  2. listens for an interrupt and SIGTERM through p.NotifyContext;
//  3. calls Init, and if it fails, runs the OnStop functions registered so
//     far and exits ExitConfig when a *config.Error is in Init's error
//     chain, or ExitFailure otherwise;
//  4. calls Serve on every Service concurrently;
//  5. waits for a signal, or for the first Service to return;
//  6. stops listening for signals, so a second one takes its default
//     action, and cancels the context passed to Serve;
//  7. waits DrainDelay;
//  8. calls Shutdown on every Service concurrently, waits for every Serve
//     to return, and then runs the OnStop functions in reverse order of
//     registration, all within one ShutdownGracePeriod, which starts after
//     the drain delay.
//
// An Init that returns no Services ends the App at once, without a drain
// delay, as there is nothing to drain. Neither a cancelled ctx nor a second
// signal cuts the drain delay short: the second signal takes its default
// action, which ends the process. Run waits for every Serve, Shutdown and OnStop call it
// made to return: the grace period reaches them through the context they
// are given, which they must honour.
//
// The status is 0, with the signal as its cause, after a signal; 0 with no
// cause when a Service returned nil on its own; ExitFailure with a
// *GracePeriodError as its cause when the grace period ran out; and
// ExitFailure with every error joined as its cause otherwise. The cause of
// every failure is written to p.Stderr, one line per error, and nothing
// else is written. Run never recovers a panic.
func (a App) Run(ctx context.Context, p bedrock.Process) bedrock.ExitStatus {
	settings, err := ReadSettings(ctx, p.Environment)
	if err != nil {
		return fail(p, ExitConfig, err)
	}

	sigCtx, stop := p.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	setup := &Setup{Process: p, Host: a.Host}
	services, err := a.Init(sigCtx, setup)
	if err != nil {
		stop()
		code := ExitFailure
		if _, ok := errors.AsType[*config.Error](err); ok {
			code = ExitConfig
		}
		errs, ranOut := shutdown(ctx, settings.ShutdownGracePeriod, setup, nil)
		errs = append([]error{fmt.Errorf("init: %w", err)}, errs...)
		return fail(p, code, failure(settings.ShutdownGracePeriod, errs, ranOut))
	}

	serveCtx, cancelServe := context.WithCancel(sigCtx)
	defer cancelServe()
	served := make(chan error, len(services))
	for _, svc := range services {
		go func() { served <- svc.Serve(serveCtx) }()
	}

	var errs []error
	pending := len(services)
	signalled := false
	if pending > 0 {
		select {
		case <-sigCtx.Done():
			signalled = true
		case err := <-served:
			pending--
			errs = appendServeErr(errs, err)
		}
	}
	cause := context.Cause(sigCtx)
	stop()
	cancelServe()
	if len(services) > 0 {
		// time.Sleep returns at once for a DrainDelay of zero or less.
		time.Sleep(a.DrainDelay)
	}

	stopServices := func(graceCtx context.Context) []error {
		errs := shutdownServices(graceCtx, services)
		for range pending {
			errs = appendServeErr(errs, <-served)
		}
		return errs
	}
	shutdownErrs, ranOut := shutdown(ctx, settings.ShutdownGracePeriod, setup, stopServices)
	errs = append(errs, shutdownErrs...)

	if err := failure(settings.ShutdownGracePeriod, errs, ranOut); err != nil {
		return fail(p, ExitFailure, err)
	}
	if signalled {
		return bedrock.ExitWithCause(0, cause)
	}
	return bedrock.Exit(0)
}

// failure is the cause of a run whose Init, Services, shutdown and OnStop
// functions returned errs: a *GracePeriodError holding them if the grace
// period ran out, every error joined if any failed, and nil otherwise.
func failure(grace time.Duration, errs []error, ranOut bool) error {
	err := errors.Join(errs...)
	if !ranOut {
		return err
	}
	if err == nil {
		err = context.DeadlineExceeded
	}
	return &GracePeriodError{GracePeriod: grace, Err: err}
}

// appendServeErr appends the error a Serve call returned, if it failed.
func appendServeErr(errs []error, err error) []error {
	if err == nil {
		return errs
	}
	return append(errs, fmt.Errorf("serve: %w", err))
}

// shutdown stops the App within one grace period: it calls stopServices,
// if it is not nil, and then runs the OnStop functions of setup. It returns
// every error they returned, and whether the grace period ran out.
//
// The grace period's context keeps ctx's values but not its cancellation,
// so a cancelled root context still gets a graceful shutdown.
func shutdown(ctx context.Context, grace time.Duration, setup *Setup, stopServices func(context.Context) []error) ([]error, bool) {
	graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	var errs []error
	if stopServices != nil {
		errs = stopServices(graceCtx)
	}
	for _, f := range setup.stopFuncs() {
		if err := f(graceCtx); err != nil {
			errs = append(errs, fmt.Errorf("on stop: %w", err))
		}
	}
	return errs, graceCtx.Err() != nil
}

// shutdownServices calls Shutdown on every Service concurrently, waits for
// every call to return, and returns the errors they returned.
func shutdownServices(ctx context.Context, services []Service) []error {
	results := make(chan error, len(services))
	for _, svc := range services {
		go func() { results <- svc.Shutdown(ctx) }()
	}
	var errs []error
	for range services {
		if err := <-results; err != nil {
			errs = append(errs, fmt.Errorf("shutdown: %w", err))
		}
	}
	return errs
}

// fail writes cause to p.Stderr, one line per error, and returns it as the
// cause of an ExitStatus with code.
func fail(p bedrock.Process, code int, cause error) bedrock.ExitStatus {
	// A write error to stderr leaves nowhere left to report it, and the
	// cause still carries the failure to the caller of Run.
	_, _ = fmt.Fprintln(p.Stderr, cause)
	return bedrock.ExitWithCause(code, cause)
}
