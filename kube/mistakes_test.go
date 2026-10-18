// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildMistake builds testdata/mistakes/common.go with file, behind the
// mistakes build tag, and returns the compiler's output and its error.
func buildMistake(t *testing.T, file string) (string, error) {
	t.Helper()
	gobin, err := exec.LookPath("go")
	require.NoError(t, err, "the go command builds the mistakes")
	dir := filepath.Join("testdata", "mistakes")
	cmd := exec.CommandContext(t.Context(), gobin, "build", "-tags", "mistakes",
		filepath.Join(dir, "common.go"), filepath.Join(dir, file))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestMistakes_dont_compile checks that each mistake the types of Field and
// FieldOf rule out is a compile error, in its own file, for the reason it
// is there. ok.go shows that the code every mistake shares compiles.
func TestMistakes_dont_compile(t *testing.T) {
	t.Parallel()

	t.Run("ok.go compiles", func(t *testing.T) {
		t.Parallel()
		out, err := buildMistake(t, "ok.go")
		require.NoError(t, err, out)
	})

	cases := []struct {
		name string
		file string
		want string
	}{
		{"a misspelled constraint", "misspelled_constraint.go", "kube.FieldOf[int32] has no field or method Minimun"},
		{"a Minimum of another type", "minimum_other_type.go", `cannot use "one" (untyped string constant) as int32 value`},
		{"a Default of another type", "default_other_type.go", "cannot use 3 (untyped int constant) as string value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := buildMistake(t, tc.file)
			require.Error(t, err, "%s compiled", tc.file)
			require.Contains(t, out, filepath.Join("testdata", "mistakes", tc.file)+":", "the error is in %s", tc.file)
			require.Contains(t, out, tc.want)
		})
	}
}
