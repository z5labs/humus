// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/kafka/orderstatus/projector"
	"github.com/z5labs/humus/fault/jsonscenario"
)

var trustingBoth = map[string]string{"FAULT_TRUSTED_TOPICS": eventsTopic + "," + retryTopic}

func codec(t *testing.T) jsonscenario.Codec {
	t.Helper()
	c, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	return c
}

func withScenario(t *testing.T, r *kgo.Record, rules ...fit.Rule) *kgo.Record {
	t.Helper()
	b, err := codec(t).Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	r.Headers = append(r.Headers, kgo.RecordHeader{Key: "baggage", Value: []byte(b.String())})
	return r
}

func carriesScenario(t *testing.T, r *kgo.Record) bool {
	t.Helper()
	var members []string
	for _, h := range r.Headers {
		if h.Key == "baggage" {
			members = append(members, string(h.Value))
		}
	}
	b, err := baggage.Parse(strings.Join(members, ","))
	require.NoError(t, err)
	_, carried, err := codec(t).Decode(b)
	require.NoError(t, err)
	return carried
}

func headerOf(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func TestInit_delays_PostgreSQL_from_a_trusted_scenario(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, nil)
		c := p.cluster(t)
		p.produce(t, c, withScenario(t, event("order-1", projector.Placed, time.Now().UTC()),
			fit.Rule{Point: "postgresql:orders", Fault: fit.Fault{Delay: time.Second}},
		))

		signal, status := p.start(t, trustingBoth, humus.WithFaults(Faults))
		start := time.Now()
		waitFor(t, "order-1 is projected", func() bool { return committed(c, eventsTopic) == 1 })
		took := time.Since(start)
		signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.GreaterOrEqual(t, took, 3*time.Second)
		require.Equal(t, projector.Placed, p.store.status("order-1"))
		changes := p.readAll(t, c, statusTopic)
		require.Len(t, changes, 1)
		require.False(t, carriesScenario(t, changes[0]), "the status event carries the scenario")
		require.Empty(t, p.readAll(t, c, retryTopic))
		require.Empty(t, p.readAll(t, c, deadLetterTopic))
	})
}

func TestInit_dead_letters_a_record_whose_trusted_scenario_aborts_PostgreSQL_on_every_attempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, nil)
		c := p.cluster(t)
		now := time.Now().UTC()
		p.produce(t, c,
			withScenario(t, event("order-1", projector.Placed, now),
				fit.Rule{Point: "postgresql:orders", Fault: fit.Fault{Abort: true}},
			),
			event("order-2", projector.Placed, now),
		)

		env := map[string]string{
			"FAULT_TRUSTED_TOPICS": trustingBoth["FAULT_TRUSTED_TOPICS"],
			"RETRY_ATTEMPTS":       "3",
			"RETRY_MIN_DELAY":      "1s",
			"RETRY_MAX_DELAY":      "1s",
		}
		signal, status := p.start(t, env, humus.WithFaults(Faults))
		waitFor(t, "order-1 is dead-lettered", func() bool {
			return committed(c, eventsTopic) == 2 && committed(c, retryTopic) == 2
		})
		signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.Empty(t, p.store.status("order-1"))
		require.Equal(t, projector.Placed, p.store.status("order-2"))

		retries := p.readAll(t, c, retryTopic)
		require.Len(t, retries, 2)
		for i, r := range retries {
			require.Equal(t, "order-1", string(r.Key))
			require.Equal(t, "unavailable", headerOf(r, "retry.reason"))
			require.Contains(t, headerOf(r, "retry.error"), "postgresql:orders")
			require.True(t, carriesScenario(t, r), "retry %d lost the scenario", i+1)
		}

		dead := p.readAll(t, c, deadLetterTopic)
		require.Len(t, dead, 1)
		require.Equal(t, "order-1", string(dead[0].Key))
		require.Equal(t, "unavailable", headerOf(dead[0], "dlq.reason"))
		require.Equal(t, "3", headerOf(dead[0], "dlq.attempts"))
		require.Equal(t, eventsTopic, headerOf(dead[0], "dlq.topic"))
		require.Equal(t, "0", headerOf(dead[0], "dlq.offset"))
		require.False(t, carriesScenario(t, dead[0]), "the dead letter carries the scenario")

		changes := p.readAll(t, c, statusTopic)
		require.Len(t, changes, 1)
		require.Equal(t, "order-2", string(changes[0].Key))
	})
}

func TestInit_strips_the_scenario_of_a_record_from_a_topic_it_does_not_trust(t *testing.T) {
	testCases := map[string]map[string]string{
		"with no trusted topics":            nil,
		"with only the retry topic trusted": {"FAULT_TRUSTED_TOPICS": retryTopic},
	}
	for desc, env := range testCases {
		t.Run(desc, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newProcessor(t, nil)
				c := p.cluster(t)
				p.produce(t, c, withScenario(t, event("order-1", projector.Placed, time.Now().UTC()),
					fit.Rule{Point: "postgresql:orders", Fault: fit.Fault{Abort: true}},
				))

				signal, status := p.start(t, env, humus.WithFaults(Faults))
				waitFor(t, "order-1 is projected", func() bool { return committed(c, eventsTopic) == 1 })
				signal(syscall.SIGTERM)
				st := <-status
				require.Zero(t, st.Code(), "cause: %v", st.Cause())

				require.Equal(t, projector.Placed, p.store.status("order-1"))
				changes := p.readAll(t, c, statusTopic)
				require.Len(t, changes, 1)
				require.False(t, carriesScenario(t, changes[0]), "the status event carries the scenario")
				require.Empty(t, p.readAll(t, c, retryTopic))
				require.Empty(t, p.readAll(t, c, deadLetterTopic))
			})
		})
	}
}

func TestFaults_with_a_bad_maximum_delay_exits_78(t *testing.T) {
	for desc, value := range map[string]string{
		"a negative delay":     "-1s",
		"a delay with no unit": "10",
	} {
		t.Run(desc, func(t *testing.T) {
			var cause error
			synctest.Test(t, func(t *testing.T) {
				p := newProcessor(t, nil)
				_, status := p.start(t, map[string]string{"FAULT_MAX_DELAY": value}, humus.WithFaults(Faults))
				st := <-status
				cause = st.Cause()
				require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", cause)
			})
			require.ErrorContains(t, cause, "config FAULT_MAX_DELAY:")
		})
	}
}
