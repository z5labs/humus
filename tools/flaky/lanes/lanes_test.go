// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package lanes

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeRunner sleeps for each run's duration, and records what ran where and
// when. Inside a synctest bubble the sleeps take no real time. It records as a
// violation a run that leaves a core with more runs pinned to it than the
// run's load, or that starts in a lane beside a run at another load.
type fakeRunner struct {
	took   func(Run) time.Duration
	laneOf map[int]int

	mu         sync.Mutex
	pinned     map[int]int
	loads      map[int]map[int]int
	maxPinned  map[int]int
	violations []string
	started    []started
	runs       map[Pair]int
}

type started struct {
	run        Run
	start, end time.Time
}

func newFakeRunner(lanes [][]int, took func(Run) time.Duration) *fakeRunner {
	f := &fakeRunner{
		took:      took,
		laneOf:    map[int]int{},
		pinned:    map[int]int{},
		loads:     map[int]map[int]int{},
		maxPinned: map[int]int{},
		runs:      map[Pair]int{},
	}
	for i, l := range lanes {
		for _, c := range l {
			f.laneOf[c] = i
		}
		f.loads[i] = map[int]int{}
	}
	return f
}

func (f *fakeRunner) Run(ctx context.Context, r Run) {
	f.mu.Lock()
	start := time.Now()
	lane := f.laneOf[r.Cores[0]]
	for load, n := range f.loads[lane] {
		if n > 0 && load != r.Pair.Load {
			f.violations = append(f.violations, fmt.Sprintf("a run at load %d started in lane %d beside %d at load %d at %s", r.Pair.Load, lane, n, load, start))
		}
	}
	f.loads[lane][r.Pair.Load]++
	for _, c := range r.Cores {
		f.pinned[c]++
		f.maxPinned[c] = max(f.maxPinned[c], f.pinned[c])
		if f.pinned[c] > r.Pair.Load {
			f.violations = append(f.violations, fmt.Sprintf("core %d has %d runs pinned at %s, at load %d", c, f.pinned[c], start, r.Pair.Load))
		}
	}
	f.runs[r.Pair]++
	i := len(f.started)
	f.started = append(f.started, started{run: r, start: start})
	f.mu.Unlock()

	select {
	case <-time.After(f.took(r)):
	case <-ctx.Done():
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range r.Cores {
		f.pinned[c]--
	}
	f.loads[lane][r.Pair.Load]--
	f.started[i].end = time.Now()
}

func cores(from, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = from + i
	}
	return out
}

func TestSplit_keeps_back_the_first_cores_and_splits_the_rest_into_lanes(t *testing.T) {
	lanes, err := Split(cores(0, 32), 8, 4)
	require.NoError(t, err)
	require.Len(t, lanes, 6)
	require.Equal(t, []int{8, 9, 10, 11}, lanes[0])
	require.Equal(t, []int{28, 29, 30, 31}, lanes[5])

	lanes, err = Split(cores(0, 11), 2, 4)
	require.NoError(t, err)
	require.Equal(t, [][]int{{2, 3, 4, 5}, {6, 7, 8, 9}}, lanes, "the core left over is in no lane")

	lanes, err = Split([]int{3, 5, 7, 9}, 0, 2)
	require.NoError(t, err)
	require.Equal(t, [][]int{{3, 5}, {7, 9}}, lanes, "the cores are the ones given, not 0..n")
}

func TestSplit_refuses_cores_that_leave_no_lane(t *testing.T) {
	_, err := Split(cores(0, 8), 8, 4)
	require.ErrorContains(t, err, "8 cores, with 8 kept back, leave no lane of 4 cores")
	_, err = Split(cores(0, 8), -1, 4)
	require.Error(t, err)
	_, err = Split(cores(0, 8), 0, 0)
	require.Error(t, err)
}

func TestSchedule_never_pins_more_runs_to_a_core_than_its_lane_s_load_and_runs_each_pair_count_times(t *testing.T) {
	for seed := range uint64(40) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, seed))
			width := []int{1, 2, 4, 8}[rng.IntN(4)]
			lanes, err := Split(cores(0, 4+rng.IntN(40)+width), 4, width)
			require.NoError(t, err)
			var pairs []Pair
			for pkg := range 1 + rng.IntN(6) {
				for cpu := 1; cpu <= width; cpu *= 2 {
					for load := 1; load <= 3; load++ {
						if (cpu == width && load == 1) || rng.IntN(2) == 0 {
							pairs = append(pairs, Pair{Package: pkg, CPU: cpu, Load: load})
						}
					}
				}
			}
			count := 1 + rng.IntN(20)
			took := map[Pair]time.Duration{}
			for _, p := range pairs {
				took[p] = time.Duration(1+rng.IntN(1000)) * time.Millisecond
			}
			var f *fakeRunner
			var schedErr error
			synctest.Test(t, func(t *testing.T) {
				f = newFakeRunner(lanes, func(r Run) time.Duration { return took[r.Pair] })
				schedErr = Schedule(t.Context(), lanes, pairs, count, f)
			})
			require.NoError(t, schedErr)
			require.Empty(t, f.violations)
			require.Len(t, f.runs, len(pairs), "every pair started")
			for _, p := range pairs {
				require.Equal(t, count, f.runs[p], "runs of %+v", p)
			}
			for _, s := range f.started {
				require.Len(t, s.run.Cores, s.run.Pair.CPU)
				require.True(t, slices.ContainsFunc(lanes, func(l []int) bool {
					return !slices.ContainsFunc(s.run.Cores, func(c int) bool { return !slices.Contains(l, c) })
				}), "run %+v is pinned within one lane", s.run)
			}
		})
	}
}

func TestSchedule_holds_a_task_that_does_not_fit_and_starts_it_as_soon_as_its_lane_has_room(t *testing.T) {
	lanes := [][]int{{0, 1, 2, 3}}
	pairs := []Pair{{Package: 0, CPU: 1, Load: 1}, {Package: 0, CPU: 4, Load: 1}}
	var f *fakeRunner
	synctest.Test(t, func(t *testing.T) {
		f = newFakeRunner(lanes, func(r Run) time.Duration {
			if r.Pair.CPU == 1 {
				return time.Second
			}
			return 3 * time.Second
		})
		require.NoError(t, Schedule(t.Context(), lanes, pairs, 2, f))
	})
	require.Len(t, f.started, 4)
	// The lane takes the run at cpu 1, then the run at cpu 4, which
	// doesn't fit beside it and is held. It takes nothing else until the
	// run at cpu 1 has ended, and then starts the held run at once.
	first, held := f.started[0], f.started[1]
	require.Equal(t, 1, first.run.Pair.CPU)
	require.Equal(t, 4, held.run.Pair.CPU)
	require.Equal(t, first.end, held.start, "the held run starts as the lane's run ends")
	for _, s := range f.started[2:] {
		require.False(t, s.start.Before(held.start), "a run started while the lane held a task: %+v", s.run)
	}
}

func TestSchedule_keeps_other_lanes_taking_tasks_while_one_holds(t *testing.T) {
	lanes := [][]int{{0, 1}, {2, 3}}
	pairs := []Pair{{Package: 0, CPU: 1, Load: 1}, {Package: 1, CPU: 2, Load: 1}, {Package: 2, CPU: 1, Load: 1}}
	var f *fakeRunner
	synctest.Test(t, func(t *testing.T) {
		f = newFakeRunner(lanes, func(r Run) time.Duration { return time.Second })
		require.NoError(t, Schedule(t.Context(), lanes, pairs, 1, f))
	})
	// Lane 0 takes package 0 at cpu 1, then holds package 1 at cpu 2.
	// Lane 1 takes package 2 meanwhile.
	byPkg := map[int]started{}
	for _, s := range f.started {
		byPkg[s.run.Pair.Package] = s
	}
	require.Equal(t, byPkg[0].start, byPkg[2].start, "lane 1 took the task after the held one at once")
	require.Equal(t, []int{2}, byPkg[2].run.Cores)
	require.Equal(t, byPkg[0].end, byPkg[1].start)
	require.Equal(t, []int{0, 1}, byPkg[1].run.Cores)
}

func TestSchedule_runs_first_the_pair_furthest_behind_and_breaks_ties_by_the_longest_runs(t *testing.T) {
	lanes := [][]int{{0}}
	pairs := []Pair{{Package: 0, CPU: 1, Load: 1}, {Package: 1, CPU: 1, Load: 1}}
	var f *fakeRunner
	synctest.Test(t, func(t *testing.T) {
		f = newFakeRunner(lanes, func(r Run) time.Duration {
			return time.Duration(r.Pair.Package+1) * time.Second
		})
		require.NoError(t, Schedule(t.Context(), lanes, pairs, 3, f))
	})
	var order []int
	for _, s := range f.started {
		order = append(order, s.run.Pair.Package)
	}
	// Before any run has finished the tie goes in order. After, package 1,
	// whose runs take longer, goes first in every tie.
	require.Equal(t, []int{0, 1, 1, 0, 1, 0}, order)
}

func TestSchedule_pins_up_to_load_runs_to_each_core_of_a_lane(t *testing.T) {
	lanes := [][]int{{0, 1, 2, 3}}
	testCases := []struct {
		name   string
		pair   Pair
		atOnce int
	}{
		{"four runs at cpu 1 at load 1", Pair{CPU: 1, Load: 1}, 4},
		{"eight runs at cpu 1 at load 2", Pair{CPU: 1, Load: 2}, 8},
		{"four runs at cpu 2 at load 2", Pair{CPU: 2, Load: 2}, 4},
		{"two runs at cpu 4 at load 2", Pair{CPU: 4, Load: 2}, 2},
		{"three runs at cpu 4 at load 3", Pair{CPU: 4, Load: 3}, 3},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var f *fakeRunner
			var err error
			synctest.Test(t, func(t *testing.T) {
				f = newFakeRunner(lanes, func(r Run) time.Duration { return time.Second })
				err = Schedule(t.Context(), lanes, []Pair{tc.pair}, tc.atOnce, f)
			})
			require.NoError(t, err)
			require.Empty(t, f.violations)
			require.Len(t, f.started, tc.atOnce)
			for _, s := range f.started {
				require.Equal(t, f.started[0].start, s.start, "the lane holds %d runs at once", tc.atOnce)
			}
			for _, c := range lanes[0] {
				require.Equal(t, tc.pair.Load, f.maxPinned[c], "core %d had its load of runs pinned to it", c)
			}
		})
	}
}

func TestSchedule_holds_a_task_at_another_load_until_its_lane_has_drained(t *testing.T) {
	lanes := [][]int{{0, 1}}
	pairs := []Pair{{Package: 0, CPU: 1, Load: 1}, {Package: 1, CPU: 1, Load: 2}}
	var f *fakeRunner
	synctest.Test(t, func(t *testing.T) {
		f = newFakeRunner(lanes, func(r Run) time.Duration {
			return time.Duration(r.Pair.Load) * time.Second
		})
		require.NoError(t, Schedule(t.Context(), lanes, pairs, 1, f))
	})
	require.Empty(t, f.violations)
	require.Len(t, f.started, 2)
	// The lane takes the run at load 1 on core 0, then the run at load 2,
	// which would fit core 1 but is at another load, so it's held until the
	// run at load 1 has ended.
	first, held := f.started[0], f.started[1]
	require.Equal(t, 1, first.run.Pair.Load)
	require.Equal(t, 2, held.run.Pair.Load)
	require.Equal(t, first.end, held.start, "the held run starts as the lane drains")
}

func TestSchedule_runs_first_the_package_CPU_count_and_load_furthest_behind(t *testing.T) {
	lanes := [][]int{{0}}
	pairs := []Pair{{Package: 0, CPU: 1, Load: 1}, {Package: 0, CPU: 1, Load: 2}}
	var f *fakeRunner
	synctest.Test(t, func(t *testing.T) {
		f = newFakeRunner(lanes, func(r Run) time.Duration {
			return time.Duration(3-r.Pair.Load) * time.Second
		})
		require.NoError(t, Schedule(t.Context(), lanes, pairs, 3, f))
	})
	require.Empty(t, f.violations)
	var loads []int
	for _, s := range f.started {
		loads = append(loads, s.run.Pair.Load)
	}
	// The pairs differ only in their load, and each is run as soon as it
	// is furthest behind, though that holds the lane until it drains.
	require.Equal(t, []int{1, 2, 1, 2, 1, 2}, loads)
}

func TestSchedule_stops_starting_runs_when_its_context_is_done(t *testing.T) {
	lanes := [][]int{{0}}
	pairs := []Pair{{Package: 0, CPU: 1, Load: 1}}
	var f *fakeRunner
	var err error
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2500*time.Millisecond)
		defer cancel()
		f = newFakeRunner(lanes, func(r Run) time.Duration { return time.Second })
		err = Schedule(ctx, lanes, pairs, 100, f)
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, f.started, 3, "the run going at the deadline is waited for, and no more start")
}

func TestSchedule_refuses_a_run_wider_than_a_lane_a_load_below_1_and_pairs_with_no_lanes(t *testing.T) {
	err := Schedule(t.Context(), [][]int{{0, 1}}, []Pair{{Package: 0, CPU: 4, Load: 1}}, 1, newFakeRunner(nil, nil))
	require.ErrorContains(t, err, "a run at CPU count 4 can't fit a lane of 2 cores")
	err = Schedule(t.Context(), [][]int{{0, 1}}, []Pair{{Package: 0, CPU: 1, Load: 0}}, 1, newFakeRunner(nil, nil))
	require.ErrorContains(t, err, "a run can't be at load 0")
	err = Schedule(t.Context(), nil, []Pair{{Package: 0, CPU: 1, Load: 1}}, 1, newFakeRunner(nil, nil))
	require.ErrorContains(t, err, "no lanes")
	require.NoError(t, Schedule(t.Context(), [][]int{{0}}, nil, 1, newFakeRunner(nil, nil)))
}
