// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"time"
)

// Drain waits, once the App has started shutting down, until it is safe to
// stop the Services: until load balancers have stopped sending the App new
// work. Run calls it after Serve's context is done and before it calls
// Shutdown on the Services, with the Setup that Init was given.
//
// Run calls it when, as shutdown starts, it hasn't yet received every
// Serve's result. ctx is done once every Service's Serve has returned,
// because nothing is left to drain, and it may be done as soon as Run calls
// the Drain, so a Drain checks it before it acts. A Drain returns when it is
// done or when ctx is done, whichever comes first, and returns nil when ctx
// is done: one that returns ctx.Err() then fails the App.
//
// Run waits for the Drain to return, and the grace period starts after it.
// An error it returns is joined into the App's cause and the App exits
// ExitFailure, and Shutdown is still called on every Service. ctx keeps the
// values of Run's context but is not cancelled with it.
type Drain func(ctx context.Context, s *Setup) error

// DrainDelay returns a Drain that waits d, or until ctx is done, and returns
// nil either way. It panics if d is negative.
func DrainDelay(d time.Duration) Drain {
	if d < 0 {
		panic("humus: DrainDelay called with a negative duration")
	}
	return func(ctx context.Context, _ *Setup) error {
		if d == 0 {
			return nil
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
		return nil
	}
}

// drain calls d with setup and a context that keeps ctx's values, is not
// cancelled with it, and is done once pending more results have arrived on
// served. While d runs, drain receives those results. It returns the errors
// they carried, how many results are still pending, and d's error.
func drain(ctx context.Context, d Drain, setup *Setup, served <-chan error, pending int) ([]error, int, error) {
	drainCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()

	type received struct {
		errs    []error
		pending int
	}
	stop := make(chan struct{})
	result := make(chan received, 1)
	go func() {
		var errs []error
		for pending > 0 {
			select {
			case err := <-served:
				pending--
				errs = appendServeErr(errs, err)
			case <-stop:
				result <- received{errs: errs, pending: pending}
				return
			}
		}
		cancel()
		result <- received{errs: errs}
	}()

	// d runs on Run's goroutine, so a panic in it reaches Run's caller. stop
	// is closed either way, so the goroutine above ends.
	err := func() error {
		defer close(stop)
		return d(drainCtx, setup)
	}()
	r := <-result
	return r.errs, r.pending, err
}
