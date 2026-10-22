// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package stress runs a sweep: it builds every package's test binary, and
// once every build has finished, runs each package at each pair of CPU count
// and load, packed into lanes of cores, and counts the results.
package stress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/z5labs/humus/tools/flaky/lanes"
	"github.com/z5labs/humus/tools/flaky/results"
	"github.com/z5labs/humus/tools/flaky/run"
)

// Runner builds and runs the packages' test binaries. Packages are named by
// their index.
type Runner interface {
	// Build builds the test binary of the package pkg.
	Build(ctx context.Context, pkg int) error
	// List returns the top-level tests that the binary of the package pkg
	// lists. It is called only once the package is built.
	List(ctx context.Context, pkg int) ([]string, error)
	// Run runs the binary of the package pkg once, at CPU count cpu,
	// pinned to cores. Other runs may be pinned to the same cores, as many
	// as the run's load allows. It is called only once every package is
	// built.
	Run(ctx context.Context, pkg, cpu int, cores []int) run.Result
}

// Config is a sweep.
type Config struct {
	// Packages is how many packages there are.
	Packages int
	// CPUs are the CPU counts each package runs at.
	CPUs []int
	// Loads are the loads each package runs at: how many runs, at most,
	// are pinned to each of a run's cores.
	Loads []int
	// Count is how many times each package runs at each pair of CPU count
	// and load.
	Count int
	// Lanes are the cores runs are pinned to, split into lanes.
	Lanes [][]int
	// Builds is how many builds run at once.
	Builds int
	// Log is where warnings and progress are written.
	Log io.Writer
	// Progress is how often the sweep's progress is written to Log, or 0
	// for never.
	Progress time.Duration
}

// Phases are how long each phase of a sweep took.
type Phases struct {
	Build, Sweep time.Duration
}

// Stress runs the sweep cfg on r, and counts every run in res. It returns an
// error, and runs nothing, when a package doesn't build. When ctx is done, it
// stops starting runs, waits for those running, and returns ctx's error.
func Stress(ctx context.Context, cfg Config, r Runner, res *results.Results) (Phases, error) {
	var phases Phases
	start := time.Now()
	if err := build(ctx, cfg, r, res); err != nil {
		return phases, err
	}
	phases.Build = time.Since(start)
	logf(cfg.Log, "built %d test binaries in %s\n", cfg.Packages, phases.Build.Round(time.Millisecond))

	start = time.Now()
	err := sweep(ctx, cfg, r, res)
	phases.Sweep = time.Since(start)
	return phases, err
}

func build(ctx context.Context, cfg Config, r Runner, res *results.Results) error {
	sem := make(chan struct{}, max(cfg.Builds, 1))
	errs := make([]error, cfg.Packages)
	var wg sync.WaitGroup
	for pkg := range cfg.Packages {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := r.Build(ctx, pkg); err != nil {
				errs[pkg] = err
				return
			}
			tests, err := r.List(ctx, pkg)
			if err != nil {
				logf(cfg.Log, "warning: %v\n", err)
			}
			res.Listed(pkg, tests)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return &BuildError{err}
	}
	return nil
}

// BuildError reports that a package's test binary didn't build.
type BuildError struct{ Err error }

func (e *BuildError) Error() string { return e.Err.Error() }

func (e *BuildError) Unwrap() error { return e.Err }

func sweep(ctx context.Context, cfg Config, r Runner, res *results.Results) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	pairs := make([]lanes.Pair, 0, cfg.Packages*len(cfg.CPUs)*len(cfg.Loads))
	for pkg := range cfg.Packages {
		for _, cpu := range cfg.CPUs {
			for _, load := range cfg.Loads {
				pairs = append(pairs, lanes.Pair{Package: pkg, CPU: cpu, Load: load})
			}
		}
	}
	a := &adapter{r: r, res: res, cancel: cancel}
	total := len(pairs) * cfg.Count
	stop := a.progress(cfg, total)
	err := lanes.Schedule(ctx, cfg.Lanes, pairs, cfg.Count, a)
	stop()
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			return cause
		}
		return err
	}
	return nil
}

type adapter struct {
	r      Runner
	res    *results.Results
	cancel context.CancelCauseFunc

	mu           sync.Mutex
	done, failed int
}

func (a *adapter) Run(ctx context.Context, lr lanes.Run) {
	result := a.r.Run(ctx, lr.Pair.Package, lr.Pair.CPU, lr.Cores)
	if result.Cancelled {
		return
	}
	if err := a.res.Add(lr.Pair.Package, lr.Pair.CPU, lr.Pair.Load, result); err != nil {
		a.cancel(fmt.Errorf("saving a run's output: %w", err))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done++
	if result.Failed() {
		a.failed++
	}
}

// progress writes the sweep's progress to cfg.Log every cfg.Progress, until
// the returned func is called.
func (a *adapter) progress(cfg Config, total int) func() {
	if cfg.Progress <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(cfg.Progress)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				a.mu.Lock()
				logf(cfg.Log, "swept %d of %d runs, %d failed or hung\n", a.done, total, a.failed)
				a.mu.Unlock()
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

func logf(w io.Writer, format string, args ...any) {
	if w != nil {
		// A failed write to the log loses a line of progress, which
		// nothing depends on.
		_, _ = fmt.Fprintf(w, format, args...)
	}
}
