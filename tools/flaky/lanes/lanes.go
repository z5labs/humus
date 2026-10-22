// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package lanes packs runs into lanes of cores.
//
// Some cores are kept back, and the rest are split into lanes as wide as the
// largest CPU count. A run at CPU count c is pinned to c free cores of one
// lane, so no core ever has two runs pinned to it.
//
// When a lane has room, it takes the queue's next task. If the task doesn't
// fit, the lane holds it, and takes nothing else until enough of its runs have
// ended. The other lanes keep taking the tasks after it. So a lane holds at
// most one task, and no task waits forever.
//
// The queue runs first the pair furthest behind its count, so every pair has
// the same share of its runs done at any moment. Ties go to the pair whose
// runs have taken longest on average, measured as they finish, so the longest
// runs don't all come last.
package lanes

import (
	"context"
	"fmt"
	"time"
)

// Pair is one package at one CPU count.
type Pair struct {
	// Package is the package's index.
	Package int
	// CPU is the run's CPU count.
	CPU int
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
// of every lane. When ctx is done, Schedule starts no more runs, waits for
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
		}
		s.lanes[i] = &lane{cores: cores, busy: make([]bool, len(cores))}
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
				if l.free() < l.held.CPU {
					continue
				}
				s.start(l, *l.held)
				l.held = nil
				changed = true
			}
			for l.held == nil && l.free() > 0 {
				p, ok := s.queue.next()
				if !ok {
					break
				}
				changed = true
				if p.CPU > l.free() {
					l.held = &p
					break
				}
				s.start(l, p)
			}
		}
	}
}

func (s *scheduler) start(l *lane, p Pair) {
	slots := l.take(p.CPU)
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
	busy  []bool
	held  *Pair
}

func (l *lane) free() int {
	n := 0
	for _, b := range l.busy {
		if !b {
			n++
		}
	}
	return n
}

// take marks the n lowest free cores busy and returns their slots.
func (l *lane) take(n int) []int {
	slots := make([]int, 0, n)
	for i, b := range l.busy {
		if len(slots) == n {
			break
		}
		if !b {
			l.busy[i] = true
			slots = append(slots, i)
		}
	}
	return slots
}

func (l *lane) release(slots []int) {
	for _, i := range slots {
		l.busy[i] = false
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
