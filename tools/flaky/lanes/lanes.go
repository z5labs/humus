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
// moment, whatever its count. Ties go to the pair whose runs have taken longest
// on average, measured as they finish, so the longest runs don't all come
// last. A task marked first goes before every task that isn't, whatever its
// share: a cluster holds its lane for longest, so it starts first.
//
// A level's runs can be capped: once as many of them are running as its cap,
// the queue passes over its pairs, and a lane holding one of them waits, until
// one ends.
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
	// Level is the index of the pair's level, whose runs Schedule's limits
	// may cap.
	Level int
}

// Task is a pair, to run Count times.
type Task struct {
	Pair  Pair
	Count int
	// First reports that the queue runs the task before every task that
	// isn't first.
	First bool
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

// Kept returns the keep cores that Split keeps back from cores: the first
// ones.
func Kept(cores []int, keep int) []int {
	n := min(max(keep, 0), len(cores))
	return cores[:n:n]
}

// Schedule runs each task's pair its count times on r, packed into lanes, and
// returns once every run has ended. Every pair's CPU count must be at most the
// width of every lane, and its load at least 1. limits caps how many runs of
// each level run at once; a level it doesn't name, or caps at 0, isn't capped.
// When ctx is done, Schedule starts no more runs, waits for those running, and
// returns ctx's error.
func Schedule(ctx context.Context, lanes [][]int, tasks []Task, limits map[int]int, r Runner) error {
	s := &scheduler{
		ctx:     ctx,
		runner:  r,
		queue:   newQueue(tasks),
		ended:   make(chan ended),
		lanes:   make([]*lane, len(lanes)),
		limits:  limits,
		running: map[int]int{},
	}
	if len(lanes) == 0 && s.queue.remaining() {
		return fmt.Errorf("no lanes to run %d pairs on", len(tasks))
	}
	for i, cores := range lanes {
		for _, t := range tasks {
			p := t.Pair
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
		if s.runs == 0 {
			return ctx.Err()
		}
		e := <-s.ended
		s.runs--
		s.running[e.run.Pair.Level]--
		e.lane.release(e.slots)
		s.queue.finished(e.run.Pair, e.took)
	}
}

type scheduler struct {
	ctx    context.Context
	runner Runner
	queue  *queue
	lanes  []*lane
	ended  chan ended
	limits map[int]int
	// runs is how many runs are running, and running how many of them
	// are at each level.
	runs    int
	running map[int]int
}

// capped reports whether as many runs of p's level are running as its limit.
func (s *scheduler) capped(p Pair) bool {
	limit := s.limits[p.Level]
	return limit > 0 && s.running[p.Level] >= limit
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
				if !l.fits(*l.held) || s.capped(*l.held) {
					continue
				}
				s.start(l, *l.held)
				l.held = nil
				changed = true
			}
			for l.held == nil && l.room() {
				p, ok := s.queue.next(s.capped)
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
	s.runs++
	s.running[p.Level]++
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
}

type pairState struct {
	pair     Pair
	first    bool
	count    int
	issued   int
	finished int
	took     time.Duration
}

func newQueue(tasks []Task) *queue {
	q := &queue{pairs: make([]pairState, len(tasks))}
	for i, t := range tasks {
		q.pairs[i] = pairState{pair: t.Pair, first: t.First, count: t.Count}
	}
	return q
}

// remaining reports whether any pair has runs left to start.
func (q *queue) remaining() bool {
	for _, p := range q.pairs {
		if p.issued < p.count {
			return true
		}
	}
	return false
}

// next takes one run of the pair furthest behind its count, a first pair
// before any other and ties going to the longest runs, passing over the pairs
// that skip reports.
func (q *queue) next(skip func(Pair) bool) (Pair, bool) {
	best := -1
	for i, p := range q.pairs {
		if p.issued >= p.count || skip(p.pair) {
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

// before reports whether a is first and b isn't, or, when both or neither
// are, whether a is further behind its count than b, by the share of its runs
// started, or as far behind and its runs take longer.
func (q *queue) before(a, b pairState) bool {
	if a.first != b.first {
		return a.first
	}
	if x, y := a.issued*b.count, b.issued*a.count; x != y {
		return x < y
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
