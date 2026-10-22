// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package stress runs a sweep: it builds every package's test binary, and
// once every build has finished, runs each package at each pair of CPU count
// and load, packed into lanes of cores, and counts the results. Then, once
// every sweep run has ended, it confirms each test that failed or hung: it runs
// the test alone at each pair, many more times.
//
// Each package is at a level, which sets how many times its runs and confirm
// runs go, and may cap how many of them go at once.
package stress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
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
	// runs. It is called only once every package is built.
	List(ctx context.Context, pkg int) ([]string, error)
	// Run runs the binary of the package pkg once, at CPU count cpu,
	// pinned to cores. Other runs may be pinned to the same cores, as many
	// as the run's load allows. A sweep run has test "", and runs the
	// package's tests; a confirm run names the one test it runs alone, by
	// that test's own -run pattern. It is called only once every package
	// is built.
	Run(ctx context.Context, pkg int, test string, cpu int, cores []int) run.Result
}

// Level is how the packages at one level run.
type Level struct {
	// Name is the level's name, which the log gives.
	Name string
	// Count is how many times each of its packages runs at each pair of
	// CPU count and load.
	Count int
	// Confirm is how many times each of its tests that failed or hung in
	// the sweep runs alone at each pair of CPU count and load, once every
	// sweep run has ended, or 0 for no confirm runs.
	Confirm int
	// Limit caps how many of its runs go at once, or is 0 for no cap.
	Limit int
	// Own reports that its packages run only tests of their own, so a
	// package whose binary lists none, or can't be listed, doesn't run.
	Own bool
}

// Config is a sweep.
type Config struct {
	// Levels are the packages' levels.
	Levels []Level
	// Packages are the index in Levels of each package's level, one for
	// each package.
	Packages []int
	// CPUs are the CPU counts each package runs at.
	CPUs []int
	// Loads are the loads each package runs at: how many runs, at most,
	// are pinned to each of a run's cores.
	Loads []int
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
	Build, Sweep, Confirm time.Duration
}

// Stress runs the sweep cfg on r, and counts every run in res. It returns an
// error, and runs nothing, when a package doesn't build. When ctx is done, it
// stops starting runs, waits for those running, and returns ctx's error.
//
// A test that fails or hangs in a sweep run is added to the confirm runs at
// once, but no confirm run starts until every sweep run has ended.
func Stress(ctx context.Context, cfg Config, r Runner, res *results.Results) (Phases, error) {
	var phases Phases
	start := time.Now()
	runs, err := build(ctx, cfg, r, res)
	if err != nil {
		return phases, err
	}
	phases.Build = time.Since(start)
	logf(cfg.Log, "built %d test binaries in %s\n", len(cfg.Packages), phases.Build.Round(time.Millisecond))

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	a := &adapter{r: r, res: res, cancel: cancel, cfg: cfg, targets: map[target]bool{}}

	var tasks []lanes.Task
	for pkg, level := range cfg.Packages {
		if runs[pkg] {
			tasks = append(tasks, atEachPair(cfg, lanes.Pair{Package: pkg, Level: level}, cfg.Levels[level].Count)...)
		}
	}
	start = time.Now()
	err = a.schedule(ctx, cfg, "swept", tasks)
	phases.Sweep = time.Since(start)
	if err != nil || !slices.ContainsFunc(cfg.Levels, func(l Level) bool { return l.Confirm > 0 }) {
		return phases, err
	}

	// Every sweep run has ended: this is the barrier the confirm runs wait
	// on.
	targets := a.confirmTargets()
	if len(targets) == 0 {
		return phases, nil
	}
	tasks = tasks[:0]
	confirming := make([]int, len(cfg.Levels))
	for _, t := range targets {
		level := cfg.Packages[t.pkg]
		confirming[level]++
		tasks = append(tasks, atEachPair(cfg, lanes.Pair{Package: t.pkg, Test: t.test, Level: level}, cfg.Levels[level].Confirm)...)
	}
	var parts []string
	for i, n := range confirming {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s tests, %d times", n, cfg.Levels[i].Name, cfg.Levels[i].Confirm))
		}
	}
	logf(cfg.Log, "confirming %s at each pair of CPU count and load\n", strings.Join(parts, ", and "))
	start = time.Now()
	err = a.schedule(ctx, cfg, "confirmed", tasks)
	phases.Confirm = time.Since(start)
	return phases, err
}

// atEachPair returns p at each pair of cfg's CPU counts and loads, each to
// run count times.
func atEachPair(cfg Config, p lanes.Pair, count int) []lanes.Task {
	tasks := make([]lanes.Task, 0, len(cfg.CPUs)*len(cfg.Loads))
	for _, cpu := range cfg.CPUs {
		for _, load := range cfg.Loads {
			p.CPU, p.Load = cpu, load
			tasks = append(tasks, lanes.Task{Pair: p, Count: count})
		}
	}
	return tasks
}

// build builds every package, and once every one is built, lists each one's
// tests. It reports which packages run: those at a level that runs only its
// own tests run only when they list some.
func build(ctx context.Context, cfg Config, r Runner, res *results.Results) ([]bool, error) {
	sem := make(chan struct{}, max(cfg.Builds, 1))
	errs := make([]error, len(cfg.Packages))
	var wg sync.WaitGroup
	for pkg := range cfg.Packages {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[pkg] = r.Build(ctx, pkg)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, &BuildError{err}
	}

	runs := make([]bool, len(cfg.Packages))
	for pkg, level := range cfg.Packages {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			tests, err := r.List(ctx, pkg)
			if err != nil {
				logf(cfg.Log, "warning: %v\n", err)
			}
			res.Listed(pkg, tests)
			runs[pkg] = !cfg.Levels[level].Own || len(tests) > 0
		})
	}
	wg.Wait()
	return runs, nil
}

// BuildError reports that a package's test binary didn't build.
type BuildError struct{ Err error }

func (e *BuildError) Error() string { return e.Err.Error() }

func (e *BuildError) Unwrap() error { return e.Err }

// schedule runs every task, and writes its progress as what.
func (a *adapter) schedule(ctx context.Context, cfg Config, what string, tasks []lanes.Task) error {
	a.mu.Lock()
	a.done, a.failed = 0, 0
	a.mu.Unlock()
	total := 0
	for _, t := range tasks {
		total += t.Count
	}
	limits := map[int]int{}
	for i, l := range cfg.Levels {
		limits[i] = l.Limit
	}
	stop := a.progress(cfg, what, total)
	err := lanes.Schedule(ctx, cfg.Lanes, tasks, limits, a)
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
	cfg    Config

	mu           sync.Mutex
	done, failed int
	// targets are the tests that failed or hung in a sweep run, which
	// are confirmed once every sweep run has ended.
	targets map[target]bool
}

// target is a test to confirm.
type target struct {
	pkg  int
	test string
}

func (a *adapter) Run(ctx context.Context, lr lanes.Run) {
	p := lr.Pair
	result := a.r.Run(ctx, p.Package, p.Test, p.CPU, lr.Cores)
	if result.Cancelled {
		return
	}
	var err error
	if p.Test == "" {
		err = a.res.Add(p.Package, p.CPU, p.Load, result)
	} else {
		err = a.res.Confirmed(p.Package, p.Test, p.CPU, p.Load, result)
	}
	if err != nil {
		a.cancel(fmt.Errorf("saving a run's output: %w", err))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done++
	if result.Failed() {
		a.failed++
	}
	if p.Test != "" || a.cfg.Levels[p.Level].Confirm == 0 {
		return
	}
	// A failure outside any test has no test to run alone, so it isn't
	// confirmed.
	for _, t := range result.Tests {
		if t.Outcome == run.Fail || t.Outcome == run.Hang {
			a.targets[target{p.Package, t.Name}] = true
		}
	}
}

// confirmTargets returns the tests to confirm, by package and then by name.
func (a *adapter) confirmTargets() []target {
	a.mu.Lock()
	defer a.mu.Unlock()
	targets := make([]target, 0, len(a.targets))
	for t := range a.targets {
		targets = append(targets, t)
	}
	slices.SortFunc(targets, func(x, y target) int {
		if x.pkg != y.pkg {
			return x.pkg - y.pkg
		}
		return strings.Compare(x.test, y.test)
	})
	return targets
}

// progress writes the progress of a pass of total runs to cfg.Log, as what,
// every cfg.Progress, until the returned func is called.
func (a *adapter) progress(cfg Config, what string, total int) func() {
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
				logf(cfg.Log, "%s %d of %d runs, %d failed or hung\n", what, a.done, total, a.failed)
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
