// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package health_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/z5labs/humus/health"
)

func ExampleBinary() {
	var b health.Binary

	healthy, _ := b.Healthy(context.Background())
	fmt.Println(healthy)

	b.MarkHealthy()

	healthy, _ = b.Healthy(context.Background())
	fmt.Println(healthy)

	b.MarkUnhealthy()

	healthy, _ = b.Healthy(context.Background())
	fmt.Println(healthy)

	// Output: false
	// true
	// false
}

func ExampleAnd() {
	var a health.Binary
	var b health.Binary

	and := health.And(&a, &b)

	healthy, _ := and.Healthy(context.Background())
	fmt.Println(healthy)

	a.MarkHealthy()

	healthy, _ = and.Healthy(context.Background())
	fmt.Println(healthy)

	b.MarkHealthy()

	healthy, _ = and.Healthy(context.Background())
	fmt.Println(healthy)

	// Output: false
	// false
	// true
}

func ExampleOr() {
	var a health.Binary
	var b health.Binary

	or := health.Or(&a, &b)

	healthy, _ := or.Healthy(context.Background())
	fmt.Println(healthy)

	a.MarkHealthy()

	healthy, _ = or.Healthy(context.Background())
	fmt.Println(healthy)

	b.MarkHealthy()

	healthy, _ = or.Healthy(context.Background())
	fmt.Println(healthy)

	// Output: false
	// true
	// true
}

// A MonitorFunc turns a check of a dependency, here a stand-in for a
// database ping, into a Monitor that combines with others.
func ExampleMonitorFunc() {
	var reachable bool
	ping := func(ctx context.Context) error {
		if !reachable {
			return errors.New("connection refused")
		}
		return nil
	}

	var started health.Binary
	probes := health.Probes{
		Liveness: health.And(),
		Readiness: health.And(&started, health.MonitorFunc(func(ctx context.Context) (bool, error) {
			return ping(ctx) == nil, nil
		})),
	}

	live, _ := probes.Liveness.Healthy(context.Background())
	ready, _ := probes.Readiness.Healthy(context.Background())
	fmt.Println(live, ready)

	started.MarkHealthy()
	ready, _ = probes.Readiness.Healthy(context.Background())
	fmt.Println(live, ready)

	reachable = true
	ready, _ = probes.Readiness.Healthy(context.Background())
	fmt.Println(live, ready)

	// Output: true false
	// true false
	// true true
}
