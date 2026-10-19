// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildMistake builds testdata/mistakes/common.go with file, behind the
// mistakes build tag, and returns the compiler's output and its error.
//
// It builds with -race when the test binary was, so the packages the
// mistakes import are the ones go test already compiled for the binary.
// Built without it, on a cold Go cache, they are all compiled again, which
// took minutes of the Kubernetes (testcontainers) job.
func buildMistake(t *testing.T, file string) (string, error) {
	t.Helper()
	gobin, err := exec.LookPath("go")
	require.NoError(t, err, "the go command builds the mistakes")
	args := []string{"build", "-tags", "mistakes"}
	if raceEnabled() {
		args = append(args, "-race")
	}
	dir := filepath.Join("testdata", "mistakes")
	args = append(args, filepath.Join(dir, "common.go"), filepath.Join(dir, file))
	cmd := exec.CommandContext(t.Context(), gobin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// raceEnabled reports whether the test binary was built with -race.
func raceEnabled() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, s := range info.Settings {
		if s.Key == "-race" {
			return s.Value == "true"
		}
	}
	return false
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
