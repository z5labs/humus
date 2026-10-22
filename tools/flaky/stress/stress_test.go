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
// happened and how many runs were pinned to each core at once.
//
// Each package has the one test TestA. A sweep run fails it when fail says
// so, hangs it when hang does, and fails outside any test when packageFail
// does. A confirm run fails it when confirmFail says so.
type fakeRunner struct {
	buildTook   func(pkg int) time.Duration
	buildErr    map[int]error
	listErr     map[int]error
	runTook     func(pkg, cpu int) time.Duration
	fail        func(pkg, cpu int) bool
	hang        func(pkg, cpu int) bool
	packageFail func(pkg, cpu int) bool
	confirmFail func(pkg, cpu int) bool

	mu           sync.Mutex
	lastBuilt    time.Time
	builds       int
	firstRun     time.Time
	runs         map[pkgCPU]int
	lastSweepEnd time.Time
	firstConfirm time.Time
	confirms     map[confirmRun]int
	pinned       map[int]int
	maxPinned    int
	running      int
	maxRunning   int
}

// confirmRun is a confirm run of one test at a CPU count.
type confirmRun struct {
	pkg  int
	test string
	cpu  int
}

// pkgCPU is a package at a CPU count. A Runner isn't told a run's load, so
// the fake counts its runs by these, over every load.
type pkgCPU struct {
	pkg, cpu int
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		buildTook: func(int) time.Duration { return time.Second },
		runTook:   func(int, int) time.Duration { return 100 * time.Millisecond },
		fail:        func(int, int) bool { return false },
		hang:        func(int, int) bool { return false },
		packageFail: func(int, int) bool { return false },
		confirmFail: func(int, int) bool { return false },
		runs:        map[pkgCPU]int{},
		confirms:    map[confirmRun]int{},
		pinned:      map[int]int{},
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

func (f *fakeRunner) Run(ctx context.Context, pkg int, test string, cpu int, cores []int) run.Result {
	f.mu.Lock()
	if f.firstRun.IsZero() {
		f.firstRun = time.Now()
	}
	if test == "" {
		f.runs[pkgCPU{pkg, cpu}]++
	} else {
		if f.firstConfirm.IsZero() {
			f.firstConfirm = time.Now()
		}
		f.confirms[confirmRun{pkg, test, cpu}]++
	}
	for _, c := range cores {
		f.pinned[c]++
		f.maxPinned = max(f.maxPinned, f.pinned[c])
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
		if now := time.Now(); test == "" && now.After(f.lastSweepEnd) {
			f.lastSweepEnd = now
		}
	}()

	select {
	case <-time.After(f.runTook(pkg, cpu)):
	case <-ctx.Done():
		return run.Result{Cancelled: true}
	}
	outcome := run.Pass
	switch {
	case test != "" && f.confirmFail(pkg, cpu):
		outcome = run.Fail
	case test != "":
	case f.hang(pkg, cpu):
		outcome = run.Hang
	case f.fail(pkg, cpu):
		outcome = run.Fail
	case f.packageFail(pkg, cpu):
		return run.Result{Tests: []run.Test{{Name: "TestA"}}, PackageFailed: true}
	}
	return run.Result{Tests: []run.Test{{Name: "TestA", Outcome: outcome}}}
}

func newResults(t *testing.T, n int, cpus, loads []int, count int) *results.Results {
	return newConfirmResults(t, n, cpus, loads, count, 0)
}

func newConfirmResults(t *testing.T, n int, cpus, loads []int, count, confirm int) *results.Results {
	pkgs := make([]discover.Package, n)
	for i := range pkgs {
		pkgs[i] = discover.Package{Module: ".", Dir: fmt.Sprintf("p%d", i), ImportPath: fmt.Sprintf("example.com/p%d", i)}
	}
	return results.New(results.Config{Packages: pkgs, CPUs: cpus, Loads: loads, Count: count, Confirm: confirm, Out: t.TempDir()})
}

func TestStress_starts_no_run_before_every_build_has_finished_and_runs_each_pair_count_times(t *testing.T) {
	const n, count = 7, 5
	cpus := []int{1, 2, 4}
	loads := []int{1, 2}
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
	res := newResults(t, n, cpus, loads, count)
	var phases stress.Phases
	var stressErr error
	synctest.Test(t, func(t *testing.T) {
		phases, stressErr = stress.Stress(t.Context(), stress.Config{
			Packages: n, CPUs: cpus, Loads: loads, Count: count, Lanes: lanesOf, Builds: 3,
		}, f, res)
	})
	require.NoError(t, stressErr)
	require.Equal(t, n, f.builds)
	require.False(t, f.firstRun.Before(f.lastBuilt), "a run started at %s, before the last build finished at %s", f.firstRun, f.lastBuilt)
	require.Equal(t, 2, f.maxPinned, "runs at load 2 share their cores, and no core has more than 2")
	require.Len(t, f.runs, n*len(cpus))
	for pair, runs := range f.runs {
		require.Equal(t, count*len(loads), runs, "runs of %+v", pair)
	}
	require.Positive(t, phases.Build)
	require.Positive(t, phases.Sweep)

	rep := res.Report()
	require.Len(t, rep.Failing, 1)
	require.Equal(t, "p3", rep.Failing[0].Package)
	require.Equal(t, []results.Pair{
		{CPU: 1, Load: 1, Runs: count}, {CPU: 1, Load: 2, Runs: count},
		{CPU: 2, Load: 1, Runs: count, Failures: count}, {CPU: 2, Load: 2, Runs: count, Failures: count},
		{CPU: 4, Load: 1, Runs: count}, {CPU: 4, Load: 2, Runs: count},
	}, rep.Failing[0].Pairs, "each pair of CPU count and load ran count times")
	require.Equal(t, n-1, rep.Passed)
}

func TestStress_confirms_each_test_that_failed_or_hung_only_once_every_sweep_run_has_ended(t *testing.T) {
	const n, count, confirm = 6, 4, 7
	cpus := []int{1, 2, 4}
	loads := []int{1, 2}
	lanesOf, err := lanes.Split(cores(24), 4, 4)
	require.NoError(t, err)
	rng := rand.New(rand.NewPCG(3, 4))
	took := map[pkgCPU]time.Duration{}
	for pkg := range n {
		for _, cpu := range cpus {
			took[pkgCPU{pkg, cpu}] = time.Duration(1+rng.IntN(90)) * time.Second
		}
	}
	f := newFakeRunner()
	f.runTook = func(pkg, cpu int) time.Duration { return took[pkgCPU{pkg, cpu}] }
	// p1 fails at cpu 2 and p4 hangs at cpu 1, both from their first runs,
	// long before the last sweep run ends. p5 fails outside any test.
	f.fail = func(pkg, cpu int) bool { return pkg == 1 && cpu == 2 }
	f.hang = func(pkg, cpu int) bool { return pkg == 4 && cpu == 1 }
	f.packageFail = func(pkg, cpu int) bool { return pkg == 5 }
	f.confirmFail = func(pkg, cpu int) bool { return pkg == 1 && cpu == 4 }
	res := newConfirmResults(t, n, cpus, loads, count, confirm)
	var log bytes.Buffer
	var phases stress.Phases
	var stressErr error
	synctest.Test(t, func(t *testing.T) {
		phases, stressErr = stress.Stress(t.Context(), stress.Config{
			Packages: n, CPUs: cpus, Loads: loads, Count: count, Confirm: confirm, Lanes: lanesOf, Builds: 3, Log: &log,
		}, f, res)
	})
	require.NoError(t, stressErr)
	require.False(t, f.firstConfirm.IsZero(), "a confirm ran")
	require.False(t, f.firstConfirm.Before(f.lastSweepEnd), "a confirm started at %s, before the last sweep run ended at %s", f.firstConfirm, f.lastSweepEnd)
	require.Equal(t, 2, f.maxPinned, "confirm runs are packed into the lanes as sweep runs are")
	for pair, runs := range f.runs {
		require.Equal(t, count*len(loads), runs, "sweep runs of %+v", pair)
	}
	want := map[confirmRun]int{}
	for _, cpu := range cpus {
		want[confirmRun{1, "TestA", cpu}] = confirm * len(loads)
		want[confirmRun{4, "TestA", cpu}] = confirm * len(loads)
	}
	require.Equal(t, want, f.confirms, "each test that failed or hung runs alone confirm times at every pair, and a failure outside any test isn't confirmed")
	require.Positive(t, phases.Confirm)
	require.Contains(t, log.String(), "confirming 2 tests, 7 times at each pair of CPU count and load\n")

	rep := res.Report()
	require.Equal(t, confirm, rep.Confirm)
	require.Equal(t, 2, rep.Confirmed)
	require.Equal(t, 2*len(cpus)*len(loads)*confirm, rep.ConfirmRuns)
	require.Len(t, rep.Failing, 2, "p1 failed every confirm run at cpu 4, and p5 every sweep run")
	p1 := rep.Failing[0]
	require.Equal(t, "p1", p1.Package)
	require.True(t, p1.Confirmed)
	require.Equal(t, []results.Pair{
		{CPU: 1, Load: 1, Runs: confirm}, {CPU: 1, Load: 2, Runs: confirm},
		{CPU: 2, Load: 1, Runs: confirm}, {CPU: 2, Load: 2, Runs: confirm},
		{CPU: 4, Load: 1, Runs: confirm, Failures: confirm}, {CPU: 4, Load: 2, Runs: confirm, Failures: confirm},
	}, p1.Pairs, "the table gives its confirm runs")
	require.Equal(t, "p5", rep.Failing[1].Package)
	require.False(t, rep.Failing[1].Confirmed)
	require.Len(t, rep.Flaky, 1)
	p4 := rep.Flaky[0]
	require.Equal(t, "p4", p4.Package)
	require.True(t, p4.Confirmed)
	require.Equal(t, len(cpus)*len(loads)*confirm, p4.Runs)
	require.Zero(t, p4.Hangs+p4.Failures, "it passed every confirm run")
	require.Equal(t, "flaky stress -run '^TestA$' -cpu 1 -load 1 -count 7 ./p4", p4.Reproduce, "with no confirm failure, the sweep's pair reproduces it")
}

func TestStress_confirms_nothing_when_its_confirm_count_is_0(t *testing.T) {
	f := newFakeRunner()
	f.fail = func(int, int) bool { return true }
	res := newResults(t, 2, []int{1}, []int{1, 2}, 3)
	var phases stress.Phases
	var err error
	synctest.Test(t, func(t *testing.T) {
		phases, err = stress.Stress(t.Context(), stress.Config{
			Packages: 2, CPUs: []int{1}, Loads: []int{1, 2}, Count: 3, Lanes: [][]int{{0}},
		}, f, res)
	})
	require.NoError(t, err)
	require.Empty(t, f.confirms)
	require.Zero(t, phases.Confirm)
	rep := res.Report()
	require.Zero(t, rep.Confirmed)
	require.Len(t, rep.Failing, 2)
	require.False(t, rep.Failing[0].Confirmed)
}

func TestStress_confirms_nothing_when_every_test_passed(t *testing.T) {
	f := newFakeRunner()
	var log bytes.Buffer
	var err error
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Packages: 2, CPUs: []int{1}, Loads: []int{1}, Count: 3, Confirm: 5, Lanes: [][]int{{0}}, Log: &log,
		}, f, newConfirmResults(t, 2, []int{1}, []int{1}, 3, 5))
	})
	require.NoError(t, err)
	require.Empty(t, f.confirms)
	require.NotContains(t, log.String(), "confirming")
}

func TestStress_stops_before_confirming_when_its_context_is_done_during_the_sweep(t *testing.T) {
	f := newFakeRunner()
	f.runTook = func(int, int) time.Duration { return time.Minute }
	f.fail = func(int, int) bool { return true }
	var err error
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
		defer cancel()
		_, err = stress.Stress(ctx, stress.Config{
			Packages: 1, CPUs: []int{1}, Loads: []int{1}, Count: 10, Confirm: 5, Lanes: [][]int{{0}},
		}, f, newConfirmResults(t, 1, []int{1}, []int{1}, 10, 5))
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, f.confirms)
}

func TestStress_pins_one_run_to_each_core_at_load_1(t *testing.T) {
	lanesOf, err := lanes.Split(cores(12), 4, 4)
	require.NoError(t, err)
	f := newFakeRunner()
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Packages: 3, CPUs: []int{1, 2, 4}, Loads: []int{1}, Count: 4, Lanes: lanesOf, Builds: 3,
		}, f, newResults(t, 3, []int{1, 2, 4}, []int{1}, 4))
	})
	require.NoError(t, err)
	require.Equal(t, 1, f.maxPinned)
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
			Packages: 3, CPUs: []int{1}, Loads: []int{1}, Count: 1, Lanes: [][]int{{0}},
		}, f, newResults(t, 3, []int{1}, []int{1}, 1))
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
			Packages: 1, CPUs: []int{1}, Loads: []int{1}, Count: 2, Lanes: [][]int{{0}}, Log: &log,
		}, f, newResults(t, 1, []int{1}, []int{1}, 2))
	})
	require.NoError(t, err)
	require.Contains(t, log.String(), "warning: TestMain failed")
	require.Contains(t, log.String(), "built 1 test binaries")
	require.Equal(t, 2, f.runs[pkgCPU{0, 1}])
}

func TestStress_writes_its_progress(t *testing.T) {
	f := newFakeRunner()
	f.runTook = func(int, int) time.Duration { return time.Minute }
	f.fail = func(int, int) bool { return true }
	var log bytes.Buffer
	synctest.Test(t, func(t *testing.T) {
		// The runs end 60s, 120s and 180s into the sweep, and the progress
		// is written at 50s, 100s and 150s, so no write ties with a run's
		// end, whose order in the bubble would be up to the scheduler.
		_, err := stress.Stress(t.Context(), stress.Config{
			Packages: 1, CPUs: []int{1}, Loads: []int{1}, Count: 3, Lanes: [][]int{{0}}, Log: &log, Progress: 50 * time.Second,
		}, f, newResults(t, 1, []int{1}, []int{1}, 3))
		require.NoError(t, err)
	})
	require.Contains(t, log.String(), "swept 0 of 3 runs, 0 failed or hung\n")
	require.Contains(t, log.String(), "swept 1 of 3 runs, 1 failed or hung\n")
	require.Contains(t, log.String(), "swept 2 of 3 runs, 2 failed or hung\n")
}

func TestStress_writes_its_confirm_runs_progress(t *testing.T) {
	f := newFakeRunner()
	f.runTook = func(int, int) time.Duration { return time.Minute }
	f.fail = func(int, int) bool { return true }
	f.confirmFail = func(int, int) bool { return true }
	var log bytes.Buffer
	synctest.Test(t, func(t *testing.T) {
		// The one sweep run ends at 60s, and the confirm runs at 120s and
		// 180s. Each pass writes its progress every 50s from its start:
		// the sweep at 50s, and the confirm runs at 110s and 170s.
		_, err := stress.Stress(t.Context(), stress.Config{
			Packages: 1, CPUs: []int{1}, Loads: []int{1}, Count: 1, Confirm: 2, Lanes: [][]int{{0}}, Log: &log, Progress: 50 * time.Second,
		}, f, newConfirmResults(t, 1, []int{1}, []int{1}, 1, 2))
		require.NoError(t, err)
	})
	require.Contains(t, log.String(), "swept 0 of 1 runs, 0 failed or hung\n")
	require.Contains(t, log.String(), "confirmed 0 of 2 runs, 0 failed or hung\n")
	require.Contains(t, log.String(), "confirmed 1 of 2 runs, 1 failed or hung\n")
}

func TestStress_stops_when_its_context_is_done_and_counts_no_cancelled_run(t *testing.T) {
	f := newFakeRunner()
	f.runTook = func(int, int) time.Duration { return time.Minute }
	f.fail = func(int, int) bool { return true }
	res := newResults(t, 1, []int{1}, []int{1}, 10)
	var err error
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
		defer cancel()
		_, err = stress.Stress(ctx, stress.Config{
			Packages: 1, CPUs: []int{1}, Loads: []int{1}, Count: 10, Lanes: [][]int{{0}},
		}, f, res)
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 3, f.runs[pkgCPU{0, 1}])
	rep := res.Report()
	require.Len(t, rep.Failing, 1)
	require.Equal(t, []results.Pair{{CPU: 1, Load: 1, Runs: 2, Failures: 2}}, rep.Failing[0].Pairs, "the run cut short isn't counted")
}

func TestStress_stops_when_a_run_s_output_cannot_be_saved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	res := results.New(results.Config{
		Packages: []discover.Package{{Module: ".", Dir: "p", ImportPath: "example.com/p"}},
		CPUs:     []int{1}, Loads: []int{1}, Count: 5, Out: file,
	})
	f := newFakeRunner()
	f.fail = func(int, int) bool { return true }
	var err error
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Packages: 1, CPUs: []int{1}, Loads: []int{1}, Count: 5, Lanes: [][]int{{0}},
		}, f, res)
	})
	require.ErrorContains(t, err, "saving a run's output")
	require.Equal(t, 1, f.runs[pkgCPU{0, 1}])
}

func TestBuildError_unwraps(t *testing.T) {
	cause := errors.New("cause")
	err := &stress.BuildError{Err: cause}
	require.ErrorIs(t, err, cause)
	require.Equal(t, "cause", err.Error())
}
