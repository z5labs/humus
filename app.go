// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus/telemetry"
)

// Exit codes of an App. Every other failure is told apart by the cause of
// the bedrock.ExitStatus, not by its code.
const (
	// ExitFailure is the code of an App whose telemetry could not be built
	// or shut down, whose Init, a Service, the drain, a Service's Shutdown or
	// an OnStop function failed, or whose shutdown ran past the grace period.
	ExitFailure = 1

	// ExitConfig is the code of an App with a setting that is missing or
	// malformed, including a config value read by the telemetry Builder
	// or by Init: EX_CONFIG from sysexits.h.
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
	// App's drain. A Service may stop on it, as a job, a queue consumer or
	// a health Service does, or keep serving until Shutdown, as an HTTP or
	// gRPC Service does. The drain ends once every Serve has returned.
	Serve(ctx context.Context) error

	// Shutdown stops the Service gracefully. The App calls it on every
	// Service after the App's drain, including one whose Serve has
	// already returned. If ctx is done before the Service has stopped,
	// Shutdown stops it at once and returns an error that wraps ctx's
	// error.
	Shutdown(ctx context.Context) error
}

// App is a bedrock.App that runs Services with humus's lifecycle. Host,
// Telemetry and Init are required.
type App struct {
	// Host is the network and filesystem the App runs on: OSHost() in
	// main, and a Host the test controls in a test.
	Host Host

	// ShutdownGracePeriod is the grace period when the environment does not
	// set HUMUS_SHUTDOWN_GRACE_PERIOD. The grace period bounds the whole
	// shutdown of the App: every Service's Shutdown and every OnStop function
	// share it.
	//
	// The grace period must be positive, so a zero ShutdownGracePeriod fails
	// startup, exiting ExitConfig, unless HUMUS_SHUTDOWN_GRACE_PERIOD is set.
	// A runtime package's App sets its own.
	ShutdownGracePeriod time.Duration

	// Drain is the App's drain when the environment does not set
	// HUMUS_SHUTDOWN_DRAIN_DELAY, such as DrainDelay(5 * time.Second). The
	// drain is the pause between shutdown starting and Shutdown being called
	// on the Services: Serve's context is done at once, and Shutdown is
	// called once the drain returns. It gives a load balancer, such as a
	// Kubernetes Service removing the pod from its endpoints, time to stop
	// sending new connections before the Services stop accepting them. The
	// grace period starts after it. A nil Drain doesn't wait.
	//
	// When HUMUS_SHUTDOWN_DRAIN_DELAY is set, the drain is DrainDelay of its
	// value in place of Drain, whatever Drain is, so an operator replaces a
	// Drain without a rebuild.
	Drain Drain

	// Telemetry builds the App's telemetry.SDK. It is required: the end
	// user's own Builder in main, and telemetry.NoopBuilder or the Builder of
	// humustest's recorder, humustest.Telemetry, in tests.
	//
	// Run calls it after it starts listening for signals and before Init,
	// with a context that a signal cancels, the Process, and the Host's
	// Dial. The SDK it returns is handed to Init in Setup.Telemetry, the
	// lifecycle logs through its LoggerProvider, and it is shut down last,
	// after the OnStop functions.
	Telemetry telemetry.Builder

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

	// Telemetry is the App's SDK, built by App.Telemetry. Init and the
	// Services get their tracers, meters and loggers from it. The App shuts
	// it down after the OnStop functions, so neither Init nor a Service
	// calls its Shutdown.
	Telemetry telemetry.SDK

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
//  1. reads the grace period from HUMUS_SHUTDOWN_GRACE_PERIOD in
//     p.Environment, a time.ParseDuration string such as "30s" defaulting to
//     the App's ShutdownGracePeriod, and the drain: DrainDelay of
//     HUMUS_SHUTDOWN_DRAIN_DELAY, a time.ParseDuration string such as "5s",
//     when it is set, in place of the App's Drain, and the App's Drain when
//     it is not. It exits ExitConfig, naming every variable at fault, if
//     either variable is malformed, the grace period is not positive after
//     its default, or HUMUS_SHUTDOWN_DRAIN_DELAY is negative;
//  2. listens for an interrupt and SIGTERM through p.NotifyContext;
//  3. builds the App's telemetry by calling App.Telemetry with the context
//     a signal cancels, p and Host.Dial. It exits ExitConfig when a
//     *config.Error is in the Builder's error chain, and ExitFailure for any
//     other failure or for an SDK with a nil field;
//  4. sets OpenTelemetry's process-wide error handler to one that writes
//     each error to p.Stderr, as one line starting with "telemetry: ";
//  5. calls Init, and if it fails, runs the OnStop functions registered so
//     far and exits ExitConfig when a *config.Error is in Init's error
//     chain, or ExitFailure otherwise;
//  6. calls Serve on every Service concurrently, and logs "serving";
//  7. waits for a signal, or for the first Service to return;
//  8. stops listening for signals, so a second one takes its default
//     action, cancels the context passed to Serve, and logs "shutting
//     down";
//  9. when it hasn't yet received every Serve's result, calls the drain
//     with the Setup that Init was given and a context that keeps ctx's
//     values, is not cancelled with it, is done once every Serve has
//     returned, and gives ShutdownCause the shutdown's cause: the signal's
//     cause after a signal, ctx's cause when ctx was cancelled, and an
//     error wrapping ErrServiceReturned, and the error Serve returned if it
//     failed, when a Service returned first. It collects the Serve results
//     that arrive while the drain runs, and waits for the drain to return;
//  10. calls Shutdown on every Service concurrently, waits for every Serve
//     to return, and then runs the OnStop functions in reverse order of
//     registration, all within one grace period, which starts after the
//     drain, and logs "stopped";
//  11. shuts the telemetry down, with a context that keeps ctx's values but
//     not its cancellation, separately from the grace period.
//
// An Init that returns no Services, and a Service that returns on its own
// when it is the only one, end the App without calling the drain, as there
// is nothing to drain. Neither a cancelled ctx nor a second signal cuts the
// drain short: the second signal takes its default action, which ends the
// process. A drain that fails is joined into the cause as "drain: " and its
// error, and Shutdown is still called on every Service. Run waits for every
// Serve, Shutdown and OnStop call it made to return: the grace period
// reaches them through the context they are given, which they must honour.
//
// The status is 0, with the signal as its cause, after a signal; 0 with no
// cause when a Service returned nil on its own; ExitFailure with a
// *GracePeriodError as its cause when the grace period ran out; and
// ExitFailure with every error joined as its cause otherwise.
//
// The lifecycle logs through the LoggerProvider of the SDK, with the
// OpenTelemetry Logs API and the logger name "github.com/z5labs/humus":
// "serving", "shutting down" and "stopped" at INFO, and every failure after
// the SDK is built at ERROR, one record per error. Standard error gets only
// what telemetry cannot report itself: a failed setting or a failure to
// build telemetry, one line per error; the export errors the handler of
// step 4 writes; and a failure to shut telemetry down, which is joined into
// the cause and turns an exit 0 into ExitFailure. Run never recovers a
// panic, including one in the drain.
func (a App) Run(ctx context.Context, p bedrock.Process) bedrock.ExitStatus {
	stderr := &lockedWriter{w: p.Stderr}
	settings, err := a.readSettings(ctx, p.Environment)
	if err != nil {
		return fail(stderr, ExitConfig, err)
	}

	sigCtx, stop := p.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	sdk, err := a.buildTelemetry(ctx, sigCtx, p)
	if err != nil {
		code := ExitFailure
		if _, ok := errors.AsType[*config.Error](err); ok {
			code = ExitConfig
		}
		return fail(stderr, code, err)
	}

	//nolint:forbidigo // The one use of OpenTelemetry's globals, allowed by #632: export errors go to the Process's stderr.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		msg := strings.ReplaceAll(err.Error(), "\n", "; ")
		// A write error to stderr leaves nowhere left to report it.
		_, _ = fmt.Fprintln(stderr, "telemetry: "+msg)
	}))

	l := &lifecycleLog{
		ctx:    context.WithoutCancel(ctx),
		logger: sdk.LoggerProvider.Logger(loggerName),
	}
	code, cause := a.run(ctx, sigCtx, stop, p, settings, sdk, l)

	if err := sdk.Shutdown(context.WithoutCancel(ctx)); err != nil {
		err = fmt.Errorf("shut down telemetry: %w", err)
		_, _ = fmt.Fprintln(stderr, err)
		cause = errors.Join(cause, err)
		if code == 0 {
			code = ExitFailure
		}
	}
	if code == 0 && cause == nil {
		return bedrock.Exit(0)
	}
	return bedrock.ExitWithCause(code, cause)
}

// loggerName is the name of the logger the lifecycle logs through.
const loggerName = "github.com/z5labs/humus"

// buildTelemetry calls App.Telemetry with sigCtx and checks the SDK it
// returns. An SDK with a nil field is shut down, if it can be, with a
// context that keeps ctx's values but not its cancellation, and is an error.
func (a App) buildTelemetry(ctx, sigCtx context.Context, p bedrock.Process) (telemetry.SDK, error) {
	if a.Telemetry == nil {
		return telemetry.SDK{}, errors.New("build telemetry: App.Telemetry is nil")
	}
	sdk, err := a.Telemetry(sigCtx, p, a.Host.Dial)
	if err != nil {
		return telemetry.SDK{}, fmt.Errorf("build telemetry: %w", err)
	}
	var missing []string
	if sdk.TracerProvider == nil {
		missing = append(missing, "TracerProvider")
	}
	if sdk.MeterProvider == nil {
		missing = append(missing, "MeterProvider")
	}
	if sdk.LoggerProvider == nil {
		missing = append(missing, "LoggerProvider")
	}
	if sdk.Propagator == nil {
		missing = append(missing, "Propagator")
	}
	if sdk.Shutdown == nil {
		missing = append(missing, "Shutdown")
	}
	if len(missing) == 0 {
		return sdk, nil
	}
	err = fmt.Errorf("build telemetry: the SDK's %s is nil", strings.Join(missing, ", "))
	if sdk.Shutdown != nil {
		if serr := sdk.Shutdown(context.WithoutCancel(ctx)); serr != nil {
			err = errors.Join(err, fmt.Errorf("shut down telemetry: %w", serr))
		}
	}
	return telemetry.SDK{}, err
}

// run is Run from Init on, up to shutting the telemetry down. It returns the
// exit code and its cause, and logs every failure in the cause through l.
func (a App) run(ctx, sigCtx context.Context, stop context.CancelFunc, p bedrock.Process, settings settings, sdk telemetry.SDK, l *lifecycleLog) (int, error) {
	setup := &Setup{Process: p, Host: a.Host, Telemetry: sdk}
	services, err := a.Init(sigCtx, setup)
	if err != nil {
		stop()
		code := ExitFailure
		if _, ok := errors.AsType[*config.Error](err); ok {
			code = ExitConfig
		}
		errs, ranOut := shutdown(ctx, settings.gracePeriod, setup, nil)
		errs = append([]error{fmt.Errorf("init: %w", err)}, errs...)
		cause := failure(settings.gracePeriod, errs, ranOut)
		l.failure(cause)
		return code, cause
	}

	serveCtx, cancelServe := context.WithCancel(sigCtx)
	defer cancelServe()
	served := make(chan error, len(services))
	for _, svc := range services {
		go func() { served <- svc.Serve(serveCtx) }()
	}
	l.info("serving")

	var errs []error
	pending := len(services)
	signalled := false
	var returned error
	if pending > 0 {
		select {
		case <-sigCtx.Done():
			signalled = true
		case err := <-served:
			pending--
			errs = appendServeErr(errs, err)
			returned = serviceReturned(err)
		}
	}
	cause := context.Cause(sigCtx)
	stop()
	cancelServe()
	l.info("shutting down")
	if pending > 0 && settings.drain != nil {
		why := cause
		if !signalled {
			why = returned
		}
		drainErrs, left, err := drain(ctx, settings.drain, setup, served, pending, why)
		errs = append(errs, drainErrs...)
		pending = left
		if err != nil {
			errs = append(errs, fmt.Errorf("drain: %w", err))
		}
	}

	stopServices := func(graceCtx context.Context) []error {
		errs := shutdownServices(graceCtx, services)
		for range pending {
			errs = appendServeErr(errs, <-served)
		}
		return errs
	}
	shutdownErrs, ranOut := shutdown(ctx, settings.gracePeriod, setup, stopServices)
	errs = append(errs, shutdownErrs...)

	if err := failure(settings.gracePeriod, errs, ranOut); err != nil {
		l.failure(err)
		l.info("stopped")
		return ExitFailure, err
	}
	l.info("stopped")
	if signalled {
		return 0, cause
	}
	return 0, nil
}

// lifecycleLog emits the lifecycle's own log records. Its context keeps the
// root context's values but not its cancellation, so the records of a
// shutdown after a cancelled root context are still exported.
type lifecycleLog struct {
	ctx    context.Context
	logger log.Logger
}

// info emits msg at INFO.
func (l *lifecycleLog) info(msg string) {
	l.emit(log.SeverityInfo, "INFO", msg)
}

// failure emits every error in cause at ERROR, one record each: each error
// it joins, or cause itself when it joins none.
func (l *lifecycleLog) failure(cause error) {
	if j, ok := cause.(interface{ Unwrap() []error }); ok {
		for _, err := range j.Unwrap() {
			l.emit(log.SeverityError, "ERROR", err.Error())
		}
		return
	}
	l.emit(log.SeverityError, "ERROR", cause.Error())
}

func (l *lifecycleLog) emit(sev log.Severity, text, body string) {
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(text)
	r.SetBody(attribute.StringValue(body))
	l.logger.Emit(l.ctx, r)
}

// lockedWriter serializes the writes to the Process's stderr, which come
// from Run and from OpenTelemetry's error handler, called on the SDK's own
// goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(b)
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

// fail writes cause to stderr, one line per error, and returns it as the
// cause of an ExitStatus with code. It is for the failures before the
// telemetry is built, which have nothing else to report them.
func fail(stderr io.Writer, code int, cause error) bedrock.ExitStatus {
	// A write error to stderr leaves nowhere left to report it, and the
	// cause still carries the failure to the caller of Run.
	_, _ = fmt.Fprintln(stderr, cause)
	return bedrock.ExitWithCause(code, cause)
}
