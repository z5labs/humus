// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package v1alpha1

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func TestMemcachedCRD(t *testing.T) {
	crd, err := Memcached.CRD()
	require.NoError(t, err)
	got, err := yaml.Marshal(crd)
	require.NoError(t, err)

	const path = "../../config/crd/bases/cache.example.com_memcacheds.yaml"
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
}
