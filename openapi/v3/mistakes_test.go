// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

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

// TestMistakes_dont_compile checks that each mistake the types rule out is
// a compile error, in its own file, for the reason it is there. ok.go
// shows that the code every mistake shares compiles.
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
		{"a missing codec argument", "missing_codec.go", "not enough arguments in call to openapi.Post"},
		{"a decoder and an encoder swapped", "swapped_codecs.go", "of encoder does not match inferred type func() openapi.Decoder[item]"},
		{"a decoder given to Error", "decoder_to_error.go", "of decoder does not match func() openapi.Encoder[notFound]"},
		{"a decoder given to CatchAll", "decoder_to_catch_all.go", "of decoder does not match inferred type func() openapi.Encoder[problem]"},
		{"a decoder given to Get", "decoder_to_get.go", "of decoder does not match inferred type func() openapi.Encoder[cart]"},
		{"a non-generic codec of another type", "other_type.go", "of otherEncoder does not match inferred type func() openapi.Encoder[cart]"},
		{"NewAPI without its RequestErrors", "omitted_request_errors.go", "not enough arguments in call to openapi.NewAPI"},
		{"NewAPI without its catch-all", "omitted_catch_all.go", "not enough arguments in call to openapi.NewAPI"},
		{"a catch-all where RequestErrors goes", "no_request_errors.go", "openapi.Fallback does not implement openapi.RequestErrorAnswer"},
		{"RequestErrors where the catch-all goes", "no_catch_all.go", "openapi.RequestErrorAnswer does not implement openapi.Fallback"},
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
