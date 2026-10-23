// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package discover_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/flaky/discover"
	"github.com/z5labs/humus/tools/flaky/internal/env"
	"github.com/z5labs/humus/tools/flaky/internal/repotest"
	"github.com/z5labs/humus/tools/flaky/run"
)

var unit = []string{discover.Unit}

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

func TestLevels_finds_the_unit_level_and_one_for_each_build_tag_the_tracked_test_files_use(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	childEnv := env.Child(bedrock.OSEnvironment{})
	levels, err := discover.Levels(t.Context(), repo, childEnv)
	require.NoError(t, err)
	require.Equal(t, unit, levels, "no test file in the repository has a build tag")

	repotest.Write(t, repo, "a/a_conformance_test.go", "// Copyright\n\n//go:build conformance\n\npackage a\n\nimport \"testing\"\n\nfunc TestAConformance(t *testing.T) {}\n")
	repotest.Commit(t, repo)
	levels, err = discover.Levels(t.Context(), repo, childEnv)
	require.NoError(t, err)
	require.Equal(t, []string{"unit", "conformance"}, levels, "a tag added is a level, with no change to flaky")

	repotest.Write(t, repo, "b/sub/sub_test.go", "/* a block comment */\n\n//go:build (loopback || testcontainers) && !functional\n\npackage sub\n\nimport \"testing\"\n\nfunc TestSub(t *testing.T) {}\n")
	repotest.Commit(t, repo)
	levels, err = discover.Levels(t.Context(), repo, childEnv)
	require.NoError(t, err)
	require.Equal(t, []string{"unit", "conformance", "functional", "loopback", "testcontainers"}, levels, "every tag in the line is a level, negated or not, in a module of its own too")

	repotest.Write(t, repo, "c/c_test.go", "//go:build untracked\n\npackage c\n")
	repotest.Write(t, repo, "notests/gen.go", "//go:build generate\n\npackage notests\n")
	repotest.Write(t, repo, "a/body_test.go", "package a\n\n//go:build body\n")
	repotest.Git(t, repo, "add", "notests/gen.go", "a/body_test.go")
	levels, err = discover.Levels(t.Context(), repo, childEnv)
	require.NoError(t, err)
	require.Equal(t, []string{"unit", "conformance", "functional", "loopback", "testcontainers"}, levels, "a test file git doesn't track, a file that isn't a test file, and a line after the package clause add no level")

	require.NoError(t, os.Remove(filepath.Join(repo, "a", "a_conformance_test.go")))
	levels, err = discover.Levels(t.Context(), repo, childEnv)
	require.NoError(t, err)
	require.Equal(t, []string{"unit", "functional", "loopback", "testcontainers"}, levels, "a tracked test file gone from the working tree has no tags")
}

func TestLevels_fails_on_a_tag_it_cannot_make_a_level_of(t *testing.T) {
	childEnv := env.Child(bedrock.OSEnvironment{})
	repo := repotest.New(t, "1.27.2")
	repotest.Write(t, repo, "a/unit_test.go", "//go:build unit\n\npackage a\n")
	repotest.Commit(t, repo)
	_, err := discover.Levels(t.Context(), repo, childEnv)
	require.ErrorContains(t, err, "a/unit_test.go uses the build tag unit, which is the name of the untagged level")

	repo = repotest.New(t, "1.27.2")
	repotest.Write(t, repo, "a/bad_test.go", "//go:build (conformance\n\npackage a\n")
	repotest.Commit(t, repo)
	_, err = discover.Levels(t.Context(), repo, childEnv)
	require.ErrorContains(t, err, "a/bad_test.go")

	_, err = discover.Levels(t.Context(), t.TempDir(), childEnv)
	require.ErrorContains(t, err, "git ls-files")
}

func TestPackages_finds_every_package_with_tests_in_every_module(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	childEnv := env.Child(bedrock.OSEnvironment{})
	pkgs, err := discover.Packages(t.Context(), repo, []string{".", "b"}, nil, unit, childEnv)
	require.NoError(t, err)
	require.Equal(t, []discover.Package{
		{Module: ".", Dir: "a", ImportPath: "example.com/repo/a", Level: "unit"},
		{Module: ".", Dir: "twocpus", ImportPath: "example.com/repo/twocpus", Level: "unit"},
		{Module: "b", Dir: "b", ImportPath: "example.com/repo/b", Level: "unit"},
	}, pkgs, "notests has no test files")

	repotest.Write(t, repo, "notests/notests_test.go", "package notests\n\nimport \"testing\"\n\nfunc TestNow(t *testing.T) {}\n")
	pkgs, err = discover.Packages(t.Context(), repo, []string{".", "b"}, nil, unit, childEnv)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "notests", "twocpus", "b"}, dirs(pkgs), "a package given a test is found")
}

func TestPackages_finds_a_package_at_a_tagged_level_when_its_tag_changes_its_test_files(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	repotest.Write(t, repo, "a/a_containers_test.go", "//go:build testcontainers\n\npackage a\n\nimport \"testing\"\n\nfunc TestAContainers(t *testing.T) {}\n")
	repotest.Write(t, repo, "c/c_test.go", "//go:build testcontainers\n\npackage c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n")
	childEnv := env.Child(bedrock.OSEnvironment{})
	levels := []string{discover.Unit, "testcontainers"}
	pkgs, err := discover.Packages(t.Context(), repo, []string{".", "b"}, nil, levels, childEnv)
	require.NoError(t, err)
	require.Equal(t, []discover.Package{
		{Module: ".", Dir: "a", ImportPath: "example.com/repo/a", Level: "unit"},
		{Module: ".", Dir: "twocpus", ImportPath: "example.com/repo/twocpus", Level: "unit"},
		{Module: ".", Dir: "a", ImportPath: "example.com/repo/a", Level: "testcontainers", Untagged: true},
		{Module: ".", Dir: "c", ImportPath: "example.com/repo/c", Level: "testcontainers"},
		{Module: "b", Dir: "b", ImportPath: "example.com/repo/b", Level: "unit"},
	}, pkgs, "twocpus and b have the same test files with the tag, and c has test files only with it")

	pkgs, err = discover.Packages(t.Context(), repo, []string{"."}, []string{"./c"}, levels, childEnv)
	require.NoError(t, err)
	require.Equal(t, []string{"c"}, dirs(pkgs), "a pattern matches at any level")
	_, err = discover.Packages(t.Context(), repo, []string{"."}, []string{"./c"}, unit, childEnv)
	require.ErrorContains(t, err, "./c matched no package with tests", "c has no tests at the unit level")
}

func TestTag_is_none_for_the_unit_level_and_the_level_s_name_for_any_other(t *testing.T) {
	require.Empty(t, discover.Tag(discover.Unit))
	require.Equal(t, "testcontainers", discover.Tag("testcontainers"))
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
		pkgs, err := discover.Packages(t.Context(), repo, modules, tc.patterns, unit, childEnv)
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
	_, err := discover.Packages(t.Context(), repo, []string{"."}, []string{"a"}, unit, childEnv)
	require.ErrorContains(t, err, "must start with ./")
	_, err = discover.Packages(t.Context(), repo, []string{"."}, []string{"./../x"}, unit, childEnv)
	require.ErrorContains(t, err, "leaves the repository")
	_, err = discover.Packages(t.Context(), repo, []string{"missing"}, nil, unit, childEnv)
	require.ErrorContains(t, err, "go list")
}

func TestPackages_tests_are_listed_by_each_binary(t *testing.T) {
	repo := repotest.New(t, "1.27.2")
	childEnv := env.Child(bedrock.OSEnvironment{})
	list := func() []string {
		pkgs, err := discover.Packages(t.Context(), repo, []string{"."}, []string{"./a"}, unit, childEnv)
		require.NoError(t, err)
		require.Len(t, pkgs, 1)
		bin := filepath.Join(t.TempDir(), "a.test")
		require.NoError(t, run.Build(t.Context(), filepath.Join(repo, pkgs[0].Module), pkgs[0].ImportPath, "", bin, childEnv))
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

func TestWalkthroughs_finds_each_module_with_a_Dockerfile_and_a_deploy_kind_up_sh(t *testing.T) {
	repo := t.TempDir()
	repotest.Write(t, repo, "example/grpc/wallet/Dockerfile", "FROM scratch\n")
	repotest.Write(t, repo, "example/grpc/wallet/deploy/kind/up.sh", "#!/bin/sh\n")
	repotest.Write(t, repo, "example/rabbitmq/mailer/Dockerfile", "FROM scratch\n")
	repotest.Write(t, repo, "example/job/billing/deploy/kind/up.sh", "#!/bin/sh\n")
	repotest.Write(t, repo, "example/kafka/orders/Dockerfile/x", "a directory isn't a Dockerfile\n")
	repotest.Write(t, repo, "example/kafka/orders/deploy/kind/up.sh", "#!/bin/sh\n")
	modules := []string{".", "example/job/billing", "example/rabbitmq/mailer", "example/kafka/orders", "example/grpc/wallet"}

	found, err := discover.Walkthroughs(repo, modules)
	require.NoError(t, err)
	require.Equal(t, []discover.Walkthrough{{Module: "example/grpc/wallet", Name: "wallet"}}, found, "billing has no Dockerfile, mailer no up.sh, and orders' Dockerfile is a directory")
	require.Equal(t, filepath.Join(repo, "example", "grpc", "wallet", "Dockerfile"), found[0].Dockerfile(repo))
	require.Equal(t, filepath.Join(repo, "example", "grpc", "wallet", "deploy", "kind", "up.sh"), found[0].Up(repo))

	repotest.Write(t, repo, "example/rabbitmq/mailer/deploy/kind/up.sh", "#!/bin/sh\n")
	found, err = discover.Walkthroughs(repo, modules)
	require.NoError(t, err)
	require.Equal(t, []discover.Walkthrough{{Module: "example/rabbitmq/mailer", Name: "mailer"}, {Module: "example/grpc/wallet", Name: "wallet"}}, found, "a walkthrough added is found, with no change to flaky")

	repotest.Write(t, repo, "other/wallet/Dockerfile", "FROM scratch\n")
	repotest.Write(t, repo, "other/wallet/deploy/kind/up.sh", "#!/bin/sh\n")
	_, err = discover.Walkthroughs(repo, append(modules, "other/wallet"))
	require.ErrorContains(t, err, "the walkthroughs example/grpc/wallet and other/wallet share the name wallet")
}
