// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus/tools/ci/internal/gittest"
	"github.com/z5labs/humus/tools/ci/release"
)

// runCI runs ci with args and returns its exit status and what it wrote.
func runCI(t *testing.T, args ...string) (bedrock.ExitStatus, string, string) {
	t.Helper()
	p, status := bedrocktest.Start(t, bedrock.AppFunc(ci), bedrocktest.WithArgs(args...))
	st := <-status
	return st, p.Stdout(), p.Stderr()
}

func TestCI_without_a_known_command_prints_the_usage_and_exits_2(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}} {
		st, stdout, stderr := runCI(t, args...)
		require.Equal(t, 2, st.Code(), args)
		require.Empty(t, stdout)
		require.Contains(t, stderr, usage)
	}
}

func TestCI_help_prints_the_usage(t *testing.T) {
	st, stdout, stderr := runCI(t, "help")
	require.Equal(t, 0, st.Code())
	require.Equal(t, usage, stdout)
	require.Empty(t, stderr)
}

// repo returns a new repository holding a root module and a nested one, each
// with a package, and the Hugo module under docs/, which has none, with the
// test's working directory in its grpc directory.
func repo(t *testing.T) string {
	t.Helper()
	dir := gittest.New(t)
	gittest.Write(t, dir, "go.mod", "module github.com/z5labs/humus\n\ngo 1.27.0\n")
	gittest.Write(t, dir, "p.go", "package p\n")
	gittest.Write(t, dir, "grpc/go.mod", "module github.com/z5labs/humus/grpc\n\ngo 1.27.0\n")
	gittest.Write(t, dir, "grpc/p.go", "package p\n")
	gittest.Write(t, dir, "docs/go.mod", "module github.com/z5labs/humus\n\ngo 1.27.0\n")
	gittest.Commit(t, dir, "modules")
	t.Chdir(filepath.Join(dir, "grpc"))
	return dir
}

func TestModules_prints_every_module_with_a_package(t *testing.T) {
	repo(t)
	st, stdout, stderr := runCI(t, "modules")
	require.Equal(t, 0, st.Code(), stderr)
	require.Equal(t, ".\ngrpc\n", stdout)
	require.Equal(t, "ci modules: docs holds no Go packages; skipping it\n", stderr)
}

func TestModules_fails_outside_a_git_repository(t *testing.T) {
	t.Chdir(t.TempDir())
	st, stdout, stderr := runCI(t, "modules")
	require.Equal(t, 1, st.Code())
	require.Empty(t, stdout)
	require.Contains(t, stderr, "ci modules: git rev-parse --show-toplevel")
}

func TestModules_takes_no_arguments(t *testing.T) {
	st, _, stderr := runCI(t, "modules", "extra")
	require.Equal(t, 2, st.Code())
	require.Contains(t, stderr, usage)
}

// gate runs ci coverage-gate 80 on packages and profile, written to files.
func gate(t *testing.T, packages, profile string) (bedrock.ExitStatus, string, string) {
	t.Helper()
	dir := t.TempDir()
	gittest.Write(t, dir, "packages.txt", packages)
	gittest.Write(t, dir, "coverage.out", profile)
	return runCI(t, "coverage-gate", "80", filepath.Join(dir, "packages.txt"), filepath.Join(dir, "coverage.out"))
}

func TestCoverageGate_exits_0_when_every_package_passes(t *testing.T) {
	st, stdout, stderr := gate(t, "example.com/m/a\n", "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 4 1\nexample.com/m/a/a.go:3.1,4.1 1 0\n")
	require.Equal(t, 0, st.Code(), stderr)
	require.Equal(t, "example.com/m/a: 80.0% of statements (4 of 5)\n", stdout)
}

func TestCoverageGate_exits_1_when_a_package_fails(t *testing.T) {
	st, stdout, _ := gate(t, "example.com/m/a\n", "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1999 1\nexample.com/m/a/a.go:3.1,4.1 501 0\n")
	require.Equal(t, 1, st.Code(), "79.96%% does not round up to 80%%")
	require.Contains(t, stdout, "::error title=Coverage below 80%::example.com/m/a")
}

func TestCoverageGate_an_empty_package_list_gates_nothing_and_says_nothing(t *testing.T) {
	st, stdout, stderr := gate(t, "", "mode: atomic\nexample.com/m/example/x/x.go:1.1,2.1 9 0\n")
	require.Equal(t, 0, st.Code())
	require.Empty(t, stdout)
	require.Empty(t, stderr)
}

func TestCoverageGate_exits_2_on_a_usage_error(t *testing.T) {
	dir := t.TempDir()
	packages := filepath.Join(dir, "packages.txt")
	profile := filepath.Join(dir, "coverage.out")
	gittest.Write(t, dir, "packages.txt", "example.com/m/a\n")
	gittest.Write(t, dir, "coverage.out", "mode: atomic\n")
	gittest.Write(t, dir, "malformed.out", "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1\n")
	for name, args := range map[string][]string{
		"a missing packages file is a usage error": {"80", filepath.Join(dir, "missing.txt"), profile},
		"a missing profile is a usage error":       {"80", packages, filepath.Join(dir, "missing.out")},
		"a non-numeric minimum is a usage error":   {"80%", packages, profile},
		"an empty minimum is a usage error":        {"", packages, profile},
		"a minimum above 100 is a usage error":     {"101", packages, profile},
		"a huge minimum is a usage error":          {strings.Repeat("9", 30), packages, profile},
		"too few arguments is a usage error":       {"80", packages},
		"a malformed profile is an error":          {"80", packages, filepath.Join(dir, "malformed.out")},
	} {
		t.Run(name, func(t *testing.T) {
			st, stdout, stderr := runCI(t, append([]string{"coverage-gate"}, args...)...)
			require.Equal(t, 2, st.Code())
			require.Empty(t, stdout)
			require.NotEmpty(t, stderr)
		})
	}
}

func TestCoverageGate_accepts_every_minimum_from_0_to_100(t *testing.T) {
	for _, s := range []string{"0", "80", "100", "0100"} {
		n, err := percent(s)
		require.NoError(t, err, s)
		require.LessOrEqual(t, n, 100)
	}
}

func TestRelease_exits_2_on_a_usage_error(t *testing.T) {
	for _, args := range []string{"", "--dry-run --list v0.16.0", "--bump --dry-run v0.16.0", "--bump --commit HEAD v0.16.0",
		"--bogus v0.16.0", "v0.16.0 v0.17.0", "--commit", "--remote"} {
		t.Run(args, func(t *testing.T) {
			st, stdout, stderr := runCI(t, append([]string{"release"}, strings.Fields(args)...)...)
			require.Equal(t, 2, st.Code())
			require.Empty(t, stdout)
			require.Contains(t, stderr, "usage: ci release")
		})
	}
}

func TestReleaseOptions_takes_its_arguments_in_any_order(t *testing.T) {
	o, err := releaseOptions([]string{"v0.16.0", "--remote", "upstream", "--dry-run", "--commit", "main"})
	require.NoError(t, err)
	require.Equal(t, release.Options{Mode: release.DryRun, Version: "v0.16.0", Commit: "main", Remote: "upstream"}, o)

	for flag, mode := range map[string]release.Mode{"--bump": release.Bump, "--list": release.List} {
		o, err := releaseOptions([]string{flag, "v0.16.0"})
		require.NoError(t, err)
		require.Equal(t, mode, o.Mode, flag)
	}
	o, err = releaseOptions([]string{"v0.16.0"})
	require.NoError(t, err)
	require.Equal(t, release.Tag, o.Mode)
}

func TestRelease_lists_the_modules_it_tags_from_a_subdirectory(t *testing.T) {
	repo(t)
	st, stdout, stderr := runCI(t, "release", "--list", "--commit", "HEAD", "v0.16.0")
	require.Equal(t, 0, st.Code(), stderr)
	require.Equal(t, "github.com/z5labs/humus\ngithub.com/z5labs/humus/grpc\n", stdout)
	require.Equal(t, "skip docs/go.mod: it holds no Go packages\n", stderr)
}

func TestRelease_prints_each_problem_on_a_line_of_its_own_and_exits_1(t *testing.T) {
	dir := repo(t)
	gittest.Write(t, dir, "a/go.mod", "module example.com/a\n")
	gittest.Write(t, dir, "a/p.go", "package p\n")
	gittest.Write(t, dir, "b/go.mod", "module example.com/b\n")
	gittest.Write(t, dir, "b/p.go", "package p\n")
	gittest.Commit(t, dir, "mismatched")

	st, _, stderr := runCI(t, "release", "--list", "v0.16.0")
	require.Equal(t, 1, st.Code())
	require.Contains(t, stderr, "ci release: a/go.mod declares module example.com/a, want github.com/z5labs/humus/a")
	require.Contains(t, stderr, "ci release: b/go.mod declares module example.com/b, want github.com/z5labs/humus/b")
}

func TestAffected_with_no_base_prints_every_module(t *testing.T) {
	repo(t)
	st, stdout, stderr := runCI(t, "affected")
	require.Equal(t, 0, st.Code(), stderr)
	require.Equal(t, ".\ngrpc\n", stdout)
	require.Equal(t, "ci affected: no base to compare HEAD with, so every module is affected\n", stderr)

	st, stdout, _ = runCI(t, "affected", "--base", "")
	require.Equal(t, 0, st.Code())
	require.Equal(t, ".\ngrpc\n", stdout)
}

func TestAffected_prints_the_modules_the_change_from_base_affects(t *testing.T) {
	dir := repo(t)
	gittest.Append(t, dir, "grpc/p.go", "\n")
	gittest.Write(t, dir, "docs/content.md", "docs\n")
	gittest.Commit(t, dir, "change")

	st, stdout, stderr := runCI(t, "affected", "--base", "HEAD^")
	require.Equal(t, 0, st.Code(), stderr)
	require.Equal(t, "grpc\n", stdout)
	require.Equal(t, "ci affected: grpc: grpc/p.go\n", stderr)

	st, stdout, stderr = runCI(t, "affected", "--base", "HEAD")
	require.Equal(t, 0, st.Code(), stderr)
	require.Empty(t, stdout, "no change affects nothing")
	require.Empty(t, stderr)
}

func TestAffected_fails_when_it_cannot_decide(t *testing.T) {
	repo(t)
	st, stdout, stderr := runCI(t, "affected", "--base", "no-such-revision")
	require.Equal(t, 1, st.Code())
	require.Empty(t, stdout)
	require.Contains(t, stderr, "ci affected: git diff")
}

func TestAffected_json_prints_what_each_job_runs_for(t *testing.T) {
	dir := repo(t)
	gittest.Write(t, dir, "grpc/p_test.go", "//go:build testcontainers\n\npackage p\n")
	gittest.Commit(t, dir, "tagged")
	gittest.Write(t, dir, ".golangci.yaml", "version: \"2\"\n")
	gittest.Append(t, dir, "grpc/p.go", "\n")
	gittest.Commit(t, dir, "change")

	st, stdout, stderr := runCI(t, "affected", "--json", "--base", "HEAD^")
	require.Equal(t, 0, st.Code(), stderr)
	require.JSONEq(t, `{
		"modules": ["grpc"],
		"lint": [".", "grpc"],
		"vet_testcontainers": ["grpc"],
		"vet_functional": []
	}`, stdout)
	require.Equal(t, "ci affected: grpc: grpc/p.go\nci affected: .: lint and unit tests only: .golangci.yaml\n", stderr)

	st, stdout, stderr = runCI(t, "affected", "--base", "HEAD", "--json")
	require.Equal(t, 0, st.Code(), stderr)
	require.JSONEq(t, `{"modules": [], "lint": [], "vet_testcontainers": [], "vet_functional": []}`, stdout,
		"no change runs nothing, and says so with empty arrays")
	require.Empty(t, stderr)

	st, stdout, _ = runCI(t, "affected", "--json")
	require.Equal(t, 0, st.Code())
	require.JSONEq(t, `{
		"modules": [".", "grpc"],
		"lint": [".", "grpc"],
		"vet_testcontainers": ["grpc"],
		"vet_functional": []
	}`, stdout, "with no base")
}

func TestAffected_takes_only_a_base_and_json(t *testing.T) {
	for _, args := range [][]string{
		{"--base"}, {"HEAD"}, {"--base", "HEAD", "extra"}, {"--head", "HEAD"},
		{"--base", "HEAD", "--base", "HEAD"}, {"--json", "--json"}, {"--json", "--base"}, {"--base", "--json"},
	} {
		st, stdout, stderr := runCI(t, append([]string{"affected"}, args...)...)
		require.Equal(t, 2, st.Code(), args)
		require.Empty(t, stdout, args)
		require.Contains(t, stderr, "usage: ci affected [--base <rev>] [--json]", args)
	}
}
