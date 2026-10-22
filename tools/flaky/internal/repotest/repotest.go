// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package repotest builds a throwaway git repository from testdata/repo, for
// the tests of what flaky discovers and runs.
package repotest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/flaky/internal/env"
)

// New returns the directory of a new git repository holding a copy of
// testdata/repo, the repository's real .github/scripts/go-modules.sh, and a
// .github/workflows/build.yaml that sets goVersion, such as 1.27.2.
func New(t *testing.T, goVersion string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	flaky := filepath.Join(filepath.Dir(file), "..", "..")
	dir := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.CopyFS(dir, os.DirFS(filepath.Join(flaky, "testdata", "repo"))))

	script, err := os.ReadFile(filepath.Join(flaky, "..", "..", ".github", "scripts", "go-modules.sh"))
	require.NoError(t, err)
	Write(t, dir, ".github/scripts/go-modules.sh", string(script))
	require.NoError(t, os.Chmod(filepath.Join(dir, ".github", "scripts", "go-modules.sh"), 0o755))
	Write(t, dir, ".github/workflows/build.yaml", "jobs:\n  test:\n    steps:\n      - uses: actions/setup-go@v7\n        with:\n          go-version: '"+goVersion+"'\n")

	Git(t, dir, "init", "-q", "-b", "main")
	Commit(t, dir)
	return dir
}

// Write writes contents to the file name, a slash-separated path under dir,
// creating its directories.
func Write(t *testing.T, dir, name, contents string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
}

// Commit commits every change in the repository dir.
func Commit(t *testing.T, dir string) {
	t.Helper()
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-q", "--allow-empty", "-m", "change")
}

// Git runs git with args in dir, isolated from the user's and the system's
// git configuration.
func Git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(env.Child(bedrock.OSEnvironment{}),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}
