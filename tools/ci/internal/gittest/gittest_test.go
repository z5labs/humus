// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package gittest_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/ci/internal/gittest"
)

func TestNew_commits_and_pushes_to_its_bare_remote(t *testing.T) {
	repo := gittest.New(t)
	require.DirExists(t, filepath.Join(repo, ".git"))
	require.DirExists(t, repo+".git")
	require.Equal(t, repo+".git", gittest.Git(t, repo, "remote", "get-url", "origin"))

	gittest.Write(t, repo, "a/b/c.txt", "one\n")
	gittest.Append(t, repo, "a/b/c.txt", "two\n")
	require.Equal(t, "one\ntwo\n", gittest.Read(t, repo, "a/b/c.txt"))
	gittest.Commit(t, repo, "first")

	require.Equal(t, gittest.Git(t, repo, "rev-parse", "HEAD"), gittest.Git(t, repo+".git", "rev-parse", "main"))
	require.Equal(t, "test", gittest.Git(t, repo, "log", "-1", "--format=%an"))
	require.Equal(t, "a/b/c.txt", gittest.Git(t, repo, "ls-files"))
}

func TestEnv_has_no_user_git_configuration(t *testing.T) {
	env := gittest.Env()
	require.Contains(t, env, "GIT_CONFIG_GLOBAL=/dev/null")
	require.Contains(t, env, "GIT_CONFIG_NOSYSTEM=1")
	path, err := bedrock.OSEnvironment{}.Value("PATH")
	require.NoError(t, err)
	require.Contains(t, env, "PATH="+path)
}
