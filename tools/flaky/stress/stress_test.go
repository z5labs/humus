// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package stress_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/tools/flaky/discover"
	"github.com/z5labs/humus/tools/flaky/lanes"
	"github.com/z5labs/humus/tools/flaky/results"
	"github.com/z5labs/humus/tools/flaky/run"
	"github.com/z5labs/humus/tools/flaky/stress"
)

// fakeRunner stands in for the test binaries. Its builds and runs sleep, so
// inside a synctest bubble they take no real time, and it records when each
// happened and which cores each run was pinned to.
type fakeRunner struct {
	buildTook func(pkg int) time.Duration
	buildErr  map[int]error
	listErr   map[int]error
	runTook   func(pkg, cpu int) time.Duration
	fail      func(pkg, cpu int) bool

	mu         sync.Mutex
	lastBuilt  time.Time
	builds     int
	firstRun   time.Time
	runs       map[lanes.Pair]int
	pinned     map[int]int
	reuse      []string
	running    int
	maxRunning int
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		buildTook: func(int) time.Duration { return time.Second },
		runTook:   func(int, int) time.Duration { return 100 * time.Millisecond },
		fail:      func(int, int) bool { return false },
		runs:      map[lanes.Pair]int{},
		pinned:    map[int]int{},
	}
}

func (f *fakeRunner) Build(ctx context.Context, pkg int) error {
	time.Sleep(f.buildTook(pkg))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds++
	if now := time.Now(); now.After(f.lastBuilt) {
		f.lastBuilt = now
	}
	return f.buildErr[pkg]
}

func (f *fakeRunner) List(ctx context.Context, pkg int) ([]string, error) {
	return []string{"TestA"}, f.listErr[pkg]
}

func (f *fakeRunner) Run(ctx context.Context, pkg, cpu int, cores []int) run.Result {
	f.mu.Lock()
	if f.firstRun.IsZero() {
		f.firstRun = time.Now()
	}
	f.runs[lanes.Pair{Package: pkg, CPU: cpu}]++
	for _, c := range cores {
		f.pinned[c]++
		if f.pinned[c] > 1 {
			f.reuse = append(f.reuse, fmt.Sprintf("core %d", c))
		}
	}
	f.running++
	f.maxRunning = max(f.maxRunning, f.running)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range cores {
			f.pinned[c]--
		}
		f.running--
	}()

	select {
	case <-time.After(f.runTook(pkg, cpu)):
	case <-ctx.Done():
		return run.Result{Cancelled: true}
	}
	outcome := run.Pass
	if f.fail(pkg, cpu) {
		outcome = run.Fail
	}
	return run.Result{Tests: []run.Test{{Name: "TestA", Outcome: outcome}}}
}

func newResults(t *testing.T, n int, cpus []int, count int) *results.Results {
	pkgs := make([]discover.Package, n)
	for i := range pkgs {
		pkgs[i] = discover.Package{Module: ".", Dir: fmt.Sprintf("p%d", i), ImportPath: fmt.Sprintf("example.com/p%d", i)}
	}
	return results.New(results.Config{Packages: pkgs, CPUs: cpus, Count: count, Out: t.TempDir()})
}

func TestStress_starts_no_run_before_every_build_has_finished(t *testing.T) {
	const n, count = 7, 5
	cpus := []int{1, 2, 4}
	lanesOf, err := lanes.Split(cores(24), 4, 4)
	require.NoError(t, err)
	rng := rand.New(rand.NewPCG(1, 2))
	buildTook := make([]time.Duration, n)
	for i := range buildTook {
		buildTook[i] = time.Duration(1+rng.IntN(60)) * time.Second
	}
	f := newFakeRunner()
	f.buildTook = func(pkg int) time.Duration { return buildTook[pkg] }
	f.fail = func(pkg, cpu int) bool { return pkg == 3 && cpu == 2 }
	res := newResults(t, n, cpus, count)
	var phases stress.Phases
	var stressErr error
	synctest.Test(t, func(t *testing.T) {
		phases, stressErr = stress.Stress(t.Context(), stress.Config{
			Packages: n, CPUs: cpus, Count: count, Lanes: lanesOf, Builds: 3,
		}, f, res)
	})
	require.NoError(t, stressErr)
	require.Equal(t, n, f.builds)
	require.False(t, f.firstRun.Before(f.lastBuilt), "a run started at %s, before the last build finished at %s", f.firstRun, f.lastBuilt)
	require.Empty(t, f.reuse, "no core ever has two runs pinned to it")
	require.Len(t, f.runs, n*len(cpus))
	for pair, runs := range f.runs {
		require.Equal(t, count, runs, "runs of %+v", pair)
	}
	require.Positive(t, phases.Build)
	require.Positive(t, phases.Sweep)

	rep := res.Report()
	require.Len(t, rep.Failing, 1)
	require.Equal(t, "p3", rep.Failing[0].Package)
	require.Equal(t, n-1, rep.Passed)
}

func cores(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func TestStress_runs_nothing_when_a_package_does_not_build(t *testing.T) {
	f := newFakeRunner()
	f.buildErr = map[int]error{1: errors.New("p1 does not compile"), 2: errors.New("p2 does not compile")}
	var err error
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Packages: 3, CPUs: []int{1}, Count: 1, Lanes: [][]int{{0}},
		}, f, newResults(t, 3, []int{1}, 1))
	})
	var buildErr *stress.BuildError
	require.ErrorAs(t, err, &buildErr)
	require.ErrorContains(t, err, "p1 does not compile")
	require.ErrorContains(t, err, "p2 does not compile")
	require.Equal(t, 3, f.builds, "every package is built, so every failure is reported")
	require.Empty(t, f.runs)
}

func TestStress_warns_of_a_binary_that_does_not_list_its_tests_and_runs_it(t *testing.T) {
	f := newFakeRunner()
	f.listErr = map[int]error{0: errors.New("TestMain failed")}
	var log bytes.Buffer
	var err error
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Packages: 1, CPUs: []int{1}, Count: 2, Lanes: [][]int{{0}}, Log: &log,
		}, f, newResults(t, 1, []int{1}, 2))
	})
	require.NoError(t, err)
	require.Contains(t, log.String(), "warning: TestMain failed")
	require.Contains(t, log.String(), "built 1 test binaries")
	require.Equal(t, 2, f.runs[lanes.Pair{Package: 0, CPU: 1}])
}

func TestStress_writes_its_progress(t *testing.T) {
	f := newFakeRunner()
	f.runTook = func(int, int) time.Duration { return time.Minute }
	f.fail = func(int, int) bool { return true }
	var log bytes.Buffer
	synctest.Test(t, func(t *testing.T) {
		_, err := stress.Stress(t.Context(), stress.Config{
			Packages: 1, CPUs: []int{1}, Count: 3, Lanes: [][]int{{0}}, Log: &log, Progress: 90 * time.Second,
		}, f, newResults(t, 1, []int{1}, 3))
		require.NoError(t, err)
	})
	require.Contains(t, log.String(), "swept 1 of 3 runs, 1 failed or hung\n")
	require.Contains(t, log.String(), "swept 2 of 3 runs, 2 failed or hung\n")
}

func TestStress_stops_when_its_context_is_done_and_counts_no_cancelled_run(t *testing.T) {
	f := newFakeRunner()
	f.runTook = func(int, int) time.Duration { return time.Minute }
	f.fail = func(int, int) bool { return true }
	res := newResults(t, 1, []int{1}, 10)
	var err error
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
		defer cancel()
		_, err = stress.Stress(ctx, stress.Config{
			Packages: 1, CPUs: []int{1}, Count: 10, Lanes: [][]int{{0}},
		}, f, res)
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 3, f.runs[lanes.Pair{Package: 0, CPU: 1}])
	rep := res.Report()
	require.Len(t, rep.Failing, 1)
	require.Equal(t, []results.CPU{{CPU: 1, Runs: 2, Failures: 2}}, rep.Failing[0].CPUs, "the run cut short isn't counted")
}

func TestStress_stops_when_a_run_s_output_cannot_be_saved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	res := results.New(results.Config{
		Packages: []discover.Package{{Module: ".", Dir: "p", ImportPath: "example.com/p"}},
		CPUs:     []int{1}, Count: 5, Out: file,
	})
	f := newFakeRunner()
	f.fail = func(int, int) bool { return true }
	var err error
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Packages: 1, CPUs: []int{1}, Count: 5, Lanes: [][]int{{0}},
		}, f, res)
	})
	require.ErrorContains(t, err, "saving a run's output")
	require.Equal(t, 1, f.runs[lanes.Pair{Package: 0, CPU: 1}])
}

func TestBuildError_unwraps(t *testing.T) {
	cause := errors.New("cause")
	err := &stress.BuildError{Err: cause}
	require.ErrorIs(t, err, cause)
	require.Equal(t, "cause", err.Error())
}
