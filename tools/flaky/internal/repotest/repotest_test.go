// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package repotest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/tools/flaky/internal/repotest"
)

func TestNew_copies_testdata_repo_into_a_git_repository(t *testing.T) {
	dir := repotest.New(t, "1.27.2")
	for _, name := range []string{".git", "go.mod", "b/go.mod", ".github/scripts/go-modules.sh", ".github/workflows/build.yaml"} {
		_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		require.NoError(t, err, name)
	}
	build, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "build.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(build), "go-version: '1.27.2'")
}
