// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/job/billing/payments"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
)

func scenario(t *testing.T, rules ...fit.Rule) string {
	t.Helper()
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	tenant, err := baggage.NewMemberRaw("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(tenant)
	require.NoError(t, err)
	b, err = codec.Encode(b, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	return b.String()
}

func evaluations(tel *humustest.Telemetry) []string {
	var points []string
	for _, s := range tel.Spans() {
		for _, ev := range s.Events() {
			if ev.Name != "humus.fit.evaluation" {
				continue
			}
			for _, kv := range ev.Attributes {
				if kv.Key == "humus.fit.point" {
					points = append(points, kv.Value.AsString())
				}
			}
		}
	}
	return points
}

func TestInit_an_abort_at_the_payments_API_opens_the_breaker_and_stops_the_run(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		accounts := make([]string, 10)
		for i := range accounts {
			accounts[i] = "acct-" + strconv.Itoa(i)
		}
		j := newJob(t, accounts...)

		st := j.run(t, j.inMemory, map[string]string{
			"BAGGAGE": scenario(t, fit.Rule{Point: "http:*", Fault: fit.Fault{Abort: true}}),
		})

		require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, j.api.sent())
		require.ErrorIs(t, st.Cause(), fit.ErrInjected)
		require.ErrorIs(t, st.Cause(), circuit.ErrRejected)
		require.ErrorIs(t, st.Cause(), payments.ErrStop)
		var injected *fault.InjectedError
		require.ErrorAs(t, st.Cause(), &injected)
		require.Equal(t, "http:payments.internal:8443", injected.Point)
		require.ErrorContains(t, st.Cause(), "stopped early, leaving 10 of 10 invoices pending")
		invoices := j.store.all()
		require.Len(t, invoices, 10)
		for _, inv := range invoices {
			require.Equal(t, "pending", inv.state)
		}
	})
}

func TestInit_a_delay_at_the_payments_API_still_charges_everything(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := newJob(t, "acct-1", "acct-2", "acct-3")
		start := time.Now()

		st := j.run(t, j.inMemory, map[string]string{
			"BAGGAGE":             scenario(t, fit.Rule{Point: "http:*", Fault: fit.Fault{Delay: 2 * time.Second}}),
			"BILLING_CONCURRENCY": "1",
		})

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.GreaterOrEqual(t, time.Since(start), 6*time.Second)
		require.Len(t, j.api.sent(), 3)
		for _, inv := range j.store.all() {
			require.Equal(t, "charged", inv.state)
			require.Equal(t, "ch_"+inv.ID.String(), inv.chargeID)
		}
	})
}

func TestInit_no_scenario_reaches_the_payments_API(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := newJob(t, "acct-1", "acct-2")
		tel := humustest.NewTelemetry()

		st := j.run(t, j.inMemory, map[string]string{
			"BAGGAGE": scenario(t, fit.Rule{Point: "postgresql:*", Fault: fit.Fault{Abort: true}}),
		}, recorded(tel))

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Contains(t, evaluations(tel), "http:payments.internal:8443")
		sent := j.api.sent()
		require.Len(t, sent, 2)
		for _, c := range sent {
			require.Contains(t, c.baggage, "tenant=acme")
			require.NotContains(t, c.baggage, "acme.fault")
		}
	})
}

func TestInit_without_BAGGAGE_no_run_carries_a_scenario(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := newJob(t, "acct-1", "acct-2")
		tel := humustest.NewTelemetry()

		st := j.run(t, j.inMemory, map[string]string{"FAULT_MAX_DELAY": "1s"}, recorded(tel))

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, evaluations(tel))
		sent := j.api.sent()
		require.Len(t, sent, 2)
		for _, c := range sent {
			require.Empty(t, c.baggage)
		}
	})
}
