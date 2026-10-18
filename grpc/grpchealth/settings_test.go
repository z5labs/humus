// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpchealth_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
var fieldNames = []string{"Settings", "Addr", "address"}

func TestCheckAddr(t *testing.T) {
	t.Parallel()

	accept := []string{
		":8081",
		":0",
		"localhost:8081",
		"0.0.0.0:65535",
		"[::1]:443",
		"[::]:0",
	}
	reject := []string{
		"",
		"localhost",
		"::1",
		"localhost:",
		":65536",
		":-1",
		":grpc",
		"dns:///localhost:8081",
	}

	for _, addr := range accept {
		require.NoError(t, grpchealth.CheckAddr(addr), "valid value %q", addr)
	}
	for _, addr := range reject {
		err := grpchealth.CheckAddr(addr)
		require.Error(t, err, "invalid value %q", addr)
		for _, name := range fieldNames {
			require.NotContains(t, err.Error(), name, "the error of invalid value %q names a field", addr)
		}
	}

	t.Run("accepts and rejects what grpcserver.CheckAddr does", func(t *testing.T) {
		t.Parallel()
		for _, addr := range append(accept, reject...) {
			require.Equal(t, grpcserver.CheckAddr(addr) == nil, grpchealth.CheckAddr(addr) == nil, "value %q", addr)
		}
	})
}

func TestSettings_zero_value_fails_its_check(t *testing.T) {
	t.Parallel()

	var s grpchealth.Settings
	require.Error(t, grpchealth.CheckAddr(s.Addr))
}
