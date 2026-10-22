// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package lanes packs runs into lanes of cores.
//
// Some cores are kept back, and the rest are split into lanes as wide as the
// largest CPU count. A run at CPU count c and load L is pinned to c cores of
// one lane, each of which has fewer than L runs pinned to it. At load 1 a run
// has its cores to itself; at load 2 it shares them with another run.
//
// A lane runs at one load at a time. At load L, each of its cores has up to L
// runs pinned to it, and every run on it is at load L.
//
// When a lane has room, it takes the queue's next task. If the task doesn't
// fit, because it is wider than the cores the lane has room on or at another
// load, the lane holds it, and takes nothing else until enough of its runs
// have ended: for a task at another load, until all of them have. The other
// lanes keep taking the tasks after it. So a lane holds at most one task, and
// no task waits forever.
//
// The queue runs first the pair furthest behind its count, counting the runs
// it has started, so every pair has the same share of its runs started at any
// moment. Ties go to the pair whose runs have taken longest on average,
// measured as they finish, so the longest runs don't all come last.
package lanes

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Pair is one package at one pair of CPU count and load, or one test of it.
type Pair struct {
	// Package is the package's index.
	Package int
	// Test is the one test a run of the pair runs alone, or "" when it
	// runs the whole package.
	Test string
	// CPU is the run's CPU count.
	CPU int
	// Load is how many runs, at most, are pinned to each of the run's
	// cores.
	Load int
}

// Run is one run of a pair, pinned to its cores.
type Run struct {
	Pair  Pair
	Cores []int
}

// Runner runs runs.
type Runner interface {
	// Run runs r on its cores, and returns once it has ended. It must
	// return soon after ctx is done.
	Run(ctx context.Context, r Run)
}

// Split keeps back keep of cores, the first ones, and splits the rest into
// lanes of width cores. Cores left over are used by neither.
func Split(cores []int, keep, width int) ([][]int, error) {
	if width < 1 {
		return nil, fmt.Errorf("a lane must be at least 1 core wide, not %d", width)
	}
	if keep < 0 {
		return nil, fmt.Errorf("can't keep back %d cores", keep)
	}
	free := len(cores) - keep
	if free < width {
		return nil, fmt.Errorf("%d cores, with %d kept back, leave no lane of %d cores", len(cores), keep, width)
	}
	lanes := make([][]int, free/width)
	for i := range lanes {
		start := keep + i*width
		lanes[i] = cores[start : start+width : start+width]
	}
	return lanes, nil
}

// Schedule runs every pair count times on r, packed into lanes, and returns
// once every run has ended. Every pair's CPU count must be at most the width
// of every lane, and its load at least 1. When ctx is done, Schedule starts no more runs, waits for
// those running, and returns ctx's error.
func Schedule(ctx context.Context, lanes [][]int, pairs []Pair, count int, r Runner) error {
	s := &scheduler{
		ctx:    ctx,
		runner: r,
		queue:  newQueue(pairs, count),
		ended:  make(chan ended),
		lanes:  make([]*lane, len(lanes)),
	}
	if len(lanes) == 0 && len(pairs) > 0 && count > 0 {
		return fmt.Errorf("no lanes to run %d pairs on", len(pairs))
	}
	for i, cores := range lanes {
		for _, p := range pairs {
			if p.CPU > len(cores) || p.CPU < 1 {
				return fmt.Errorf("a run at CPU count %d can't fit a lane of %d cores", p.CPU, len(cores))
			}
			if p.Load < 1 {
				return fmt.Errorf("a run can't be at load %d", p.Load)
			}
		}
		s.lanes[i] = &lane{cores: cores, pinned: make([]int, len(cores))}
	}
	for {
		if ctx.Err() == nil {
			s.fill()
		}
		if s.running == 0 {
			return ctx.Err()
		}
		e := <-s.ended
		s.running--
		e.lane.release(e.slots)
		s.queue.finished(e.run.Pair, e.took)
	}
}

type scheduler struct {
	ctx     context.Context
	runner  Runner
	queue   *queue
	lanes   []*lane
	ended   chan ended
	running int
}

type ended struct {
	lane  *lane
	slots []int
	run   Run
	took  time.Duration
}

// fill starts every run that fits: first each lane's held task, then the
// queue's next tasks, until no lane can take another.
func (s *scheduler) fill() {
	for changed := true; changed; {
		changed = false
		for _, l := range s.lanes {
			if l.held != nil {
				if !l.fits(*l.held) {
					continue
				}
				s.start(l, *l.held)
				l.held = nil
				changed = true
			}
			for l.held == nil && l.room() {
				p, ok := s.queue.next()
				if !ok {
					break
				}
				changed = true
				if !l.fits(p) {
					l.held = &p
					break
				}
				s.start(l, p)
			}
		}
	}
}

func (s *scheduler) start(l *lane, p Pair) {
	slots := l.take(p)
	cores := make([]int, len(slots))
	for i, slot := range slots {
		cores[i] = l.cores[slot]
	}
	run := Run{Pair: p, Cores: cores}
	s.running++
	go func() {
		start := time.Now()
		s.runner.Run(s.ctx, run)
		s.ended <- ended{lane: l, slots: slots, run: run, took: time.Since(start)}
	}()
}

type lane struct {
	cores []int
	// pinned is how many runs are pinned to each core.
	pinned []int
	// runs is how many runs are on the lane, and load is the load they
	// are all at, or 0 when there are none.
	runs, load int
	held       *Pair
}

// room reports whether the lane can take another task: it has no runs, or
// one of its cores has fewer runs pinned to it than its load.
func (l *lane) room() bool {
	return l.runs == 0 || l.open(l.load) > 0
}

// open returns how many of the lane's cores have fewer than load runs pinned
// to them.
func (l *lane) open(load int) int {
	n := 0
	for _, pinned := range l.pinned {
		if pinned < load {
			n++
		}
	}
	return n
}

// fits reports whether a run of p can start on the lane now.
func (l *lane) fits(p Pair) bool {
	if l.runs > 0 && p.Load != l.load {
		return false
	}
	return l.open(p.Load) >= p.CPU
}

// take pins a run of p to the p.CPU cores with fewest runs pinned to them,
// the lowest first among equals, and returns their slots. It must fit.
func (l *lane) take(p Pair) []int {
	slots := make([]int, 0, len(l.cores))
	for i, pinned := range l.pinned {
		if pinned < p.Load {
			slots = append(slots, i)
		}
	}
	slices.SortStableFunc(slots, func(a, b int) int { return l.pinned[a] - l.pinned[b] })
	slots = slots[:p.CPU:p.CPU]
	slices.Sort(slots)
	for _, i := range slots {
		l.pinned[i]++
	}
	l.runs++
	l.load = p.Load
	return slots
}

func (l *lane) release(slots []int) {
	for _, i := range slots {
		l.pinned[i]--
	}
	l.runs--
	if l.runs == 0 {
		l.load = 0
	}
}

type queue struct {
	pairs []pairState
	count int
}

type pairState struct {
	pair     Pair
	issued   int
	finished int
	took     time.Duration
}

func newQueue(pairs []Pair, count int) *queue {
	q := &queue{count: count, pairs: make([]pairState, len(pairs))}
	for i, p := range pairs {
		q.pairs[i] = pairState{pair: p}
	}
	return q
}

// next takes one run of the pair furthest behind its count, ties going to the
// longest runs.
func (q *queue) next() (Pair, bool) {
	best := -1
	for i, p := range q.pairs {
		if p.issued >= q.count {
			continue
		}
		if best < 0 || q.before(p, q.pairs[best]) {
			best = i
		}
	}
	if best < 0 {
		return Pair{}, false
	}
	q.pairs[best].issued++
	return q.pairs[best].pair, true
}

func (q *queue) before(a, b pairState) bool {
	if a.issued != b.issued {
		return a.issued < b.issued
	}
	return a.mean() > b.mean()
}

func (p pairState) mean() time.Duration {
	if p.finished == 0 {
		return 0
	}
	return p.took / time.Duration(p.finished)
}

func (q *queue) finished(p Pair, took time.Duration) {
	for i := range q.pairs {
		if q.pairs[i].pair == p {
			q.pairs[i].finished++
			q.pairs[i].took += took
			return
		}
	}
}
