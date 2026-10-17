// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package job

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/propagation"
)

var _ propagation.TextMapCarrier = envCarrier{}

func TestEnvCarrier_Get_reads_the_keys_normalized_name(t *testing.T) {
	t.Parallel()

	cases := []struct {
		key, name string
	}{
		{"traceparent", "TRACEPARENT"},
		{"TRACEPARENT", "TRACEPARENT"},
		{"x-b3-traceid", "X_B3_TRACEID"},
		{"x-test.parent", "X_TEST_PARENT"},
		{"", "_"},
		{"1abc", "_1ABC"},
		{"_already_OK_1", "_ALREADY_OK_1"},
		{"héllo wörld", "H_LLO_W_RLD"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.name, envName(tc.key))

			c := envCarrier{env: bedrock.NewStaticEnvironment(map[string]string{tc.name: "value"})}
			require.Equal(t, "value", c.Get(tc.key))
		})
	}
}

func TestEnvCarrier_a_normalized_name_is_unchanged(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"TRACEPARENT", "TRACESTATE", "BAGGAGE", "_", "X_B3_TRACEID", "_1ABC"} {
		require.Equal(t, name, envName(name))
	}
}

func TestEnvCarrier_Get_never_reads_the_keys_own_spelling(t *testing.T) {
	t.Parallel()

	c := envCarrier{env: bedrock.NewStaticEnvironment(map[string]string{
		"traceparent":  "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"x-b3-traceid": "4bf92f3577b34da6a3ce929d0e0e4736",
	})}
	require.Empty(t, c.Get("traceparent"))
	require.Empty(t, c.Get("x-b3-traceid"))
}

func TestEnvCarrier_Get_returns_empty_when_unset(t *testing.T) {
	t.Parallel()

	require.Empty(t, envCarrier{env: bedrock.NewStaticEnvironment(nil)}.Get("traceparent"))
	require.Empty(t, envCarrier{}.Get("traceparent"), "a nil environment has nothing set")
}

func TestEnvCarrier_Keys_returns_nil_and_Set_changes_nothing(t *testing.T) {
	t.Parallel()

	c := envCarrier{env: bedrock.NewStaticEnvironment(map[string]string{"TRACEPARENT": "before"})}
	require.Nil(t, c.Keys())

	c.Set("traceparent", "after")
	c.Set("tracestate", "after")
	require.Equal(t, "before", c.Get("traceparent"))
	require.Empty(t, c.Get("tracestate"))
	require.Nil(t, c.Keys())
}
