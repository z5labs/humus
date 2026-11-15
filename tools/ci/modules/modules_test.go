// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package modules_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/tools/ci/internal/gittest"
	"github.com/z5labs/humus/tools/ci/modules"
)

func TestFind_finds_every_go_mod_the_Go_tool_does_not_ignore_in_order(t *testing.T) {
	dirs, ignored := modules.Find([]string{
		"README.md",
		"grpc/go.mod",
		"go.mod",
		"grpc/go.sum",
		"grpc/testdata/x/go.mod",
		"_tools/go.mod",
		"queue/.hidden/m/go.mod",
		"queue/kafka/go.mod",
		"notgo.mod",
		"a/notgo.mod",
		"testdata/go.mod",
		"",
	})
	require.Equal(t, []string{"grpc", ".", "queue/kafka"}, dirs)
	require.Equal(t, []modules.Ignored{
		{GoMod: "grpc/testdata/x/go.mod", Under: "testdata"},
		{GoMod: "_tools/go.mod", Under: "_tools"},
		{GoMod: "queue/.hidden/m/go.mod", Under: ".hidden"},
		{GoMod: "testdata/go.mod", Under: "testdata"},
	}, ignored)
}

func TestFind_finds_nothing_in_no_files(t *testing.T) {
	dirs, ignored := modules.Find(nil)
	require.Empty(t, dirs)
	require.Empty(t, ignored)
}

func TestGoMod(t *testing.T) {
	require.Equal(t, "go.mod", modules.GoMod("."))
	require.Equal(t, "queue/kafka/go.mod", modules.GoMod("queue/kafka"))
}

func module(t *testing.T, repo, dir, path string, goFile bool) {
	t.Helper()
	gittest.Write(t, repo, modules.GoMod(dir), "module "+path+"\n\ngo 1.27.0\n")
	if goFile {
		gittest.Write(t, repo, dir+"/p.go", "package p\n")
	}
}

func TestList_lists_every_tracked_module_with_a_package_sorted(t *testing.T) {
	repo := gittest.New(t)
	module(t, repo, ".", "example.com/repo", true)
	module(t, repo, "queue/kafka", "example.com/repo/queue/kafka", true)
	module(t, repo, "grpc", "example.com/repo/grpc", true)
	module(t, repo, "docs", "example.com/repo/docs", false)
	module(t, repo, "grpc/testdata/x", "example.com/x", true)
	module(t, repo, "_tools", "example.com/tools", true)
	module(t, repo, "tests", "example.com/repo/tests", false)
	gittest.Write(t, repo, "tests/p_test.go", "package p\n")
	gittest.Commit(t, repo, "modules")
	module(t, repo, "untracked", "example.com/repo/untracked", true)

	l, err := modules.List(t.Context(), filepath.Join(repo, "grpc"), gittest.Env())
	require.NoError(t, err)
	root, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	require.Equal(t, modules.Listing{
		Root:  root,
		Dirs:  []string{".", "grpc", "queue/kafka", "tests"},
		Empty: []string{"docs"},
	}, l, "a module with only test files has a package, and an untracked go.mod is no module")
}

func TestList_fails_on_a_broken_module(t *testing.T) {
	repo := gittest.New(t)
	module(t, repo, ".", "example.com/repo", true)
	gittest.Write(t, repo, "broken/go.mod", "module example.com/repo/broken\n\nnot a directive\n")
	gittest.Commit(t, repo, "broken")

	_, err := modules.List(t.Context(), repo, gittest.Env())
	require.ErrorContains(t, err, "go list failed in broken")
}

func TestList_fails_outside_a_git_repository(t *testing.T) {
	_, err := modules.List(t.Context(), t.TempDir(), gittest.Env())
	require.ErrorContains(t, err, "git rev-parse --show-toplevel")
}

func TestList_fails_when_git_cannot_list_the_files(t *testing.T) {
	repo := gittest.New(t)
	module(t, repo, ".", "example.com/repo", true)
	gittest.Commit(t, repo, "root")
	gittest.Write(t, repo, ".git/index", "not an index")

	_, err := modules.List(t.Context(), repo, gittest.Env())
	require.ErrorContains(t, err, "git ls-files")
}
