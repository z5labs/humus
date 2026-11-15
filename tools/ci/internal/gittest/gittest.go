// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package gittest builds throwaway git repositories, each with a bare
// repository as its remote origin, for the tests of the tools that read
// one. They touch nothing outside the test's temporary directory.
package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
)

// names are the variables of this process the tools' git and go commands are
// given in these tests, when they are set: the go command's and git's, so
// that go list and go mod edit work offline from the module cache.
var names = []string{
	"PATH", "HOME", "TMPDIR", "GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE",
	"GOFLAGS", "GOTOOLCHAIN", "GOPROXY", "XDG_CACHE_HOME",
}

// Env returns the environment of git and go in these tests: names, from this
// process, and a git configuration of no user's and no system's, with an
// author and a committer.
func Env() []string {
	out := []string{
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
	for _, name := range names {
		if v, err := (bedrock.OSEnvironment{}).Value(name); err == nil {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// New returns the directory of a new git repository on branch main, whose
// remote origin is the bare repository at the same path with .git appended,
// and which holds nothing yet.
func New(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	Git(t, "", "init", "-q", "--bare", dir+".git")
	Git(t, "", "init", "-q", "-b", "main", dir)
	Git(t, dir, "remote", "add", "origin", dir+".git")
	return dir
}

// Write writes contents to the file name, a slash-separated path under dir,
// creating its directories.
func Write(t *testing.T, dir, name, contents string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(contents), 0o644))
}

// Append appends contents to the file name, a slash-separated path under
// dir.
func Append(t *testing.T, dir, name, contents string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, filepath.FromSlash(name)), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(contents)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// Read returns the contents of the file name, a slash-separated path under
// dir.
func Read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	require.NoError(t, err)
	return string(b)
}

// Commit commits every change in the repository dir with message, and
// pushes main to origin.
func Commit(t *testing.T, dir, message string) {
	t.Helper()
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-q", "--allow-empty", "-m", message)
	Git(t, dir, "push", "-q", "origin", "main")
}

// Git runs git with args in dir, or in the test's working directory when dir
// is "", with Env, and returns what it printed on stdout, trimmed.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = Env()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "git %v: %s", args, stderr.String())
	return strings.TrimSpace(string(out))
}
