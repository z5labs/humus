// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jsonscenario_test

import (
	"context"
	"fmt"
	"time"

	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"

	"github.com/z5labs/humus/fault/jsonscenario"
)

func Example() {
	// The edge, or a test harness, and every service share one Codec. In a
	// service, it is the Decoder of the fault.Settings humus.WithFaults
	// returns.
	codec, err := jsonscenario.New("acme.fault")
	if err != nil {
		fmt.Println(err)
		return
	}

	// The edge writes a scenario into a request's baggage: fail the payments
	// call, and slow every other point down.
	tenant, _ := baggage.NewMemberRaw("tenant", "acme")
	b, _ := baggage.New(tenant)
	ctx := baggage.ContextWithBaggage(context.Background(), b)
	ctx, err = fit.ContextWithScenario(ctx, codec, fit.Scenario{Rules: []fit.Rule{
		{Point: "http:payments:8443", Fault: fit.Fault{Abort: true}},
		{Point: "*", Fault: fit.Fault{Delay: 250 * time.Millisecond}},
	}})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(baggage.FromContext(ctx).Member("acme.fault").Value())

	// A propagator carries it to the next service, which reads it back.
	carrier := propagation.MapCarrier{}
	propagation.Baggage{}.Inject(ctx, carrier)
	next := propagation.Baggage{}.Extract(context.Background(), carrier)

	s, ok, err := fit.ScenarioFromContext(next, codec)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(ok, s.Rules[0].Point, s.Rules[0].Fault.Abort, s.Rules[1].Point, s.Rules[1].Fault.Delay)

	// A misspelt field is an error, not a rule that spares its point.
	typo, _ := baggage.NewMemberRaw("acme.fault", `{"rules":[{"point":"*","abrot":true}]}`)
	b, _ = baggage.New(typo, tenant)
	_, _, err = codec.Decode(b)
	fmt.Println(err)

	// Stripped, as a server strips a caller it doesn't trust, the scenario
	// is gone and the tenant stays.
	b = codec.Strip(b)
	fmt.Println(b.String())
	// Output:
	// {"rules":[{"point":"http:payments:8443","abort":true},{"point":"*","delay_ms":250}]}
	// true http:payments:8443 true * 250ms
	// jsonscenario: baggage member acme.fault: json: unknown field "abrot"
	// tenant=acme
}
