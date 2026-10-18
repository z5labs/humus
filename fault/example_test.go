// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package fault_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/fault"
)

// Registry is an end user's fit.Decoder: the baggage member acme.scenario
// carries the ID of one of its scenarios, which an edge of theirs sets on
// the requests under test. An ID it doesn't hold is rejected.
type Registry map[string]fit.Scenario

// Decode reads the scenario of the ID in b.
func (r Registry) Decode(b baggage.Baggage) (fit.Scenario, bool, error) {
	m := b.Member("acme.scenario")
	if m.Key() == "" {
		return fit.Scenario{}, false, nil
	}
	s, ok := r[m.Value()]
	if !ok {
		return fit.Scenario{}, false, fmt.Errorf("unknown scenario %q", m.Value())
	}
	return s, true, nil
}

// Strip removes the ID from b.
func (Registry) Strip(b baggage.Baggage) baggage.Baggage {
	return b.DeleteMember("acme.scenario")
}

// queryOrders is an injection point of the end user's own, named
// <system>:<name>. A fault aborts it with an *fault.InjectedError, as
// humus's clients fail.
func queryOrders(ctx context.Context, faults fault.Injector) error {
	const point = "postgresql:orders"
	f := faults.Eval(ctx, point)
	if err := f.Wait(ctx); err != nil {
		return &fault.InjectedError{Point: point, Err: err}
	}
	if f.Abort {
		return &fault.InjectedError{Point: point}
	}
	return nil // query the database
}

func Example() {
	settings := fault.DefaultSettings()
	settings.Decoder = Registry{
		"orders-down": {Rules: []fit.Rule{{Point: "postgresql:orders", Fault: fit.Fault{Abort: true}}}},
	}
	// In an App, humus.WithFaults returns these settings, and the App builds
	// the Injector before Init and puts it in Setup.Faults.
	faults, err := fault.New(settings)
	if err != nil {
		fmt.Println(err)
		return
	}

	// A request under test, as its server hands it on.
	id, _ := baggage.NewMemberRaw("acme.scenario", "orders-down")
	tenant, _ := baggage.NewMemberRaw("tenant", "acme")
	b, _ := baggage.New(id, tenant)
	ctx := baggage.ContextWithBaggage(context.Background(), b)

	err = queryOrders(ctx, faults)
	fmt.Println(err)
	fmt.Println(errors.Is(err, fit.ErrInjected))

	// Stripped, as a server does for a caller it doesn't trust, the request
	// carries no scenario, and keeps its other members.
	stripped, ok := faults.Strip(ctx)
	fmt.Println(ok, baggage.FromContext(stripped).String())
	fmt.Println(queryOrders(stripped, faults))

	// Output:
	// fit: injected failure at postgresql:orders
	// true
	// true tenant=acme
	// <nil>
}
