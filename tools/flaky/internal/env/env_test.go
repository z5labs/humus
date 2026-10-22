// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package env_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/flaky/internal/env"
)

func TestChild_passes_on_the_variables_it_knows_that_are_set(t *testing.T) {
	got := env.Child(bedrock.NewStaticEnvironment(map[string]string{
		"PATH":    "/bin",
		"GOFLAGS": "-mod=mod",
		"SECRET":  "not passed on",
	}))
	require.Equal(t, []string{"PATH=/bin", "GOFLAGS=-mod=mod"}, got)
}

func TestChild_is_never_nil_so_a_child_never_inherits_the_real_environment(t *testing.T) {
	got := env.Child(bedrock.NewStaticEnvironment(map[string]string{}))
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestWith_sets_a_variable_in_place_of_any_value_it_had(t *testing.T) {
	in := []string{"PATH=/bin", "KIND_CLUSTER_NAME=kind", "KIND_CLUSTER_NAME_X=y"}
	require.Equal(t, []string{"PATH=/bin", "KIND_CLUSTER_NAME_X=y", "KIND_CLUSTER_NAME=flaky-wallet"}, env.With(in, "KIND_CLUSTER_NAME", "flaky-wallet"))
	require.Equal(t, []string{"PATH=/bin", "KIND_CLUSTER_NAME=kind", "KIND_CLUSTER_NAME_X=y"}, in, "env isn't changed")
	require.Equal(t, []string{"A=b"}, env.With(nil, "A", "b"))
}
