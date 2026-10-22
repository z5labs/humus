// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package dockertest puts stand-ins for the docker, kind and kubectl commands
// on PATH, for the tests that need no Docker.
package dockertest

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
)

// Fake puts copies of testdata/docker/docker, kind and kubectl first on PATH
// for the rest of the test, and returns their directory, which holds their
// state: see each script's comment.
func Fake(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := t.TempDir()
	for _, name := range []string{"docker", "kind", "kubectl"} {
		script, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "testdata", "docker", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), script, 0o755))
	}
	path, err := bedrock.OSEnvironment{}.Value("PATH")
	require.NoError(t, err)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+path)
	return dir
}

// Touch creates the file name in the fake's directory dir, with contents.
func Touch(t *testing.T, dir, name, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644))
}

// Read returns the contents of the file name in the fake's directory dir, or
// "" when there is none.
func Read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(b)
}

// Buffer is a bytes.Buffer safe for concurrent use.
type Buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
