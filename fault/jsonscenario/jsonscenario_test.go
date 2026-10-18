// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jsonscenario_test

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"

	"github.com/z5labs/humus/fault/jsonscenario"
)

// key is the member the tests' Codec reads and writes.
const key = "acme.fault"

// newCodec returns the Codec of key.
func newCodec(t *testing.T) jsonscenario.Codec {
	t.Helper()
	c, err := jsonscenario.New(key)
	require.NoError(t, err)
	return c
}

// bag returns baggage of the members kv names, in pairs of key and raw
// value.
func bag(t *testing.T, kv ...string) baggage.Baggage {
	t.Helper()
	var members []baggage.Member
	for i := 0; i < len(kv); i += 2 {
		m, err := baggage.NewMemberRaw(kv[i], kv[i+1])
		require.NoError(t, err)
		members = append(members, m)
	}
	b, err := baggage.New(members...)
	require.NoError(t, err)
	return b
}

// scenario is the post's example scenario.
var scenario = fit.Scenario{Rules: []fit.Rule{
	{Point: "http:payments:8443", Fault: fit.Fault{Abort: true}},
	{Point: "*", Fault: fit.Fault{Delay: 250 * time.Millisecond}},
}}

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("accepts a W3C baggage key", func(t *testing.T) {
		t.Parallel()
		for _, k := range []string{"acme.fault", "fault", "a", "A-Z_0.9", "!#$%&'*+-.^_`|~"} {
			_, err := jsonscenario.New(k)
			require.NoError(t, err, k)
		}
	})

	t.Run("rejects a key that isn't a W3C baggage key", func(t *testing.T) {
		t.Parallel()
		for _, k := range []string{"", "acme fault", "acme=fault", "acme,fault", "acme;fault", `"acme"`, "acme/fault", "ãcme", "acme\tfault", "(acme)"} {
			c, err := jsonscenario.New(k)
			require.Error(t, err, "%q", k)
			require.ErrorContains(t, err, "not a valid W3C baggage key")
			require.Equal(t, jsonscenario.Codec{}, c)
		}
	})
}

func TestCodec_Decode(t *testing.T) {
	t.Parallel()

	t.Run("reports false when the baggage has no member with the key", func(t *testing.T) {
		t.Parallel()
		for name, b := range map[string]baggage.Baggage{
			"empty":         {},
			"other members": bag(t, "tenant", "acme", "other.fault", `{"rules":[{"point":"*","abort":true}]}`),
		} {
			s, ok, err := newCodec(t).Decode(b)
			require.NoError(t, err, name)
			require.False(t, ok, name)
			require.Equal(t, fit.Scenario{}, s, name)
		}
	})

	t.Run("reads the grammar", func(t *testing.T) {
		t.Parallel()
		cases := map[string]struct {
			value string
			want  fit.Scenario
		}{
			"the post's example": {
				value: `{"rules":[{"point":"http:payments:8443","abort":true},{"point":"*","delay_ms":250}]}`,
				want:  scenario,
			},
			"abort defaults to false and delay_ms to 0": {
				value: `{"rules":[{"point":"kafka:orders"}]}`,
				want:  fit.Scenario{Rules: []fit.Rule{{Point: "kafka:orders"}}},
			},
			"abort false and delay_ms 0 written out": {
				value: `{"rules":[{"point":"kafka:orders","delay_ms":0,"abort":false}]}`,
				want:  fit.Scenario{Rules: []fit.Rule{{Point: "kafka:orders"}}},
			},
			"a delay and an abort together": {
				value: `{"rules":[{"abort":true,"delay_ms":1500,"point":"grpc:*"}]}`,
				want:  fit.Scenario{Rules: []fit.Rule{{Point: "grpc:*", Fault: fit.Fault{Delay: 1500 * time.Millisecond, Abort: true}}}},
			},
			"white space around it": {
				value: " \n{ \"rules\" : [ { \"point\" : \"*\" } ] }\t\n",
				want:  fit.Scenario{Rules: []fit.Rule{{Point: "*"}}},
			},
			"no rules": {
				value: `{"rules":[]}`,
				want:  fit.Scenario{Rules: []fit.Rule{}},
			},
			"no rules field": {
				value: `{}`,
				want:  fit.Scenario{Rules: []fit.Rule{}},
			},
			"null rules": {
				value: `{"rules":null}`,
				want:  fit.Scenario{Rules: []fit.Rule{}},
			},
			"a delay_ms too large for a time.Duration reads as the longest one": {
				value: `{"rules":[{"point":"*","delay_ms":99999999999999999999999}]}`,
				want:  fit.Scenario{Rules: []fit.Rule{{Point: "*", Fault: fit.Fault{Delay: time.Duration(math.MaxInt64/int64(time.Millisecond)) * time.Millisecond}}}},
			},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				s, ok, err := newCodec(t).Decode(bag(t, key, tc.value, "tenant", "acme"))
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, tc.want, s)
			})
		}
	})

	t.Run("reads any size that arrives", func(t *testing.T) {
		t.Parallel()
		point := strings.Repeat("p", 6000)
		s, ok, err := newCodec(t).Decode(bag(t, key, `{"rules":[{"point":"`+point+`","delay_ms":5}]}`))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, fit.Scenario{Rules: []fit.Rule{{Point: point, Fault: fit.Fault{Delay: 5 * time.Millisecond}}}}, s)
	})

	t.Run("reports an error for a member that breaks the grammar", func(t *testing.T) {
		t.Parallel()
		cases := map[string]struct {
			value string
			want  string
		}{
			"an unknown field":            {value: `{"rules":[{"point":"*","abrot":true}]}`, want: `unknown field "abrot"`},
			"an unknown top-level field":  {value: `{"rules":[],"version":1}`, want: `unknown field "version"`},
			"a missing point":             {value: `{"rules":[{"point":"*"},{"abort":true}]}`, want: "rules[1]: point is required"},
			"an empty point":              {value: `{"rules":[{"point":"","abort":true}]}`, want: "rules[0]: point is required"},
			"a null point":                {value: `{"rules":[{"point":null,"abort":true}]}`, want: "rules[0]: point is required"},
			"a null rule":                 {value: `{"rules":[null]}`, want: "rules[0]: point is required"},
			"a negative delay_ms":         {value: `{"rules":[{"point":"*","delay_ms":-250}]}`, want: "delay_ms must be a non-negative whole number"},
			"a fractional delay_ms":       {value: `{"rules":[{"point":"*","delay_ms":1.5}]}`, want: "delay_ms must be a non-negative whole number"},
			"an exponent in delay_ms":     {value: `{"rules":[{"point":"*","delay_ms":1e3}]}`, want: "delay_ms must be a non-negative whole number"},
			"a delay_ms string":           {value: `{"rules":[{"point":"*","delay_ms":"250"}]}`, want: "delay_ms must be a non-negative whole number"},
			"a null delay_ms":             {value: `{"rules":[{"point":"*","delay_ms":null}]}`, want: "delay_ms must be a non-negative whole number"},
			"an abort string":             {value: `{"rules":[{"point":"*","abort":"true"}]}`, want: "abort"},
			"a point number":              {value: `{"rules":[{"point":7}]}`, want: "point"},
			"trailing data":               {value: `{"rules":[{"point":"*"}]}x`, want: "trailing data"},
			"a second object":             {value: `{"rules":[]}{"rules":[]}`, want: "trailing data"},
			"null":                        {value: `null`, want: "not null"},
			"an array, as fit's examples": {value: `[{"point":"*","abort":true}]`, want: "cannot unmarshal array"},
			"not JSON":                    {value: `abort`, want: "invalid character"},
			"empty":                       {value: ``, want: "EOF"},
			"truncated":                   {value: `{"rules":[{"point":"*"`, want: "unexpected EOF"},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				s, ok, err := newCodec(t).Decode(bag(t, key, tc.value))
				require.Error(t, err)
				require.ErrorContains(t, err, "baggage member "+key)
				require.ErrorContains(t, err, tc.want)
				require.True(t, ok)
				require.Equal(t, fit.Scenario{}, s)
			})
		}
	})
}

func TestCodec_Encode(t *testing.T) {
	t.Parallel()

	t.Run("writes the scenario into the member", func(t *testing.T) {
		t.Parallel()
		b, err := newCodec(t).Encode(bag(t, "tenant", "acme"), scenario)
		require.NoError(t, err)
		require.Equal(t, `{"rules":[{"point":"http:payments:8443","abort":true},{"point":"*","delay_ms":250}]}`, b.Member(key).Value())
		require.Equal(t, "acme", b.Member("tenant").Value())
		require.Equal(t, 2, b.Len())
	})

	t.Run("percent-encodes the member as the package doc shows", func(t *testing.T) {
		t.Parallel()
		b, err := newCodec(t).Encode(baggage.Baggage{}, scenario)
		require.NoError(t, err)
		require.Equal(t, "baggage: "+b.Member(key).String(), "baggage: acme.fault={%22rules%22:[{%22point%22:%22http:payments:8443%22%2C%22abort%22:true}%2C{%22point%22:%22*%22%2C%22delay_ms%22:250}]}")
	})

	t.Run("writes a scenario with no rules as an empty array", func(t *testing.T) {
		t.Parallel()
		b, err := newCodec(t).Encode(baggage.Baggage{}, fit.Scenario{})
		require.NoError(t, err)
		require.Equal(t, `{"rules":[]}`, b.Member(key).Value())
	})

	t.Run("replaces any scenario the member held", func(t *testing.T) {
		t.Parallel()
		c := newCodec(t)
		b := bag(t, key, `{"rules":[{"point":"*","abort":true}]}`, "tenant", "acme")
		b, err := c.Encode(b, fit.Scenario{Rules: []fit.Rule{{Point: "kafka:orders", Fault: fit.Fault{Delay: time.Second}}}})
		require.NoError(t, err)
		require.Equal(t, `{"rules":[{"point":"kafka:orders","delay_ms":1000}]}`, b.Member(key).Value())
		require.Equal(t, 2, b.Len())

		s, ok, err := c.Decode(b)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, fit.Scenario{Rules: []fit.Rule{{Point: "kafka:orders", Fault: fit.Fault{Delay: time.Second}}}}, s)
	})

	t.Run("rejects a member over 2048 bytes percent-encoded", func(t *testing.T) {
		t.Parallel()
		c := newCodec(t)
		// Grow the point one byte at a time: the last member Encode accepts
		// is exactly 2048 bytes on the wire, and the next is rejected.
		var last string
		for n := 1; ; n++ {
			in := bag(t, "tenant", "acme")
			b, err := c.Encode(in, fit.Scenario{Rules: []fit.Rule{{Point: strings.Repeat("p", n), Fault: fit.Fault{Abort: true}}}})
			if err != nil {
				require.ErrorContains(t, err, "2049 bytes percent-encoded, over the limit of 2048")
				require.Equal(t, in, b, "Encode returns the baggage unchanged")
				break
			}
			last = b.Member(key).String()
		}
		require.Len(t, last, 2048)
	})

	t.Run("counts the percent-encoding toward the limit", func(t *testing.T) {
		t.Parallel()
		// Each comma in the point is three bytes on the wire, %2C, so the
		// member is over the limit though its JSON is well under it.
		point := strings.Repeat(",", 700)
		_, err := newCodec(t).Encode(baggage.Baggage{}, fit.Scenario{Rules: []fit.Rule{{Point: point}}})
		require.ErrorContains(t, err, "over the limit of 2048")
	})

	t.Run("rejects a Delay that isn't whole milliseconds", func(t *testing.T) {
		t.Parallel()
		for _, d := range []time.Duration{1500 * time.Microsecond, time.Nanosecond, time.Second + time.Microsecond} {
			in := bag(t, "tenant", "acme")
			b, err := newCodec(t).Encode(in, fit.Scenario{Rules: []fit.Rule{{Point: "*"}, {Point: "http:payments:8443", Fault: fit.Fault{Delay: d}}}})
			require.ErrorContains(t, err, "rules[1] (http:payments:8443): delay "+d.String()+" is not a whole number of milliseconds")
			require.Equal(t, in, b)
		}
	})

	t.Run("rejects a negative Delay", func(t *testing.T) {
		t.Parallel()
		_, err := newCodec(t).Encode(baggage.Baggage{}, fit.Scenario{Rules: []fit.Rule{{Point: "*", Fault: fit.Fault{Delay: -time.Millisecond}}}})
		require.ErrorContains(t, err, "rules[0] (*): delay -1ms is negative")
	})

	t.Run("rejects a rule with no point", func(t *testing.T) {
		t.Parallel()
		_, err := newCodec(t).Encode(baggage.Baggage{}, fit.Scenario{Rules: []fit.Rule{{Fault: fit.Fault{Abort: true}}}})
		require.ErrorContains(t, err, "rules[0]: point is required")
	})

	t.Run("rejects a point that isn't valid UTF-8", func(t *testing.T) {
		t.Parallel()
		_, err := newCodec(t).Encode(baggage.Baggage{}, fit.Scenario{Rules: []fit.Rule{{Point: "http:\xff"}}})
		require.ErrorContains(t, err, "is not valid UTF-8")
	})
}

func TestCodec_Strip(t *testing.T) {
	t.Parallel()

	t.Run("removes the member and leaves every other member alone", func(t *testing.T) {
		t.Parallel()
		b := bag(t, key, `{"rules":[{"point":"*","abort":true}]}`, "tenant", "acme", "other.fault", `{"rules":[]}`, "acme.faults", "x")
		got := newCodec(t).Strip(b)
		require.Equal(t, bag(t, "tenant", "acme", "other.fault", `{"rules":[]}`, "acme.faults", "x"), got)
	})

	t.Run("leaves baggage without the member alone", func(t *testing.T) {
		t.Parallel()
		b := bag(t, "tenant", "acme")
		require.Equal(t, b, newCodec(t).Strip(b))
	})

	t.Run("strips a member that breaks the grammar", func(t *testing.T) {
		t.Parallel()
		b := bag(t, key, "not json", "tenant", "acme")
		require.Equal(t, bag(t, "tenant", "acme"), newCodec(t).Strip(b))
	})
}

func TestCodec_zero_value(t *testing.T) {
	t.Parallel()

	var c jsonscenario.Codec
	b := bag(t, key, `{"rules":[{"point":"*","abort":true}]}`, "tenant", "acme")

	s, ok, err := c.Decode(b)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, fit.Scenario{}, s)

	require.Equal(t, b, c.Strip(b))

	got, err := c.Encode(b, scenario)
	require.ErrorContains(t, err, "build one with New")
	require.Equal(t, b, got)
}

func TestCodec_round_trips_through_the_baggage_propagator(t *testing.T) {
	t.Parallel()

	c := newCodec(t)
	scenarios := map[string]fit.Scenario{
		"the post's example": scenario,
		"no rules":           {Rules: []fit.Rule{}},
		"points with characters baggage escapes": {Rules: []fit.Rule{
			{Point: `http:a,b;c=d "e" %20 \ é 🙂:8443`, Fault: fit.Fault{Abort: true}},
			{Point: "postgresql:*", Fault: fit.Fault{Delay: 2 * time.Minute, Abort: true}},
		}},
	}
	for name, want := range scenarios {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tenant, err := baggage.NewMemberRaw("tenant", "acme")
			require.NoError(t, err)
			b, err := baggage.New(tenant)
			require.NoError(t, err)
			ctx := baggage.ContextWithBaggage(context.Background(), b)

			// The edge, or a test, writes the scenario.
			ctx, err = fit.ContextWithScenario(ctx, c, want)
			require.NoError(t, err)

			// A propagator carries it to the next service.
			carrier := propagation.MapCarrier{}
			propagation.Baggage{}.Inject(ctx, carrier)
			require.NotEmpty(t, carrier.Get("baggage"))
			got := propagation.Baggage{}.Extract(context.Background(), carrier)

			s, ok, err := fit.ScenarioFromContext(got, c)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, want, s)
			require.Equal(t, "acme", baggage.FromContext(got).Member("tenant").Value())

			// Stripped, the scenario is gone and the tenant remains.
			stripped := fit.ContextWithoutScenario(got, c)
			_, ok, err = fit.ScenarioFromContext(stripped, c)
			require.NoError(t, err)
			require.False(t, ok)
			require.Equal(t, "acme", baggage.FromContext(stripped).Member("tenant").Value())
		})
	}
}

func TestCodec_drives_a_fit_Evaluator(t *testing.T) {
	t.Parallel()

	c := newCodec(t)
	var mu sync.Mutex
	var evals []fit.Evaluation
	eval, err := fit.NewEvaluator(c, 10*time.Second, func(_ context.Context, e fit.Evaluation) {
		mu.Lock()
		defer mu.Unlock()
		evals = append(evals, e)
	})
	require.NoError(t, err)

	ctx := baggage.ContextWithBaggage(context.Background(), bag(t, key, `{"rules":[{"point":"http:payments:8443","abrot":true}]}`))
	require.Equal(t, fit.Fault{}, eval.Eval(ctx, "http:payments:8443"))
	require.Len(t, evals, 1)
	require.ErrorContains(t, evals[0].Err, `unknown field "abrot"`)

	ctx = baggage.ContextWithBaggage(context.Background(), bag(t, key, `{"rules":[{"point":"http:payments:8443","abort":true}]}`))
	require.Equal(t, fit.Fault{Abort: true}, eval.Eval(ctx, "http:payments:8443"))
	require.Equal(t, fit.Fault{}, eval.Eval(ctx, "kafka:orders"))
}

func TestCodec_is_safe_for_concurrent_use(t *testing.T) {
	t.Parallel()

	c := newCodec(t)
	in := bag(t, "tenant", "acme")
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 50 {
				b, err := c.Encode(in, scenario)
				if err != nil {
					t.Error(err)
					return
				}
				s, ok, err := c.Decode(b)
				if err != nil || !ok {
					t.Error(ok, err)
					return
				}
				if len(s.Rules) != 2 {
					t.Error(s)
					return
				}
				if c.Strip(b).Len() != 1 {
					t.Error("strip")
					return
				}
			}
		})
	}
	wg.Wait()
}
