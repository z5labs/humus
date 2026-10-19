// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
)

// Every test in this file runs an App whole with a Drain that reads
// ShutdownCause, inside a synctest bubble, on a bedrocktest Process and a
// humustest Host.

// causeDrain is a Drain that records ShutdownCause of its context, and of
// a context derived from it, and returns nil.
type causeDrain struct {
	mu      sync.Mutex
	calls   int
	cause   error
	derived error
}

func (d *causeDrain) Drain(ctx context.Context, _ *humus.Setup) error {
	child, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.cause = humus.ShutdownCause(ctx)
	d.derived = humus.ShutdownCause(child)
	return nil
}

// recordedCause returns the cause the Drain recorded, failing t unless the
// Drain was called once and a context derived from its own gave the same
// cause.
func (d *causeDrain) recordedCause(t *testing.T) error {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls != 1 {
		t.Fatalf("the Drain was called %d times, want once", d.calls)
	}
	if d.derived != d.cause {
		t.Errorf("ShutdownCause of a context derived from the Drain's = %v, want the Drain's %v", d.derived, d.cause)
	}
	return d.cause
}

func TestShutdownCause(t *testing.T) {
	t.Run("after SIGTERM is the signal's cause", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := &causeDrain{}
			_, status := signalled(t, drainApp(t, d.Drain, serving(nil)), bedrocktest.NewProcess(t))
			st := <-status

			if st.Code() != 0 {
				t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
			}
			got := d.recordedCause(t)
			if got == nil || got.Error() != "terminated signal received" {
				t.Errorf("ShutdownCause = %v, want terminated signal received", got)
			}
			if got != st.Cause() {
				t.Errorf("ShutdownCause = %v, want the App's cause, %v", got, st.Cause())
			}
			if errors.Is(got, humus.ErrServiceReturned) {
				t.Errorf("ShutdownCause = %v wraps ErrServiceReturned after a signal", got)
			}
		})
	})

	t.Run("after a Service returned nil wraps ErrServiceReturned", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := &causeDrain{}
			job := &fakeService{serve: func(context.Context) error { return nil }}
			app := drainApp(t, d.Drain, job, serving(nil))
			p := bedrocktest.NewProcess(t)

			st := app.Run(t.Context(), p.Process())

			if st.Code() != 0 || st.Cause() != nil {
				t.Errorf("status = (%d, %v), want (0, nil)", st.Code(), st.Cause())
			}
			if got := d.recordedCause(t); !errors.Is(got, humus.ErrServiceReturned) {
				t.Errorf("ShutdownCause = %v, want it to wrap ErrServiceReturned", got)
			}
		})
	})

	t.Run("after a Service returned an error wraps ErrServiceReturned and the error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			errServe := errors.New("export: bucket not found")
			d := &causeDrain{}
			job := &fakeService{serve: func(context.Context) error { return errServe }}
			app := drainApp(t, d.Drain, job, serving(nil))
			p := bedrocktest.NewProcess(t)

			st := app.Run(t.Context(), p.Process())

			if st.Code() != humus.ExitFailure || !errors.Is(st.Cause(), errServe) {
				t.Errorf("status = (%d, %v), want (%d, the Serve error)", st.Code(), st.Cause(), humus.ExitFailure)
			}
			got := d.recordedCause(t)
			if !errors.Is(got, humus.ErrServiceReturned) {
				t.Errorf("ShutdownCause = %v, want it to wrap ErrServiceReturned", got)
			}
			if !errors.Is(got, errServe) {
				t.Errorf("ShutdownCause = %v, want it to wrap the Serve error", got)
			}
		})
	})

	t.Run("after Run's context was cancelled is its cause", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			errStop := errors.New("the test harness stopped the App")
			d := &causeDrain{}
			app := drainApp(t, d.Drain, serving(nil))
			p := bedrocktest.NewProcess(t)

			ctx, cancel := context.WithCancelCause(t.Context())
			status := make(chan bedrock.ExitStatus, 1)
			go func() { status <- app.Run(ctx, p.Process()) }()
			<-p.Notified(syscall.SIGTERM)
			synctest.Wait() // every Serve has started
			cancel(errStop)
			st := <-status

			if st.Code() != 0 {
				t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
			}
			got := d.recordedCause(t)
			if got != errStop {
				t.Errorf("ShutdownCause = %v, want Run's context's cause, %v", got, errStop)
			}
			if errors.Is(got, humus.ErrServiceReturned) {
				t.Errorf("ShutdownCause = %v wraps ErrServiceReturned after Run's context was cancelled", got)
			}
		})
	})

	t.Run("of any other context is nil", func(t *testing.T) {
		if got := humus.ShutdownCause(context.Background()); got != nil {
			t.Errorf("ShutdownCause(context.Background()) = %v, want nil", got)
		}
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(errors.New("cancelled"))
		if got := humus.ShutdownCause(ctx); got != nil {
			t.Errorf("ShutdownCause of a cancelled context = %v, want nil", got)
		}
	})
}

// lbAddr is the address of the fake load balancer's API on the test Host.
const lbAddr = "lb.internal:8080"

// fakeLB is a load balancer's API, served on a test Host. It reports the
// state of the App's target: "healthy", and "draining" once a deregistration
// it was told about has taken its time.
type fakeLB struct {
	mu         sync.Mutex
	drainingAt time.Time
	polls      int
}

// deregister starts deregistering the target, which takes d.
func (lb *fakeLB) deregister(d time.Duration) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.drainingAt = time.Now().Add(d)
}

func (lb *fakeLB) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.polls++
	state := "healthy"
	if !lb.drainingAt.IsZero() && !time.Now().Before(lb.drainingAt) {
		state = "draining"
	}
	_, _ = io.WriteString(w, state)
}

func (lb *fakeLB) pollCount() int {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.polls
}

// serveLB serves lb on lbAddr of h until the test's bubble ends.
func serveLB(t *testing.T, h *humustest.Host, lb *fakeLB) {
	t.Helper()
	ln, err := h.Listen("tcp", lbAddr)
	if err != nil {
		t.Fatalf("listen for the load balancer: %v", err)
	}
	srv := &http.Server{Handler: lb}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
}

// waitUntilDeregistered polls the load balancer through dial, once a
// second, until it reports the App's target draining. It returns nil when
// ctx is done.
func waitUntilDeregistered(ctx context.Context, dial func(ctx context.Context, network, address string) (net.Conn, error)) error {
	client := &http.Client{Transport: &http.Transport{DialContext: dial, DisableKeepAlives: true}}
	for {
		state, err := targetState(ctx, client)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		if state == "draining" {
			return nil
		}
		t := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func targetState(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+lbAddr+"/targets/api", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("load balancer: %s", resp.Status)
	}
	return strings.TrimSpace(string(b)), nil
}

// deregisteringDrain is the Drain of revision 2's example: it returns at
// once when no signal came, and waits for the load balancer otherwise.
func deregisteringDrain(ctx context.Context, s *humus.Setup) error {
	if errors.Is(humus.ShutdownCause(ctx), humus.ErrServiceReturned) {
		return nil // no signal came, so nothing is deregistering the pod
	}
	return waitUntilDeregistered(ctx, s.Host.Dial)
}

func TestApp_Run_a_Drain_that_waits_on_a_load_balancer(t *testing.T) {
	t.Run("returns at once when another Service returned on its own", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			lb := &fakeLB{}
			serveLB(t, h, lb)
			var shutdownAt time.Time
			api := serving(func(context.Context) error {
				shutdownAt = time.Now()
				return nil
			})
			job := &fakeService{serve: func(context.Context) error { return nil }}
			app := drainApp(t, deregisteringDrain, job, api)
			app.Host = h.Host()
			p := bedrocktest.NewProcess(t)

			begin := time.Now()
			st := app.Run(t.Context(), p.Process())

			if st.Code() != 0 || st.Cause() != nil {
				t.Errorf("status = (%d, %v), want (0, nil)", st.Code(), st.Cause())
			}
			if got := shutdownAt.Sub(begin); got != 0 {
				t.Errorf("Shutdown called %s after the job returned, want at once", got)
			}
			if n := lb.pollCount(); n != 0 {
				t.Errorf("the Drain polled the load balancer %d times, want never", n)
			}
		})
	})

	t.Run("after SIGTERM waits until the load balancer reports the target draining", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			lb := &fakeLB{}
			serveLB(t, h, lb)
			var shutdownAt time.Time
			api := serving(func(context.Context) error {
				shutdownAt = time.Now()
				return nil
			})
			app := drainApp(t, deregisteringDrain, api)
			app.Host = h.Host()
			p := bedrocktest.NewProcess(t)

			status := start(t, app, p)
			<-p.Notified(syscall.SIGTERM)
			synctest.Wait() // every Serve has started
			sigterm := time.Now()
			lb.deregister(3 * time.Second) // Kubernetes deregisters the pod as it sends SIGTERM
			p.Signal(syscall.SIGTERM)
			st := <-status

			if st.Code() != 0 {
				t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
			}
			if got := shutdownAt.Sub(sigterm); got != 3*time.Second {
				t.Errorf("Shutdown called %s after SIGTERM, want 3s, once the load balancer reported the target draining", got)
			}
			if n := lb.pollCount(); n != 4 {
				t.Errorf("the Drain polled the load balancer %d times, want 4: at 0s, 1s, 2s and 3s", n)
			}
		})
	})
}

func TestDrainDelay_waits_its_whole_delay_after_a_Service_returned_on_its_own(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var shutdownAt time.Time
		api := serving(func(context.Context) error {
			shutdownAt = time.Now()
			return nil
		})
		job := &fakeService{serve: func(context.Context) error { return nil }}
		app := drainApp(t, humus.DrainDelay(5*time.Second), job, api)
		p := bedrocktest.NewProcess(t)

		begin := time.Now()
		st := app.Run(t.Context(), p.Process())

		if st.Code() != 0 || st.Cause() != nil {
			t.Errorf("status = (%d, %v), want (0, nil)", st.Code(), st.Cause())
		}
		if got := shutdownAt.Sub(begin); got != 5*time.Second {
			t.Errorf("Shutdown called %s after the job returned, want the drain's whole 5s", got)
		}
	})
}

func TestShutdownCause_after_a_SIGTERM_as_the_App_starts_serving_is_the_signals_cause(t *testing.T) {
	lost := &tally{t: t}
	for i := range tries {
		var st bedrock.ExitStatus
		var got error
		synctest.Test(t, func(t *testing.T) {
			d := &causeDrain{}
			p := bedrocktest.NewProcess(t)
			logs := sigtermAsServing(p, nil)
			// The first Service stops on Serve's context, as the health
			// Service does, and the second serves until Shutdown, as an
			// HTTP server does, so the Drain is called.
			app := drainApp(t, d.Drain, &fakeService{}, serving(nil))
			app.Telemetry = hookedTelemetry(logs, nil)

			st = app.Run(t.Context(), p.Process())
			got = d.recordedCause(t)
		})

		t.Logf("App %d: ShutdownCause %v, code %d, cause %v", i+1, got, st.Code(), st.Cause())
		if got == nil || got.Error() != "terminated signal received" || errors.Is(got, humus.ErrServiceReturned) {
			lost.fail(i, "ShutdownCause = %v, want terminated signal received", got)
		}
		if got != st.Cause() {
			t.Errorf("App %d: ShutdownCause = %v, want the App's cause, %v", i+1, got, st.Cause())
		}
	}
	lost.report("ShutdownCause was not the signal")
}

func TestApp_Run_a_Drain_that_waits_on_a_load_balancer_after_a_SIGTERM_as_the_App_starts_serving_waits_until_the_target_is_draining(t *testing.T) {
	cut := &tally{t: t}
	for i := range tries {
		var st bedrock.ExitStatus
		var shutdownAfter time.Duration
		var polls int
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			lb := &fakeLB{}
			serveLB(t, h, lb)
			var shutdownAt time.Time
			api := serving(func(context.Context) error {
				shutdownAt = time.Now()
				return nil
			})
			health := &fakeService{} // stops on Serve's context, as the health Service does
			p := bedrocktest.NewProcess(t)
			var sigterm time.Time
			logs := sigtermAsServing(p, func() {
				sigterm = time.Now()
				lb.deregister(3 * time.Second) // Kubernetes deregisters the pod as it sends SIGTERM
			})
			app := drainApp(t, deregisteringDrain, health, api)
			app.Host = h.Host()
			app.Telemetry = hookedTelemetry(logs, nil)

			st = app.Run(t.Context(), p.Process())
			shutdownAfter = shutdownAt.Sub(sigterm)
			polls = lb.pollCount()
		})

		t.Logf("App %d: Shutdown %s after SIGTERM, %d polls, code %d, cause %v", i+1, shutdownAfter, polls, st.Code(), st.Cause())
		if st.Code() != 0 {
			t.Errorf("App %d: code = %d, want 0: %v", i+1, st.Code(), st.Cause())
		}
		if shutdownAfter != 3*time.Second {
			cut.fail(i, "Shutdown called %s after SIGTERM, want 3s, once the load balancer reported the target draining", shutdownAfter)
		}
		if polls != 4 {
			t.Errorf("App %d: the Drain polled the load balancer %d times, want 4: at 0s, 1s, 2s and 3s", i+1, polls)
		}
	}
	cut.report("Shutdown was called before the load balancer reported the target draining")
}

func TestShutdownCause_after_Runs_context_was_cancelled_as_the_App_starts_serving_is_its_cause(t *testing.T) {
	errStop := errors.New("the test harness stopped the App")
	lost := &tally{t: t}
	for i := range tries {
		var st bedrock.ExitStatus
		var got error
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			d := &causeDrain{}
			logs := &hookLogger{onServing: func() {
				cancel(errStop)
				synctest.Wait() // a Serve that stops on its context has returned
			}}
			app := drainApp(t, d.Drain, &fakeService{}, serving(nil))
			app.Telemetry = hookedTelemetry(logs, nil)
			p := bedrocktest.NewProcess(t)

			st = app.Run(ctx, p.Process())
			got = d.recordedCause(t)
		})

		t.Logf("App %d: ShutdownCause %v, code %d, cause %v", i+1, got, st.Code(), st.Cause())
		if got != errStop {
			lost.fail(i, "ShutdownCause = %v, want Run's context's cause, %v", got, errStop)
		}
	}
	lost.report("ShutdownCause was not Run's context's cause")
}

func TestShutdownCause_after_a_Service_returned_on_its_own_before_a_SIGTERM_as_the_App_starts_serving_wraps_ErrServiceReturned(t *testing.T) {
	signalled := &tally{t: t}
	for i := range tries {
		var st bedrock.ExitStatus
		var got error
		synctest.Test(t, func(t *testing.T) {
			d := &causeDrain{}
			p := bedrocktest.NewProcess(t)
			logs := sigtermAfterReturned(p)
			job := &fakeService{serve: func(context.Context) error { return nil }}
			app := drainApp(t, d.Drain, job, serving(nil))
			app.Telemetry = hookedTelemetry(logs, nil)

			st = app.Run(t.Context(), p.Process())
			got = d.recordedCause(t)
		})

		if !errors.Is(got, humus.ErrServiceReturned) {
			signalled.fail(i, "ShutdownCause = %v, want it to wrap ErrServiceReturned: the Service returned before the signal", got)
		}
		if st.Code() != 0 || st.Cause() != nil {
			t.Errorf("App %d: status = (%d, %v), want (0, nil)", i+1, st.Code(), st.Cause())
		}
	}
	signalled.report("ShutdownCause did not wrap ErrServiceReturned")
}
