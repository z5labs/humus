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
//
// A walkthrough's functional tests run on a kind cluster of its own. Its image
// is built in the build phase, beside the test binaries. Its cluster holds
// ClusterCPU cores of one lane at load 1, from the moment it comes up until it
// is deleted, and is one of its level's runs, so the level's cap is how many
// clusters are up at once. Clusters start before every other run. A cluster
// serves one run at a time, each pinned to its cores at CPU count ClusterCPU,
// since a run that rolls the walkthrough's Deployments out would be seen by
// another. A test of a walkthrough that failed or hung gets a fresh cluster for
// its confirm runs.
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

// ClusterCPU is the CPU count of a cluster's runs, and how many cores of its
// lane a cluster holds: a GitHub runner's.
const ClusterCPU = 4

// Runner builds and runs the packages' test binaries, and builds the
// walkthroughs' images and brings their clusters up. Packages and
// walkthroughs are named by their index.
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
	// Image builds the image of the walkthrough w, in the build phase.
	Image(ctx context.Context, w int) error
	// Up brings a fresh cluster up for the walkthrough w, its node pinned
	// to cores, and deploys the walkthrough onto it. The runs of its
	// packages follow, on the same cores, until Down.
	Up(ctx context.Context, w int, cores []int) error
	// Down deletes the cluster of the walkthrough w. It follows every Up,
	// whether or not Up succeeded, and must delete the cluster though ctx
	// is done.
	Down(ctx context.Context, w int) error
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
	// Walkthroughs are, for each package, the index of the walkthrough on
	// whose cluster it runs, or -1 when it needs no cluster. Nil means no
	// package does. A walkthrough's packages are at one level, whose
	// Limit caps how many clusters are up at once.
	Walkthroughs []int
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

// walkthrough returns the index of the walkthrough on whose cluster the
// package pkg runs, or -1.
func (cfg Config) walkthrough(pkg int) int {
	if cfg.Walkthroughs == nil {
		return -1
	}
	return cfg.Walkthroughs[pkg]
}

// Phases are how long each phase of a sweep took.
type Phases struct {
	Build, Sweep, Confirm time.Duration
}

// Stress runs the sweep cfg on r, and counts every run in res. It returns an
// error, and runs nothing, when a package doesn't build. When ctx is done, it
// stops starting runs, waits for those running, deletes the clusters that are
// up, and returns ctx's error. res then holds every run that ended, and how
// many were planned, and a run cut short is counted as neither a pass nor a
// failure.
//
// A test that fails or hangs in a sweep run is added to the confirm runs at
// once, but no confirm run starts until every sweep run has ended, and none
// starts once ctx is done. The confirm runs are planned in res, and each test
// to confirm is marked in res, only when they are about to start.
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
	onCluster := map[int][]clusterRun{}
	for pkg, level := range cfg.Packages {
		switch w := cfg.walkthrough(pkg); {
		case !runs[pkg]:
		case w >= 0:
			onCluster[w] = append(onCluster[w], clusterRun{pkg: pkg})
		default:
			tasks = append(tasks, atEachPair(cfg, lanes.Pair{Package: pkg, Level: level}, cfg.Levels[level].Count)...)
		}
	}
	// A walkthrough's sweep is one cluster, which runs each of its
	// packages in turn, its level's count times over.
	sessions := map[int][][]clusterRun{}
	for w, pkgs := range onCluster {
		count := cfg.Levels[cfg.Packages[pkgs[0].pkg]].Count
		sessions[w] = [][]clusterRun{slices.Repeat(pkgs, count)}
	}
	tasks = append(tasks, a.clusterTasks(sessions)...)
	start = time.Now()
	err = a.schedule(ctx, cfg, "swept", tasks)
	phases.Sweep = time.Since(start)
	if err != nil || !slices.ContainsFunc(cfg.Levels, func(l Level) bool { return l.Confirm > 0 }) {
		return phases, err
	}

	// Every sweep run has ended: this is the barrier the confirm runs wait
	// on.
	if err := ctx.Err(); err != nil {
		return phases, err
	}
	targets := a.confirmTargets()
	if len(targets) == 0 {
		return phases, nil
	}
	tasks = tasks[:0]
	sessions = map[int][][]clusterRun{}
	confirming := make([]int, len(cfg.Levels))
	for _, t := range targets {
		res.Confirming(t.pkg, t.test)
		level := cfg.Packages[t.pkg]
		confirming[level]++
		if w := cfg.walkthrough(t.pkg); w >= 0 {
			// The test gets a fresh cluster for its confirm runs.
			sessions[w] = append(sessions[w], slices.Repeat([]clusterRun{{pkg: t.pkg, test: t.test}}, cfg.Levels[level].Confirm))
			continue
		}
		tasks = append(tasks, atEachPair(cfg, lanes.Pair{Package: t.pkg, Test: t.test, Level: level}, cfg.Levels[level].Confirm)...)
	}
	tasks = append(tasks, a.clusterTasks(sessions)...)
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

// build builds every package, and the image of every walkthrough a package
// runs on the cluster of, and once every one is built, lists each package's
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
	var walkthroughs []int
	for pkg := range cfg.Packages {
		if w := cfg.walkthrough(pkg); w >= 0 && !slices.Contains(walkthroughs, w) {
			walkthroughs = append(walkthroughs, w)
		}
	}
	imageErrs := make([]error, len(walkthroughs))
	for i, w := range walkthroughs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			imageErrs[i] = r.Image(ctx, w)
		})
	}
	wg.Wait()
	errs = append(errs, imageErrs...)
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

// clusterTasks returns a task for each walkthrough in sessions, whose one run
// brings up a fresh cluster for each of the walkthrough's sessions in turn,
// and runs the session's runs on it, one at a time. The task goes first, and
// holds ClusterCPU cores of a lane at load 1. It sets the adapter's clusters
// to sessions' tasks.
func (a *adapter) clusterTasks(sessions map[int][][]clusterRun) []lanes.Task {
	a.clusters = map[int]cluster{}
	var tasks []lanes.Task
	for w, ss := range sessions {
		// The task is named by the first package it runs, so that no
		// two tasks have the same pair.
		pkg := ss[0][0].pkg
		a.clusters[pkg] = cluster{walkthrough: w, sessions: ss}
		tasks = append(tasks, lanes.Task{
			Pair:  lanes.Pair{Package: pkg, CPU: ClusterCPU, Load: 1, Level: a.cfg.Packages[pkg]},
			Count: 1,
			First: true,
		})
	}
	slices.SortFunc(tasks, func(x, y lanes.Task) int { return x.Pair.Package - y.Pair.Package })
	return tasks
}

// schedule runs every task, and writes its progress as what.
func (a *adapter) schedule(ctx context.Context, cfg Config, what string, tasks []lanes.Task) error {
	a.mu.Lock()
	a.done, a.failed = 0, 0
	a.mu.Unlock()
	total := 0
	for _, t := range tasks {
		c, ok := a.clusters[t.Pair.Package]
		if !ok || cfg.walkthrough(t.Pair.Package) < 0 {
			total += t.Count
			continue
		}
		for _, session := range c.sessions {
			total += len(session)
		}
	}
	limits := map[int]int{}
	for i, l := range cfg.Levels {
		limits[i] = l.Limit
	}
	a.res.Planned(total)
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

	// clusters are the clusters of the pass running, by the package that
	// names each one's task.
	clusters map[int]cluster

	mu           sync.Mutex
	done, failed int
	// targets are the tests that failed or hung in a sweep run, which
	// are confirmed once every sweep run has ended.
	targets map[target]bool
}

// cluster is the work of a walkthrough's cluster in a pass: each session is
// a fresh cluster, and the runs it serves, in order.
type cluster struct {
	walkthrough int
	sessions    [][]clusterRun
}

// clusterRun is one run on a cluster: of the package pkg's tests, when test
// is "", or of test alone.
type clusterRun struct {
	pkg  int
	test string
}

// target is a test to confirm.
type target struct {
	pkg  int
	test string
}

func (a *adapter) Run(ctx context.Context, lr lanes.Run) {
	if c, ok := a.clusters[lr.Pair.Package]; ok && a.cfg.walkthrough(lr.Pair.Package) >= 0 {
		a.cluster(ctx, c, lr)
		return
	}
	p := lr.Pair
	a.record(p, a.r.Run(ctx, p.Package, p.Test, p.CPU, lr.Cores))
}

// cluster brings up a fresh cluster on lr's cores for each of c's sessions,
// runs the session's runs on it one at a time, and deletes it. A cluster that
// doesn't come up is recorded, and its session's runs don't run.
func (a *adapter) cluster(ctx context.Context, c cluster, lr lanes.Run) {
	for _, session := range c.sessions {
		if ctx.Err() != nil {
			return
		}
		if err := a.r.Up(ctx, c.walkthrough, lr.Cores); err != nil {
			if ctx.Err() == nil {
				if err := a.res.UpFailed(c.walkthrough, err); err != nil {
					a.cancel(fmt.Errorf("saving a cluster's output: %w", err))
				}
			}
		} else {
			for _, cr := range session {
				if ctx.Err() != nil {
					break
				}
				p := lanes.Pair{Package: cr.pkg, Test: cr.test, CPU: ClusterCPU, Load: 1, Level: lr.Pair.Level}
				a.record(p, a.r.Run(ctx, cr.pkg, cr.test, ClusterCPU, lr.Cores))
			}
		}
		if err := a.r.Down(ctx, c.walkthrough); err != nil {
			logf(a.cfg.Log, "warning: %v\n", err)
		}
	}
}

// record counts a run of the pair p that ended with result.
func (a *adapter) record(p lanes.Pair, result run.Result) {
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
