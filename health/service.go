// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package health

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// options is what the Options given to [NewService] build up.
type options struct {
	liveness  []Monitor
	readiness []Monitor
}

// Option configures the [Service] that [NewService] builds.
type Option func(*options)

// Liveness adds m to the Service's liveness probe. The probe checks its
// monitors in the order they were added. Liveness panics if m is nil.
func Liveness(m Monitor) Option {
	if m == nil {
		panic("health: Liveness called with a nil Monitor")
	}
	return func(o *options) {
		o.liveness = append(o.liveness, m)
	}
}

// Readiness adds m to the Service's readiness probe. The probe checks the
// Service's own state first, and then its monitors in the order they were
// added. Readiness panics if m is nil.
func Readiness(m Monitor) Option {
	if m == nil {
		panic("health: Readiness called with a nil Monitor")
	}
	return func(o *options) {
		o.readiness = append(o.readiness, m)
	}
}

// Service is a humus.Service that owns an App's readiness. It opens no
// listener and serves no protocol: whatever exposes its [Service.Probes]
// does that.
//
// Its own readiness is false until its Serve starts, true while Serve runs,
// and false for good from the moment Serve's context is done or Shutdown is
// called. The App cancels Serve's context as soon as it starts shutting
// down, before its drain, so a load balancer checking readiness stops
// sending the App traffic while it drains.
//
// An App that builds a Service but doesn't return it from its Init never
// turns ready.
type Service struct {
	liveness  []Monitor
	readiness []Monitor

	// serving is set once, by the first Serve, to that Serve's run, or by
	// a Shutdown called before any Serve, to notServed.
	serving atomic.Pointer[run]

	// stop is closed by the first Shutdown.
	stop     chan struct{}
	stopOnce sync.Once
}

// run is one call of Serve: the done channel of its context, and served,
// which is closed when it returns.
type run struct {
	done   <-chan struct{}
	served chan struct{}
}

// notServed is the run of a Service that was shut down before it was
// served.
var notServed = &run{}

// NewService returns a Service with the monitors opts add to its probes.
func NewService(opts ...Option) *Service {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return &Service{
		liveness:  o.liveness,
		readiness: o.readiness,
		stop:      make(chan struct{}),
	}
}

// Probes returns the Service's probes, its own readiness included:
//
//   - Readiness is [And] of the Service's own state and the monitors
//     [Readiness] added, in the order they were added.
//   - Liveness is [And] of the monitors [Liveness] added, in the order they
//     were added, and is healthy when there are none.
//
// The monitors are safe for concurrent use, as long as the monitors they
// check are. They report the Service's state when they are checked, so
// Probes may be called before Serve.
func (s *Service) Probes() Probes {
	readiness := make(AndMonitor, 0, len(s.readiness)+1)
	readiness = append(readiness, MonitorFunc(s.ready))
	readiness = append(readiness, s.readiness...)
	return Probes{
		Liveness:  And(slices.Clone(s.liveness)...),
		Readiness: readiness,
	}
}

// ready reports the Service's own state: whether Serve is running, and
// neither its context is done nor Shutdown has been called.
func (s *Service) ready(context.Context) (bool, error) {
	r := s.serving.Load()
	if r == nil || r == notServed {
		return false, nil
	}
	select {
	case <-r.done:
		return false, nil
	case <-r.served:
		return false, nil
	case <-s.stop:
		return false, nil
	default:
		return true, nil
	}
}

// Serve marks the Service ready, and returns nil once ctx is done or
// Shutdown is called, marking it not ready for good.
//
// A Service is served once. A Serve called after Shutdown, or after an
// earlier Serve, returns nil at once and leaves the Service not ready.
func (s *Service) Serve(ctx context.Context) error {
	r := &run{done: ctx.Done(), served: make(chan struct{})}
	if !s.serving.CompareAndSwap(nil, r) {
		return nil
	}
	defer close(r.served)

	select {
	case <-ctx.Done():
	case <-s.stop:
	}
	return nil
}

// Shutdown marks the Service not ready for good and stops its Serve. It
// returns nil once Serve has returned, or at once when Serve was never
// called or had already returned. If ctx is done before Serve has returned,
// Shutdown returns an error that wraps ctx's error.
func (s *Service) Shutdown(ctx context.Context) error {
	s.serving.CompareAndSwap(nil, notServed)
	r := s.serving.Load()

	returned := r == notServed
	if !returned {
		select {
		case <-r.served:
			returned = true
		default:
		}
	}
	s.stopOnce.Do(func() { close(s.stop) })
	if returned {
		return nil
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("health: shutting down the Service: %w", err)
	}
	select {
	case <-r.served:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("health: shutting down the Service: %w", ctx.Err())
	}
}
