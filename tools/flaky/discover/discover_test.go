// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package discover_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/flaky/discover"
	"github.com/z5labs/humus/tools/flaky/internal/env"
	"github.com/z5labs/humus/tools/flaky/internal/repotest"
	"github.com/z5labs/humus/tools/flaky/run"
)

func dirs(pkgs []discover.Package) []string {
	out := make([]string, len(pkgs))
	for i, p := range pkgs {
		out[i] = p.Dir
	}
	return out
}

func TestModules_lists_what_go_modules_sh_lists(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	childEnv := env.Child(bedrock.OSEnvironment{})
	modules, err := discover.Modules(t.Context(), repo, childEnv)
	require.NoError(t, err)
	require.Equal(t, []string{".", "b"}, modules, "docs holds no Go packages")

	repotest.Write(t, repo, "c/go.mod", "module example.com/repo/c\n\ngo 1.27.0\n")
	repotest.Write(t, repo, "c/c_test.go", "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n")
	repotest.Commit(t, repo)
	modules, err = discover.Modules(t.Context(), repo, childEnv)
	require.NoError(t, err)
	require.Equal(t, []string{".", "b", "c"}, modules, "a module added is found, with no change to flaky")
}

func TestModules_fails_without_go_modules_sh(t *testing.T) {
	_, err := discover.Modules(t.Context(), t.TempDir(), env.Child(bedrock.OSEnvironment{}))
	require.ErrorContains(t, err, "go-modules.sh")
}

func TestPackages_finds_every_package_with_tests_in_every_module(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	childEnv := env.Child(bedrock.OSEnvironment{})
	pkgs, err := discover.Packages(t.Context(), repo, []string{".", "b"}, nil, childEnv)
	require.NoError(t, err)
	require.Equal(t, []discover.Package{
		{Module: ".", Dir: "a", ImportPath: "example.com/repo/a"},
		{Module: ".", Dir: "twocpus", ImportPath: "example.com/repo/twocpus"},
		{Module: "b", Dir: "b", ImportPath: "example.com/repo/b"},
	}, pkgs, "notests has no test files")

	repotest.Write(t, repo, "notests/notests_test.go", "package notests\n\nimport \"testing\"\n\nfunc TestNow(t *testing.T) {}\n")
	pkgs, err = discover.Packages(t.Context(), repo, []string{".", "b"}, nil, childEnv)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "notests", "twocpus", "b"}, dirs(pkgs), "a package given a test is found")
}

func TestPackages_matches_patterns_from_the_repository_s_root(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	repotest.Write(t, repo, "b/sub/sub_test.go", "package sub\n\nimport \"testing\"\n\nfunc TestSub(t *testing.T) {}\n")
	childEnv := env.Child(bedrock.OSEnvironment{})
	modules := []string{".", "b"}
	testCases := []struct {
		patterns []string
		want     []string
	}{
		{[]string{"./..."}, []string{"a", "twocpus", "b", "b/sub"}},
		{[]string{"."}, nil},
		{[]string{"./a"}, []string{"a"}},
		{[]string{"./b/..."}, []string{"b", "b/sub"}},
		{[]string{"./b/sub"}, []string{"b/sub"}},
		{[]string{"./b/sub/..."}, []string{"b/sub"}},
		{[]string{"./twocpus", "./b"}, []string{"twocpus", "b"}},
	}
	for _, tc := range testCases {
		pkgs, err := discover.Packages(t.Context(), repo, modules, tc.patterns, childEnv)
		if tc.want == nil {
			require.ErrorContains(t, err, "matched no package with tests", "%v", tc.patterns)
			continue
		}
		require.NoError(t, err, "%v", tc.patterns)
		require.Equal(t, tc.want, dirs(pkgs), "%v", tc.patterns)
	}
}

func TestPackages_refuses_a_pattern_not_from_the_repository_s_root(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	childEnv := env.Child(bedrock.OSEnvironment{})
	_, err := discover.Packages(t.Context(), repo, []string{"."}, []string{"a"}, childEnv)
	require.ErrorContains(t, err, "must start with ./")
	_, err = discover.Packages(t.Context(), repo, []string{"."}, []string{"./../x"}, childEnv)
	require.ErrorContains(t, err, "leaves the repository")
	_, err = discover.Packages(t.Context(), repo, []string{"missing"}, nil, childEnv)
	require.ErrorContains(t, err, "go list")
}

func TestPackages_tests_are_listed_by_each_binary(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	childEnv := env.Child(bedrock.OSEnvironment{})
	list := func() []string {
		pkgs, err := discover.Packages(t.Context(), repo, []string{"."}, []string{"./a"}, childEnv)
		require.NoError(t, err)
		require.Len(t, pkgs, 1)
		bin := filepath.Join(t.TempDir(), "a.test")
		require.NoError(t, run.Build(t.Context(), filepath.Join(repo, pkgs[0].Module), pkgs[0].ImportPath, bin, childEnv))
		tests, err := run.List(t.Context(), bin, filepath.Join(repo, pkgs[0].Dir), childEnv)
		require.NoError(t, err)
		return tests
	}
	require.Equal(t, []string{"TestA", "TestAToo"}, list())

	repotest.Write(t, repo, "a/more_test.go", "package a\n\nimport \"testing\"\n\nfunc TestMore(t *testing.T) {}\n")
	require.Equal(t, []string{"TestA", "TestAToo", "TestMore"}, list(), "a test added is found, with no change to flaky")
}

func TestRoot_Commit_and_GoVersion(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	childEnv := env.Child(bedrock.OSEnvironment{})
	root, err := discover.Root(t.Context(), filepath.Join(repo, "a"), childEnv)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	require.Equal(t, want, root)

	commit, err := discover.Commit(t.Context(), repo, childEnv)
	require.NoError(t, err)
	require.Len(t, commit, 40)

	version, err := discover.GoVersion(t.Context(), repo, childEnv)
	require.NoError(t, err)
	require.Regexp(t, `^go1\.`, version)

	notRepo := t.TempDir()
	_, err = discover.Root(t.Context(), notRepo, childEnv)
	require.Error(t, err)
	_, err = discover.Commit(t.Context(), notRepo, childEnv)
	require.Error(t, err)
	_, err = discover.GoVersion(t.Context(), filepath.Join(notRepo, "missing"), childEnv)
	require.Error(t, err)
}

func TestCheckGoVersion_warns_when_the_local_Go_is_not_CI_s(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	require.Empty(t, discover.CheckGoVersion(repo, "go1.27.2"))
	require.Equal(t, "warning: the local Go is go1.27.0, but build.yaml sets 1.27.2", discover.CheckGoVersion(repo, "go1.27.0"))
	require.Contains(t, discover.CheckGoVersion(t.TempDir(), "go1.27.2"), "warning: can't tell which Go CI uses")

	repotest.Write(t, repo, ".github/workflows/build.yaml", "jobs: {}\n")
	require.Contains(t, discover.CheckGoVersion(repo, "go1.27.2"), "sets no go-version")
}
