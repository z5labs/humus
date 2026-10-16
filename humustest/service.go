// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
)

// The rules of the humus.Service contract. Each is the name of one of
// TestService's subtests, and a failure message quotes the rule that the
// Service broke.
const (
	ruleServeReturnsNil = "Serve returns nil after Shutdown"
	ruleDoneContext     = "Shutdown with a done context stops the Service at once"
	ruleSafe            = "Shutdown is safe"
	ruleBeforeServe     = "before Serve"
	ruleAfterReturn     = "after Serve has returned on its own"
	ruleTwice           = "twice"
	ruleNoLeak          = "No goroutine the Service started outlives Serve"
)

// waitLimit bounds every wait of a subtest for the Service: for Serve to
// return, and for the context given to Shutdown to run out. It is the App's
// default ShutdownGracePeriod, and it passes in fake time.
const waitLimit = 10 * time.Second

// The messages synctest panics with when a bubble deadlocks. A goroutine
// left blocked after the subtest's own goroutine has returned is a leak; every
// goroutine blocked while it is still running is a hang.
const (
	leakPanic = "deadlock: main bubble goroutine has exited but blocked goroutines remain"
	hangPanic = "deadlock: all goroutines in bubble are blocked"
)

// errNoService is the cause of the App of a subtest whose newService did not
// return a Service.
var errNoService = errors.New("humustest: newService did not return a Service")

// TestService runs the humus.Service contract against Services that
// newService builds. A package that provides a Service calls it from its own
// tests, so that every Service shuts down the way the App lifecycle expects:
//
//   - Serve returns nil after Shutdown.
//   - Shutdown with a context that is already done stops the Service at
//     once, and returns an error for which errors.Is(err, ctx.Err()) holds.
//   - Shutdown is safe when it is called before Serve, after Serve has
//     returned on its own, and twice: it neither panics nor blocks, and it
//     returns nil. A Serve called after Shutdown returns.
//   - No goroutine the Service started outlives Serve.
//
// Each rule is a subtest named after it, and its failure message names the
// rule the Service broke. Every subtest runs inside testing/synctest, on a
// new [Host], with a new Service. It runs a humus.App, started with
// bedrocktest.Start, whose Init calls newService with the subtest's t and the
// App's Setup, so the Setup's Process is bedrocktest's and its Host is the
// new Host. The App runs a Service of TestService's own, which makes the
// subtest's calls on the Service newService built; when that is done, the
// App shuts down and runs the functions newService registered with
// Setup.OnStop, as it does in production.
//
// newService runs on the subtest's goroutine, so it may call t.Fatal. It
// must build a Service that runs until it is stopped: a job that finishes at
// once can't show that Shutdown stops it. A Service that keeps serving after
// Serve's context is done, as an HTTP server does, never returns on its own,
// and the subtest for Shutdown after that reports SKIP.
//
// The App runs a Service of TestService's own rather than the Service under
// test, so that each subtest can call Serve and Shutdown in the order its
// rule needs. The Service's Serve is called with a context that stays live
// until Serve returns, unless the subtest cancels it, so Shutdown must stop
// the Service without help from that context. Fake time makes every wait
// free: a Serve that has not returned 10 seconds after Shutdown never will.
//
// A goroutine that outlives Serve is found by synctest, which reports it as
// a deadlock when the subtest ends. Only the leak subtest fails on it.
//
// All of this rests on synctest seeing the Service's goroutines block, so
// the Service must reach the network and the filesystem only through the
// Setup's Host. A goroutine blocked where synctest can't see it, such as on
// a real socket, in a syscall, on a sync.Mutex or on a channel made outside
// the subtest, stops fake time: the subtest's waits never run out, a leak
// of such a goroutine is never reported, and the test binary hangs until
// its -test.timeout. TestService must not be called inside a synctest
// bubble.
func TestService(t *testing.T, newService func(t *testing.T, s *humus.Setup) humus.Service) {
	t.Helper()
	if newService == nil {
		t.Fatal("humustest: TestService requires a non-nil newService")
	}

	runRule(t, ruleServeReturnsNil, ruleServeReturnsNil, newService, checkServeReturnsNil)
	runRule(t, ruleDoneContext, ruleDoneContext, newService, checkDoneContext)
	t.Run(ruleSafe, func(t *testing.T) {
		runRule(t, ruleBeforeServe, ruleSafe+" "+ruleBeforeServe, newService, checkBeforeServe)
		runRule(t, ruleAfterReturn, ruleSafe+" "+ruleAfterReturn, newService, checkAfterReturn)
		runRule(t, ruleTwice, ruleSafe+" "+ruleTwice, newService, checkTwice)
	})
	runRule(t, ruleNoLeak, ruleNoLeak, newService, checkNoLeak)
}

// subtest is one run of one rule: the Service under test, and what the
// subtest has seen of it.
type subtest struct {
	// rule is the rule the subtest checks, as its failure messages quote
	// it.
	rule string

	// t is the subtest's t inside the synctest bubble. svc is the Service
	// newService built, and ctx the context the App called Serve with.
	t   *testing.T
	svc humus.Service
	ctx context.Context

	// step is what the subtest was waiting for, to report a hang with, and
	// inService whether that was a call on the Service. served is whether
	// Serve has returned, and skipped why the subtest skipped, if it did.
	// All of them are read from outside the bubble.
	step      atomic.Pointer[string]
	inService atomic.Bool
	served    atomic.Bool
	skipped   atomic.Pointer[string]
}

// runRule runs check as the subtest name of t, inside synctest, on a new
// App running a new Service.
func runRule(t *testing.T, name, rule string, newService func(*testing.T, *humus.Setup) humus.Service, check func(*subtest)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		st := &subtest{rule: rule}
		st.setStep("starting the App")
		defer st.finish(t)
		synctest.Test(t, func(t *testing.T) {
			st.t = t
			st.run(newService, check)
		})
	})
}

// finish runs outside the bubble once it has ended. It turns the panic of
// a deadlocked bubble into a failure of t: a leak fails only the leak
// subtest, so that a Service which leaks fails no other rule, and a hang
// fails any subtest. Every other panic is not the Service's contract to
// report, and is re-raised.
//
// It then skips t if the subtest skipped inside the bubble: testing does
// not carry a skip out of a synctest bubble, and the subtest would
// otherwise report PASS having checked nothing.
func (st *subtest) finish(t *testing.T) {
	if r := recover(); r != nil {
		err, ok := r.(error)
		if !ok {
			panic(r)
		}
		switch {
		case strings.HasPrefix(err.Error(), leakPanic):
			if st.rule == ruleNoLeak && st.served.Load() {
				t.Errorf("humustest: the Service broke %q: a goroutine was still blocked after Serve returned and the App's OnStop functions had run (%v)", st.rule, err)
			}
		case strings.HasPrefix(err.Error(), hangPanic) && st.inService.Load():
			t.Errorf("humustest: the Service broke %q: every goroutine was blocked, for good, while %s (%v)", st.rule, *st.step.Load(), err)
		case strings.HasPrefix(err.Error(), hangPanic):
			t.Errorf("humustest: %q: every goroutine was blocked, for good, while %s (%v)", st.rule, *st.step.Load(), err)
		default:
			panic(r)
		}
	}
	if why := st.skipped.Load(); why != nil && !t.Failed() {
		t.Skip(*why)
	}
}

// run starts an App whose Init calls newService, runs check against the
// Service it builds, and waits for the App to return.
func (st *subtest) run(newService func(*testing.T, *humus.Setup) humus.Service, check func(*subtest)) {
	t := st.t
	host := NewHost(t)
	setups := make(chan *humus.Setup)
	built := make(chan humus.Service, 1)
	drv := &driver{
		serving: make(chan context.Context, 1),
		done:    make(chan struct{}),
	}
	app := humus.App{
		Host: host.Host(),
		Init: func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			select {
			case setups <- s:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			// built is closed without a Service when newService fails
			// the subtest.
			if svc, ok := <-built; ok && svc != nil {
				return []humus.Service{drv}, nil
			}
			return nil, errNoService
		},
	}
	_, status := bedrocktest.Start(t, app)

	// newService runs here, on the subtest's goroutine, rather than in
	// Init, so that it can call t.Fatal.
	st.setStep("waiting for the App to call Init")
	var s *humus.Setup
	select {
	case s = <-setups:
	case es := <-status:
		t.Fatalf("humustest: the App exited %d before it called Init: %v", es.Code(), es.Cause())
	}
	st.setStep("calling newService")
	func() {
		defer close(built)
		st.svc = newService(t, s)
		built <- st.svc
	}()
	if st.svc == nil {
		t.Fatal("humustest: newService returned a nil Service")
	}

	st.setStep("waiting for the App to call Serve")
	select {
	case st.ctx = <-drv.serving:
	case es := <-status:
		t.Fatalf("humustest: the App exited %d before it called Serve: %v", es.Code(), es.Cause())
	}
	func() {
		defer close(drv.done)
		check(st)
	}()

	st.setStep("waiting for the App to return")
	if es := <-status; es.Code() != 0 {
		t.Errorf("humustest: the App exited %d after %q was checked: %v", es.Code(), st.rule, es.Cause())
	}
}

// driver is the Service the App of a subtest runs. Its Serve hands the
// subtest the App's context and returns once the subtest is done, which
// ends the App.
type driver struct {
	serving chan context.Context
	done    chan struct{}
}

func (d *driver) Serve(ctx context.Context) error {
	d.serving <- ctx
	<-d.done
	return nil
}

func (*driver) Shutdown(context.Context) error {
	return nil
}

// checkServeReturnsNil calls Shutdown on a Service that is serving, and
// checks that its Serve then returns nil.
func checkServeReturnsNil(st *subtest) {
	ctx, cancel := context.WithCancel(st.ctx)
	defer cancel()
	served := st.serveUntilStopped(ctx)

	if c := st.shutdown(); c.panicked {
		st.errorf("Shutdown panicked: %v", c.value)
		return
	}
	c, ok := st.awaitServe(served, "Serve to return after Shutdown")
	switch {
	case !ok:
		st.errorf("Serve had not returned %s after Shutdown returned", waitLimit)
	case c.panicked:
		st.errorf("Serve panicked after Shutdown: %v", c.value)
	case c.err != nil:
		st.errorf("Serve returned %v after Shutdown, want nil", c.err)
	}
}

// checkDoneContext calls Shutdown with a context that is already done on a
// Service that is serving, and checks that Shutdown returns the context's
// error with no time passing, and that Serve has returned by then.
func checkDoneContext(st *subtest) {
	ctx, cancel := context.WithCancel(st.ctx)
	defer cancel()
	served := st.serveUntilStopped(ctx)

	done, cancelDone := context.WithCancel(context.WithoutCancel(st.ctx))
	cancelDone()
	st.setServiceStep("calling Shutdown with a done context")
	start := time.Now()
	c := callShutdown(done, st.svc)
	took := time.Since(start)
	switch {
	case c.panicked:
		st.errorf("Shutdown panicked: %v", c.value)
		return
	case !errors.Is(c.err, done.Err()):
		st.errorf("Shutdown returned %v, want an error for which errors.Is(err, ctx.Err()) holds", c.err)
	}
	if took > 0 {
		st.errorf("Shutdown took %s to return, want it to return at once", took)
	}

	synctest.Wait()
	select {
	case <-served:
		st.served.Store(true)
	default:
		st.errorf("Serve had not returned when Shutdown returned, want the Service stopped at once")
		st.awaitServe(served, "Serve to return after Shutdown with a done context")
	}
}

// checkBeforeServe calls Shutdown on a Service whose Serve has not been
// called, and then calls Serve, which the App does when a signal arrives
// as it starts the Services.
func checkBeforeServe(st *subtest) {
	c := st.shutdown()
	switch {
	case c.panicked:
		st.errorf("Shutdown panicked when it was called before Serve: %v", c.value)
		return
	case c.err != nil:
		st.errorf("Shutdown returned %v when it was called before Serve, want nil", c.err)
	}

	ctx, cancel := context.WithCancel(st.ctx)
	defer cancel()
	served := st.serve(ctx)
	r, ok := st.awaitServe(served, "Serve to return after Shutdown was called before it")
	switch {
	case !ok:
		st.errorf("Serve had not returned %s after it was called on a Service already shut down; the App waits for every Serve, so it would never exit", waitLimit)
	case r.panicked:
		st.errorf("Serve panicked when it was called after Shutdown: %v", r.value)
	}
}

// checkAfterReturn cancels Serve's context, as the App does when it starts
// shutting down, and calls Shutdown once Serve has returned. A Service that
// keeps serving after its context is done is stopped, and the subtest is
// skipped.
func checkAfterReturn(st *subtest) {
	ctx, cancel := context.WithCancel(st.ctx)
	defer cancel()
	served := st.serve(ctx)
	cancel()
	if _, ok := st.awaitServe(served, "Serve to return once its context was done"); !ok {
		st.shutdown()
		st.awaitServe(served, "Serve to return after Shutdown")
		st.skipf("Serve keeps running after its context is done, as an HTTP Service does, so it never returns on its own")
	}

	c := st.shutdown()
	switch {
	case c.panicked:
		st.errorf("Shutdown panicked when it was called after Serve had returned on its own: %v", c.value)
	case c.err != nil:
		st.errorf("Shutdown returned %v when it was called after Serve had returned on its own, want nil; the App calls Shutdown on every Service, so this fails the App", c.err)
	}
}

// checkTwice calls Shutdown twice on a Service that is serving.
func checkTwice(st *subtest) {
	ctx, cancel := context.WithCancel(st.ctx)
	defer cancel()
	served := st.serve(ctx)
	synctest.Wait() // so that Serve has started

	if c := st.shutdown(); c.panicked {
		st.errorf("Shutdown panicked the first time it was called: %v", c.value)
		return
	}
	c := st.shutdown()
	switch {
	case c.panicked:
		st.errorf("Shutdown panicked when it was called a second time: %v", c.value)
	case c.err != nil:
		st.errorf("Shutdown returned %v when it was called a second time, want nil", c.err)
	}
	st.awaitServe(served, "Serve to return after Shutdown")
}

// checkNoLeak stops a Service that is serving and waits for Serve to
// return. synctest finds a goroutine that outlives it when the subtest ends,
// and recoverDeadlock reports it.
func checkNoLeak(st *subtest) {
	ctx, cancel := context.WithCancel(st.ctx)
	defer cancel()
	served := st.serveUntilStopped(ctx)

	st.shutdown()
	if _, ok := st.awaitServe(served, "Serve to return after Shutdown"); !ok {
		st.skipf("Serve had not returned %s after Shutdown returned", waitLimit)
	}
}

// call is what a call of Serve or Shutdown came to: the error it returned,
// or the value it panicked with.
type call struct {
	err      error
	panicked bool
	value    any
}

// callShutdown calls svc.Shutdown(ctx), recovering a panic.
func callShutdown(ctx context.Context, svc humus.Service) (c call) {
	defer func() {
		if r := recover(); r != nil {
			c = call{panicked: true, value: r}
		}
	}()
	return call{err: svc.Shutdown(ctx)}
}

// shutdown calls Shutdown with a live context, which runs out after
// waitLimit.
func (st *subtest) shutdown() call {
	st.setServiceStep("calling Shutdown")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(st.ctx), waitLimit)
	defer cancel()
	return callShutdown(ctx, st.svc)
}

// serve calls Serve in a new goroutine, and returns the channel what it came
// to arrives on. A panic in Serve arrives as a call, rather than crashing
// the test binary.
func (st *subtest) serve(ctx context.Context) <-chan call {
	served := make(chan call, 1)
	go func() {
		var c call
		defer func() {
			if r := recover(); r != nil {
				c = call{panicked: true, value: r}
			}
			served <- c
		}()
		c.err = st.svc.Serve(ctx)
	}()
	return served
}

// serveUntilStopped calls Serve, waits for every goroutine in the bubble to
// block, and stops the subtest if Serve has already returned: a Service
// that is not serving can't show what stops it.
func (st *subtest) serveUntilStopped(ctx context.Context) <-chan call {
	served := st.serve(ctx)
	synctest.Wait()
	select {
	case c := <-served:
		st.served.Store(true)
		st.t.Fatalf("humustest: %q can't be checked: Serve returned before anything stopped it (%s); newService must build a Service that runs until it is stopped", st.rule, c)
	default:
	}
	return served
}

// awaitServe waits up to waitLimit for Serve to return, and reports what it
// came to and whether it returned.
func (st *subtest) awaitServe(served <-chan call, step string) (call, bool) {
	st.setServiceStep("waiting for " + step)
	select {
	case c := <-served:
		st.served.Store(true)
		return c, true
	case <-time.After(waitLimit):
		return call{}, false
	}
}

// errorf fails the subtest with a message naming the rule the Service broke.
func (st *subtest) errorf(format string, args ...any) {
	st.t.Helper()
	st.t.Errorf("humustest: the Service broke %q: %s", st.rule, fmt.Sprintf(format, args...))
}

// skipf skips the subtest, which can't check its rule on this Service.
func (st *subtest) skipf(format string, args ...any) {
	why := fmt.Sprintf("humustest: %q can't be checked: %s", st.rule, fmt.Sprintf(format, args...))
	st.skipped.Store(&why)
	st.t.Skip(why)
}

// setStep records what the subtest is doing, outside a call on the
// Service.
func (st *subtest) setStep(step string) {
	st.step.Store(&step)
	st.inService.Store(false)
}

// setServiceStep records a call on the Service the subtest is making or
// waiting for.
func (st *subtest) setServiceStep(step string) {
	st.step.Store(&step)
	st.inService.Store(true)
}

// String describes what a call came to, for a failure message.
func (c call) String() string {
	if c.panicked {
		return fmt.Sprintf("it panicked: %v", c.value)
	}
	if c.err != nil {
		return fmt.Sprintf("it returned %v", c.err)
	}
	return "it returned nil"
}
