// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
)

// ticker is a Service that does a unit of work every interval, until
// Shutdown stops it or Serve's context is done.
type ticker struct {
	interval time.Duration

	mu      sync.Mutex
	started bool
	stop    chan struct{}
	once    sync.Once
	done    chan struct{}
}

func newTicker(interval time.Duration) *ticker {
	return &ticker{
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (tk *ticker) Serve(ctx context.Context) error {
	tk.mu.Lock()
	tk.started = true
	tk.mu.Unlock()
	defer close(tk.done)

	t := time.NewTicker(tk.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tk.stop:
			return nil
		case <-t.C:
		}
	}
}

func (tk *ticker) Shutdown(ctx context.Context) error {
	tk.mu.Lock()
	started := tk.started
	tk.mu.Unlock()

	// A ticker that has already stopped, or never started, has nothing
	// left to stop.
	select {
	case <-tk.done:
		return nil
	default:
	}
	tk.once.Do(func() { close(tk.stop) })
	if !started {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ticker: %w", err)
	}
	select {
	case <-tk.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ticker: %w", ctx.Err())
	}
}

// A package that provides a Service calls TestService from a test of its
// own, in a _test.go file, with a function that builds the Service. The
// function may register OnStop functions on the Setup, such as closing a
// pool, and TestService runs them through the App lifecycle.
//
// The ticker above is the Service. testTicker is the test a package writes,
// as func TestTicker(t *testing.T) in its ticker_test.go. This example has
// no Output comment, so it is compiled but never run; humustest's
// TestTicker runs the same test.
func ExampleTestService() {
	testTicker := func(t *testing.T) {
		humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
			return newTicker(time.Second)
		})
	}

	_ = testTicker
}
