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
	"slices"
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
	// noTests are the packages whose binaries list no tests.
	noTests map[int]bool
	// walkOf is the walkthrough on whose cluster each package runs, for
	// the packages that run on one. upFails reports that the nth Up of a
	// walkthrough's cluster, from 1, fails.
	walkOf  map[int]int
	upFails func(w, n int) bool

	mu           sync.Mutex
	firstList    time.Time
	runningPkg   map[int]int
	maxPkg       map[int]int
	lastBuilt    time.Time
	builds       int
	firstRun     time.Time
	lastStart    time.Time
	runs         map[pkgCPU]int
	lastSweepEnd time.Time
	firstConfirm time.Time
	confirms     map[confirmRun]int
	pinned       map[int]int
	maxPinned    int
	running      int
	maxRunning   int
	// images are how many times each walkthrough's image was built, and
	// lastImage when the last build ended.
	images    map[int]int
	lastImage time.Time
	// ups and downs are how many times each walkthrough's cluster came
	// up, or tried to, and was deleted. up holds the cores of each
	// cluster up now, upCores the cores of every Up in turn, and upAt
	// when each began. maxUp is the most clusters up at once.
	ups, downs map[int]int
	up         map[int][]int
	upCores    [][]int
	upAt       []time.Time
	maxUp      int
	// onCluster is how many runs are running on each walkthrough's
	// cluster, and violations what broke a cluster's rules.
	onCluster  map[int]int
	violations []string
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
		buildTook:   func(int) time.Duration { return time.Second },
		runTook:     func(int, int) time.Duration { return 100 * time.Millisecond },
		fail:        func(int, int) bool { return false },
		hang:        func(int, int) bool { return false },
		packageFail: func(int, int) bool { return false },
		confirmFail: func(int, int) bool { return false },
		runs:        map[pkgCPU]int{},
		confirms:    map[confirmRun]int{},
		runningPkg:  map[int]int{},
		maxPkg:      map[int]int{},
		pinned:      map[int]int{},
		upFails:     func(int, int) bool { return false },
		images:      map[int]int{},
		ups:         map[int]int{},
		downs:       map[int]int{},
		up:          map[int][]int{},
		onCluster:   map[int]int{},
	}
}

func (f *fakeRunner) Image(ctx context.Context, w int) error {
	time.Sleep(5 * time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[w]++
	if now := time.Now(); now.After(f.lastImage) {
		f.lastImage = now
	}
	return nil
}

func (f *fakeRunner) Up(ctx context.Context, w int, cores []int) error {
	f.mu.Lock()
	f.ups[w]++
	n := f.ups[w]
	if _, ok := f.up[w]; ok {
		f.violations = append(f.violations, fmt.Sprintf("walkthrough %d's cluster came up while it was up", w))
	}
	for _, c := range cores {
		if f.pinned[c] > 0 {
			f.violations = append(f.violations, fmt.Sprintf("walkthrough %d's cluster came up on core %d, which a run is pinned to", w, c))
		}
	}
	f.up[w] = slices.Clone(cores)
	f.upCores = append(f.upCores, slices.Clone(cores))
	f.upAt = append(f.upAt, time.Now())
	f.maxUp = max(f.maxUp, len(f.up))
	f.mu.Unlock()
	time.Sleep(time.Minute)
	if f.upFails(w, n) {
		return fmt.Errorf("up.sh of walkthrough %d: exit status 1", w)
	}
	return nil
}

func (f *fakeRunner) Down(ctx context.Context, w int) error {
	time.Sleep(10 * time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downs[w]++
	delete(f.up, w)
	return nil
}

// checkCluster records a violation when a run of pkg on cores breaks a
// cluster's rules: a run on a cluster runs alone on it, on its cores and at
// its CPU count, and no other run is pinned to a cluster's cores. It is called
// with f.mu held, and returns the walkthrough pkg runs on the cluster of, or
// -1.
func (f *fakeRunner) checkCluster(pkg, cpu int, cores []int) int {
	w, ok := f.walkOf[pkg]
	if !ok {
		for other, upCores := range f.up {
			for _, c := range cores {
				if slices.Contains(upCores, c) {
					f.violations = append(f.violations, fmt.Sprintf("package %d ran on core %d of walkthrough %d's cluster", pkg, c, other))
				}
			}
		}
		return -1
	}
	upCores, up := f.up[w]
	switch {
	case !up:
		f.violations = append(f.violations, fmt.Sprintf("package %d ran with walkthrough %d's cluster down", pkg, w))
	case !slices.Equal(upCores, cores):
		f.violations = append(f.violations, fmt.Sprintf("package %d ran on cores %v, not its cluster's %v", pkg, cores, upCores))
	case cpu != stress.ClusterCPU:
		f.violations = append(f.violations, fmt.Sprintf("package %d ran on its cluster at CPU count %d", pkg, cpu))
	case f.onCluster[w] > 0:
		f.violations = append(f.violations, fmt.Sprintf("package %d ran beside another run on walkthrough %d's cluster", pkg, w))
	}
	f.onCluster[w]++
	return w
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
	f.mu.Lock()
	if f.firstList.IsZero() {
		f.firstList = time.Now()
	}
	f.mu.Unlock()
	if f.noTests[pkg] {
		return nil, f.listErr[pkg]
	}
	return []string{"TestA"}, f.listErr[pkg]
}

func (f *fakeRunner) Run(ctx context.Context, pkg int, test string, cpu int, cores []int) run.Result {
	f.mu.Lock()
	if f.firstRun.IsZero() {
		f.firstRun = time.Now()
	}
	f.lastStart = time.Now()
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
	f.runningPkg[pkg]++
	f.maxPkg[pkg] = max(f.maxPkg[pkg], f.runningPkg[pkg])
	w := f.checkCluster(pkg, cpu, cores)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if w >= 0 {
			f.onCluster[w]--
		}
		for _, c := range cores {
			f.pinned[c]--
		}
		f.running--
		f.runningPkg[pkg]--
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
		pkgs[i] = discover.Package{Module: ".", Dir: fmt.Sprintf("p%d", i), ImportPath: fmt.Sprintf("example.com/p%d", i), Level: "unit"}
	}
	return results.New(results.Config{Packages: pkgs, CPUs: cpus, Loads: loads, Out: t.TempDir(), Levels: []results.Level{{Name: "unit", Count: count, Confirm: confirm}}})
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
			Levels: []stress.Level{{Name: "unit", Count: count}}, Packages: make([]int, n), CPUs: cpus, Loads: loads, Lanes: lanesOf, Builds: 3,
		}, f, res)
	})
	require.NoError(t, stressErr)
	require.Equal(t, n, f.builds)
	require.False(t, f.firstList.Before(f.lastBuilt), "a binary was listed at %s, before the last build finished at %s", f.firstList, f.lastBuilt)
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
			Levels: []stress.Level{{Name: "unit", Count: count, Confirm: confirm}}, Packages: make([]int, n), CPUs: cpus, Loads: loads, Lanes: lanesOf, Builds: 3, Log: &log,
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
	require.Contains(t, log.String(), "confirming 2 unit tests, 7 times at each pair of CPU count and load\n")

	rep := res.Report()
	require.Equal(t, confirm, rep.Levels[0].Confirm)
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
	require.Equal(t, "flaky stress -tags unit -run '^TestA$' -cpu 1 -load 1 -count 7 ./p4", p4.Reproduce, "with no confirm failure, the sweep's pair reproduces it")
}

func TestStress_runs_each_level_s_packages_its_count_and_confirm_times_no_more_at_once_than_its_limit(t *testing.T) {
	cpus, loads := []int{1, 2}, []int{1, 2}
	lanesOf, err := lanes.Split(cores(12), 4, 4)
	require.NoError(t, err)
	f := newFakeRunner()
	f.runTook = func(pkg, cpu int) time.Duration { return time.Duration(1+pkg) * time.Second }
	// p0 is at the unit level, and p1 and p2 at the testcontainers level,
	// which runs only tests of its own: p2's binary has none. p1 fails at
	// cpu 1, in its sweep runs and its confirm runs.
	f.noTests = map[int]bool{2: true}
	f.fail = func(pkg, cpu int) bool { return pkg == 1 && cpu == 1 }
	f.confirmFail = f.fail
	pkgs := []discover.Package{
		{Module: ".", Dir: "p0", ImportPath: "example.com/p0", Level: "unit"},
		{Module: ".", Dir: "p1", ImportPath: "example.com/p1", Level: "testcontainers"},
		{Module: ".", Dir: "p2", ImportPath: "example.com/p2", Level: "testcontainers"},
	}
	res := results.New(results.Config{Packages: pkgs, CPUs: cpus, Loads: loads, Out: t.TempDir(), Levels: []results.Level{
		{Name: "unit", Count: 6}, {Name: "testcontainers", Count: 2, Confirm: 3},
	}})
	var log bytes.Buffer
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Levels: []stress.Level{
				{Name: "unit", Count: 6},
				{Name: "testcontainers", Count: 2, Confirm: 3, Limit: 1, Own: true},
			},
			Packages: []int{0, 1, 1}, CPUs: cpus, Loads: loads, Lanes: lanesOf, Builds: 3, Log: &log,
		}, f, res)
	})
	require.NoError(t, err)
	require.Equal(t, 3, f.builds, "p2 is built and listed, though it doesn't run")
	require.Equal(t, map[pkgCPU]int{
		{0, 1}: 6 * len(loads), {0, 2}: 6 * len(loads),
		{1, 1}: 2 * len(loads), {1, 2}: 2 * len(loads),
	}, f.runs, "each level's packages run its count times, and p2 not at all")
	require.Equal(t, map[confirmRun]int{
		{1, "TestA", 1}: 3 * len(loads), {1, "TestA", 2}: 3 * len(loads),
	}, f.confirms, "a test confirms its level's confirm times, and the unit level confirms none")
	require.Equal(t, 1, f.maxPkg[1], "no more than 1 of the testcontainers level's runs at once")
	require.Greater(t, f.maxPkg[0], 1, "the unit level isn't capped")
	require.Contains(t, log.String(), "confirming 1 testcontainers tests, 3 times at each pair of CPU count and load\n")

	rep := res.Report()
	require.Len(t, rep.Failing, 1)
	require.Equal(t, "testcontainers", rep.Failing[0].Level)
	require.Equal(t, "flaky stress -tags testcontainers -run '^TestA$' -cpu 1 -load 1 -count 3 ./p1", rep.Failing[0].Reproduce)
}

func TestStress_confirms_nothing_when_its_confirm_count_is_0(t *testing.T) {
	f := newFakeRunner()
	f.fail = func(int, int) bool { return true }
	res := newResults(t, 2, []int{1}, []int{1, 2}, 3)
	var phases stress.Phases
	var err error
	synctest.Test(t, func(t *testing.T) {
		phases, err = stress.Stress(t.Context(), stress.Config{
			Levels: []stress.Level{{Name: "unit", Count: 3}}, Packages: make([]int, 2), CPUs: []int{1}, Loads: []int{1, 2}, Lanes: [][]int{{0}},
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
			Levels: []stress.Level{{Name: "unit", Count: 3, Confirm: 5}}, Packages: make([]int, 2), CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0}}, Log: &log,
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
			Levels: []stress.Level{{Name: "unit", Count: 10, Confirm: 5}}, Packages: make([]int, 1), CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0}},
		}, f, newConfirmResults(t, 1, []int{1}, []int{1}, 10, 5))
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, f.confirms)
}

func TestStress_reports_the_sweep_runs_that_ended_when_its_context_is_done_during_the_sweep(t *testing.T) {
	f := newFakeRunner()
	f.runTook = func(int, int) time.Duration { return time.Minute }
	f.fail = func(pkg, _ int) bool { return pkg == 0 }
	res := newConfirmResults(t, 2, []int{1}, []int{1}, 10, 5)
	var err error
	var deadline time.Time
	synctest.Test(t, func(t *testing.T) {
		// The builds end at 1s, and the runs, two at a time, every minute
		// from then, so two of each package's ten end.
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
		defer cancel()
		deadline, _ = ctx.Deadline()
		_, err = stress.Stress(ctx, stress.Config{
			Levels: []stress.Level{{Name: "unit", Count: 10, Confirm: 5}}, Packages: make([]int, 2), CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0}, {1}},
		}, f, res)
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, f.confirms, "no confirm run starts after the interrupt")
	require.True(t, f.lastStart.Before(deadline), "the last run started at %s, after the context was done at %s", f.lastStart, deadline)
	rep := res.Report()
	require.Equal(t, 20, rep.PlannedRuns, "the sweep's runs; its confirm runs were never planned")
	require.Equal(t, 4, rep.EndedRuns, "the two runs cut short didn't end")
	require.Zero(t, rep.Confirmed)
	require.Len(t, rep.Failing, 1)
	p0 := rep.Failing[0]
	require.Equal(t, "p0", p0.Package)
	require.False(t, p0.Confirmed, "it has no confirm runs")
	require.False(t, p0.ConfirmIncomplete, "its confirm runs were never planned")
	require.Equal(t, []results.Pair{{CPU: 1, Load: 1, Runs: 2, Failures: 2}}, p0.Pairs, "its sweep runs that ended")
	require.Equal(t, 1, rep.Passed, "p1's TestA passed the runs that ended")
}

func TestStress_keeps_the_confirm_runs_that_ended_when_its_context_is_done_during_the_confirm_runs(t *testing.T) {
	f := newFakeRunner()
	f.runTook = func(int, int) time.Duration { return time.Minute }
	f.fail = func(pkg, _ int) bool { return pkg < 2 }
	f.confirmFail = func(pkg, _ int) bool { return pkg == 0 }
	res := newConfirmResults(t, 3, []int{1}, []int{1}, 1, 4)
	var err error
	var deadline time.Time
	synctest.Test(t, func(t *testing.T) {
		// The builds end at 1s, and the sweep's three runs, one to a
		// lane, at 61s. Then p0's and p1's eight confirm runs go three at
		// a time: three end at 121s, and the context is done at 150s,
		// while three more run and two have yet to start.
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
		defer cancel()
		deadline, _ = ctx.Deadline()
		_, err = stress.Stress(ctx, stress.Config{
			Levels: []stress.Level{{Name: "unit", Count: 1, Confirm: 4}}, Packages: make([]int, 3), CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0}, {1}, {2}},
		}, f, res)
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, f.lastStart.Before(deadline), "the last run started at %s, after the context was done at %s", f.lastStart, deadline)
	started := 0
	for _, n := range f.confirms {
		started += n
	}
	require.Equal(t, 6, started, "no confirm run starts after the interrupt")
	rep := res.Report()
	require.Equal(t, 3+2*4, rep.PlannedRuns, "every sweep run, and every confirm run once the sweep had ended")
	require.Equal(t, 3+3, rep.EndedRuns, "the three confirm runs cut short didn't end")
	tests := append(rep.Failing, rep.Flaky...)
	require.Len(t, tests, 2)
	ended := 0
	for _, test := range tests {
		ended += test.ConfirmEnded
		require.True(t, test.ConfirmIncomplete, "%s's confirm runs didn't all end", test.Package)
		require.Equal(t, 4, test.ConfirmPlanned)
		if test.ConfirmEnded == 0 {
			require.False(t, test.Confirmed, "%s has no confirm run that ended, so its counts are its sweep runs'", test.Package)
			require.Equal(t, 1, test.Runs)
			continue
		}
		require.True(t, test.Confirmed)
		require.Equal(t, test.ConfirmEnded, test.Runs, "%s's counts are of its confirm runs that ended", test.Package)
	}
	require.Equal(t, 3, ended, "the confirm runs that ended are kept")
	require.Equal(t, 1, rep.Passed, "p2's TestA")
}

func TestStress_pins_one_run_to_each_core_at_load_1(t *testing.T) {
	lanesOf, err := lanes.Split(cores(12), 4, 4)
	require.NoError(t, err)
	f := newFakeRunner()
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Levels: []stress.Level{{Name: "unit", Count: 4}}, Packages: make([]int, 3), CPUs: []int{1, 2, 4}, Loads: []int{1}, Lanes: lanesOf, Builds: 3,
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
			Levels: []stress.Level{{Name: "unit", Count: 1}}, Packages: make([]int, 3), CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0}},
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
			Levels: []stress.Level{{Name: "unit", Count: 2}}, Packages: make([]int, 1), CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0}}, Log: &log,
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
			Levels: []stress.Level{{Name: "unit", Count: 3}}, Packages: make([]int, 1), CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0}}, Log: &log, Progress: 50 * time.Second,
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
			Levels: []stress.Level{{Name: "unit", Count: 1, Confirm: 2}}, Packages: make([]int, 1), CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0}}, Log: &log, Progress: 50 * time.Second,
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
			Levels: []stress.Level{{Name: "unit", Count: 10}}, Packages: make([]int, 1), CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0}},
		}, f, res)
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 3, f.runs[pkgCPU{0, 1}])
	rep := res.Report()
	require.Len(t, rep.Failing, 1)
	require.Equal(t, []results.Pair{{CPU: 1, Load: 1, Runs: 2, Failures: 2}}, rep.Failing[0].Pairs, "the run cut short isn't counted")
	require.Equal(t, 10, rep.PlannedRuns)
	require.Equal(t, 2, rep.EndedRuns, "the run cut short didn't end")
}

func TestStress_stops_when_a_run_s_output_cannot_be_saved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	res := results.New(results.Config{
		Packages: []discover.Package{{Module: ".", Dir: "p", ImportPath: "example.com/p", Level: "unit"}},
		CPUs:     []int{1}, Loads: []int{1}, Out: file,
		Levels: []results.Level{{Name: "unit", Count: 5}},
	})
	f := newFakeRunner()
	f.fail = func(int, int) bool { return true }
	var err error
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Levels: []stress.Level{{Name: "unit", Count: 5}}, Packages: make([]int, 1), CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0}},
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

func TestStress_runs_each_walkthrough_s_tests_on_a_cluster_of_its_own_one_run_at_a_time_on_its_own_lane(t *testing.T) {
	cpus, loads := []int{1, 2, 4}, []int{1, 2}
	lanesOf, err := lanes.Split(cores(16), 0, 4)
	require.NoError(t, err)
	f := newFakeRunner()
	// p0 and p1 are unit packages. p2 and p3 are the functional tests of
	// walkthrough 0, p4 of walkthrough 1 and p5 of walkthrough 2, whose
	// cluster never comes up. p2 and p3 fail their first sweep run and pass
	// every other run.
	f.walkOf = map[int]int{2: 0, 3: 0, 4: 1, 5: 2}
	f.upFails = func(w, _ int) bool { return w == 2 }
	var mu sync.Mutex
	failed := map[int]bool{}
	f.fail = func(pkg, cpu int) bool {
		mu.Lock()
		defer mu.Unlock()
		if (pkg == 2 || pkg == 3) && !failed[pkg] {
			failed[pkg] = true
			return true
		}
		return false
	}
	f.runTook = func(pkg, cpu int) time.Duration {
		if _, ok := f.walkOf[pkg]; ok {
			return 30 * time.Second
		}
		return time.Second
	}
	pkgs := []discover.Package{
		{Module: ".", Dir: "p0", ImportPath: "example.com/p0", Level: "unit"},
		{Module: ".", Dir: "p1", ImportPath: "example.com/p1", Level: "unit"},
		{Module: "w0", Dir: "w0/p2", ImportPath: "example.com/w0/p2", Level: "functional"},
		{Module: "w0", Dir: "w0/p3", ImportPath: "example.com/w0/p3", Level: "functional"},
		{Module: "w1", Dir: "w1/p4", ImportPath: "example.com/w1/p4", Level: "functional"},
		{Module: "w2", Dir: "w2/p5", ImportPath: "example.com/w2/p5", Level: "functional"},
	}
	res := results.New(results.Config{
		Packages: pkgs, CPUs: cpus, Loads: loads, Out: t.TempDir(),
		Levels: []results.Level{
			{Name: "unit", Count: 4},
			{Name: "functional", Count: 3, Confirm: 2, CPU: stress.ClusterCPU, Load: 1},
		},
		Walkthroughs: []discover.Walkthrough{{Module: "w0", Name: "w0"}, {Module: "w1", Name: "w1"}, {Module: "w2", Name: "w2"}},
	})
	var log bytes.Buffer
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Levels: []stress.Level{
				{Name: "unit", Count: 4},
				{Name: "functional", Count: 3, Confirm: 2, Limit: 2, Own: true},
			},
			Packages:     []int{0, 0, 1, 1, 1, 1},
			Walkthroughs: []int{-1, -1, 0, 0, 1, 2},
			CPUs:         cpus, Loads: loads, Lanes: lanesOf, Builds: 4, Log: &log,
		}, f, res)
	})
	require.NoError(t, err)
	require.Empty(t, f.violations, "a cluster serves one run at a time, on its own cores")
	require.Equal(t, map[int]int{0: 1, 1: 1, 2: 1}, f.images, "each walkthrough's image is built once")
	require.False(t, f.upAt[0].Before(f.lastImage), "no cluster comes up before every image is built")
	require.False(t, f.firstRun.Before(f.lastImage), "no run starts before every image is built")
	require.Equal(t, 2, f.maxUp, "no more clusters are up at once than the cap")
	require.ElementsMatch(t, [][]int{lanesOf[0], lanesOf[1]}, f.upCores[:2], "the clusters start before every other run, on the first lanes, and each holds a whole lane")

	require.Equal(t, map[int]int{0: 3, 1: 1, 2: 1}, f.ups, "walkthrough 0 gets a fresh cluster for each of its tests' confirm runs, and walkthrough 2's cluster isn't retried")
	require.Equal(t, f.ups, f.downs, "every cluster is deleted, the one that didn't come up too")
	require.Equal(t, map[pkgCPU]int{
		{0, 1}: 8, {0, 2}: 8, {0, 4}: 8, {1, 1}: 8, {1, 2}: 8, {1, 4}: 8,
		{2, 4}: 3, {3, 4}: 3, {4, 4}: 3,
	}, f.runs, "a cluster's packages run its level's count times, at its CPU count alone, and walkthrough 2's not at all")
	require.Equal(t, map[confirmRun]int{{2, "TestA", 4}: 2, {3, "TestA", 4}: 2}, f.confirms)
	require.Contains(t, log.String(), "confirming 2 functional tests, 2 times at each pair of CPU count and load\n")

	rep := res.Report()
	require.True(t, rep.Failed())
	require.Len(t, rep.UpFailures, 1)
	require.Equal(t, "w2", rep.UpFailures[0].Walkthrough, "the walkthrough whose cluster didn't come up is reported by name")
	require.Equal(t, []string{"up.sh of walkthrough 2: exit status 1"}, rep.UpFailures[0].Error)
	require.Len(t, rep.Flaky, 2)
	require.Equal(t, []results.Pair{{CPU: stress.ClusterCPU, Load: 1, Runs: 2}}, rep.Flaky[0].Pairs, "a functional test's table has its one pair")
	require.Equal(t, "flaky stress -tags functional -run '^TestA$' -count 2 ./w0/p2", rep.Flaky[0].Reproduce, "its command gives no -cpu or -load, which its runs ignore")
}

func TestStress_runs_nothing_when_a_walkthrough_s_image_does_not_build(t *testing.T) {
	f := &imageFails{fakeRunner: newFakeRunner()}
	var err error
	synctest.Test(t, func(t *testing.T) {
		_, err = stress.Stress(t.Context(), stress.Config{
			Levels: []stress.Level{{Name: "unit", Count: 1}, {Name: "functional", Count: 1, Own: true}}, Packages: []int{0, 1}, Walkthroughs: []int{-1, 0},
			CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0, 1, 2, 3}},
		}, f, newResults(t, 2, []int{1}, []int{1}, 1))
	})
	var buildErr *stress.BuildError
	require.ErrorAs(t, err, &buildErr)
	require.ErrorContains(t, err, "building the image of walkthrough 0")
	require.Empty(t, f.runs)
	require.Empty(t, f.ups)
}

// imageFails is a fakeRunner whose images don't build.
type imageFails struct{ *fakeRunner }

func (f *imageFails) Image(ctx context.Context, w int) error {
	return fmt.Errorf("building the image of walkthrough %d", w)
}

func TestStress_deletes_a_cluster_and_stops_when_its_context_is_done(t *testing.T) {
	f := newFakeRunner()
	f.walkOf = map[int]int{0: 0}
	f.runTook = func(int, int) time.Duration { return time.Minute }
	res := results.New(results.Config{
		Packages: []discover.Package{{Module: "w", Dir: "w/p", ImportPath: "example.com/w/p", Level: "functional"}},
		CPUs:     []int{1}, Loads: []int{1}, Out: t.TempDir(),
		Levels:       []results.Level{{Name: "functional", Count: 10, CPU: stress.ClusterCPU, Load: 1}},
		Walkthroughs: []discover.Walkthrough{{Module: "w", Name: "w"}},
	})
	var err error
	synctest.Test(t, func(t *testing.T) {
		// The image is built at 5s, the cluster is up at 65s, and its
		// runs end every minute from then.
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Second)
		defer cancel()
		_, err = stress.Stress(ctx, stress.Config{
			Levels: []stress.Level{{Name: "functional", Count: 10, Own: true}}, Packages: []int{0}, Walkthroughs: []int{0},
			CPUs: []int{1}, Loads: []int{1}, Lanes: [][]int{{0, 1, 2, 3}},
		}, f, res)
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, map[int]int{0: 1}, f.downs, "the cluster is deleted though the sweep was cut short")
	require.Equal(t, 3, f.runs[pkgCPU{0, stress.ClusterCPU}])
	require.Empty(t, res.Report().UpFailures)
}
