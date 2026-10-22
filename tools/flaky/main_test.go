// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus/tools/flaky/internal/repotest"
	"github.com/z5labs/humus/tools/flaky/results"
	"github.com/z5labs/humus/tools/flaky/run"
)

// flakyEnv is the environment flaky runs with in these tests: the variables
// the go command needs from the real one, and a TMPDIR of the test's own.
func flakyEnv(t *testing.T) map[string]string {
	vars := map[string]string{"TMPDIR": t.TempDir()}
	for _, name := range []string{"PATH", "HOME", "GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOTOOLCHAIN", "XDG_CACHE_HOME"} {
		if v, err := (bedrock.OSEnvironment{}).Value(name); err == nil {
			vars[name] = v
		}
	}
	return vars
}

// stressIn runs flaky with args in a copy of testdata/repo, and returns its
// exit status and what it wrote.
func stressIn(t *testing.T, repo string, args ...string) (bedrock.ExitStatus, string, string) {
	t.Helper()
	t.Chdir(filepath.Join(repo, "a"))
	p, status := bedrocktest.Start(t, bedrock.AppFunc(flaky), bedrocktest.WithArgs(args...), bedrocktest.WithEnv(flakyEnv(t)))
	st := <-status
	return st, p.Stdout(), p.Stderr()
}

func needCores(t *testing.T, n int) {
	cores, err := run.Affinity()
	require.NoError(t, err)
	if len(cores) < n {
		t.Skipf("this process may run on %d cores, and the test needs %d", len(cores), n)
	}
}

func TestStress_reports_a_test_that_failed_and_exits_1(t *testing.T) {
	needCores(t, 2)
	repo := repotest.New(t, "0.0.1")
	out := filepath.Join(t.TempDir(), "out")
	st, stdout, stderr := stressIn(t, repo, "stress", "-cpu", "1,2", "-count", "2", "-keep", "0", "-o", out)
	require.Equal(t, 1, st.Code(), "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	require.Contains(t, stderr, "but build.yaml sets 0.0.1", "it warns that the local Go isn't CI's")
	require.Contains(t, stderr, "found 3 packages with tests")
	require.Contains(t, stderr, "built 3 test binaries")

	require.Contains(t, stdout, "FAILING: failed or hung every run at some CPU count\n\ntwocpus  TestFailsOnTwoCPUs\n")
	require.Contains(t, stdout, "    cpu 2            2         0         2\n")
	require.Contains(t, stdout, "runtime.NumCPU() is 2")
	require.Contains(t, stdout, "reproduce: flaky stress -run '^TestFailsOnTwoCPUs$' -cpu 2 -count 2 ./twocpus\n")
	require.Contains(t, stdout, "taskset -c 0-1 go -C . test -race -cover -count=2 -cpu 2 -timeout 2m0s -run '^TestFailsOnTwoCPUs$' ./twocpus\n")
	require.Contains(t, stdout, "3 tests passed every run\n", "TestA, TestAToo and TestB")
	require.Contains(t, stdout, "phases: discovery ")
	require.NotContains(t, stdout, "FLAKY")

	saved, err := os.ReadDir(filepath.Join(out, "twocpus", "TestFailsOnTwoCPUs"))
	require.NoError(t, err)
	require.Len(t, saved, 2, "each failure's whole output is saved")
}

func TestStress_writes_the_report_as_JSON(t *testing.T) {
	needCores(t, 2)
	repo := repotest.New(t, "0.0.1")
	st, stdout, stderr := stressIn(t, repo, "stress", "-cpu", "2", "-count", "1", "-keep", "0", "-timeout", "1m", "-json", "./twocpus")
	require.Equal(t, 1, st.Code(), stderr)
	var rep results.Report
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep))
	require.Len(t, rep.Commit, 40)
	require.True(t, strings.HasPrefix(rep.Go, "go1."))
	require.Positive(t, rep.Cores)
	require.Equal(t, []int{2}, rep.CPUs)
	require.Len(t, rep.Failing, 1)
	require.Equal(t, "TestFailsOnTwoCPUs", rep.Failing[0].Test)
	require.Equal(t, "flaky stress -run '^TestFailsOnTwoCPUs$' -cpu 2 -count 1 -timeout 1m0s ./twocpus", rep.Failing[0].Reproduce)
	require.Len(t, rep.Phases, 3)
	require.Regexp(t, `/flaky-\d{8}T\d{6}$`, rep.Output, "the output goes under TMPDIR by default")
}

func TestStress_exits_0_when_every_test_passed_every_run(t *testing.T) {
	repo := repotest.New(t, "0.0.1")
	st, stdout, stderr := stressIn(t, repo, "stress", "-cpu", "1", "-count", "3", "-keep", "0", "-run", "^TestA$", "./a", "./b/...")
	require.Equal(t, 0, st.Code(), stderr)
	require.Equal(t, "1 test passed every run", strings.Split(stdout, "\n")[0], "-run narrows the tests that run")
}

func TestStress_exits_1_when_a_package_does_not_build(t *testing.T) {
	repo := repotest.New(t, "0.0.1")
	repotest.Write(t, repo, "a/broken_test.go", "package a\n\nfunc broken() int { return \"\" }\n")
	st, _, stderr := stressIn(t, repo, "stress", "-cpu", "1", "-count", "1", "-keep", "0", "./a")
	require.Equal(t, 1, st.Code())
	require.Contains(t, stderr, "building example.com/repo/a")
}

func TestStress_exits_2_when_it_cannot_run(t *testing.T) {
	repo := repotest.New(t, "0.0.1")
	testCases := []struct {
		name string
		args []string
		want string
	}{
		{"a flag it doesn't know", []string{"stress", "-nope"}, "flag provided but not defined"},
		{"a count below 1", []string{"stress", "-count", "0"}, "-count must be at least 1"},
		{"a timeout that isn't positive", []string{"stress", "-timeout", "0s"}, "-timeout must be positive"},
		{"a CPU count that isn't one", []string{"stress", "-cpu", "1,x"}, `"x" is not a CPU count`},
		{"no lane", []string{"stress", "-keep", "100000"}, "leave no lane"},
		{"a pattern that matches nothing", []string{"stress", "-keep", "0", "-cpu", "1", "./nothing"}, "./nothing matched no package with tests"},
		{"no command", nil, "usage: flaky stress"},
		{"a command it doesn't know", []string{"nope"}, `unknown command "nope"`},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			st, _, stderr := stressIn(t, repo, tc.args...)
			require.Equal(t, 2, st.Code())
			require.Contains(t, stderr, tc.want)
		})
	}
}

func TestStress_fails_outside_a_git_repository(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "a"), 0o755))
	st, _, stderr := stressIn(t, dir, "stress", "-keep", "0")
	require.Equal(t, 2, st.Code())
	require.Contains(t, stderr, "git rev-parse")
}

func TestFlaky_prints_its_usage_when_asked(t *testing.T) {
	repo := repotest.New(t, "0.0.1")
	st, stdout, _ := stressIn(t, repo, "help")
	require.Equal(t, 0, st.Code())
	require.Contains(t, stdout, "usage: flaky stress")

	st, _, stderr := stressIn(t, repo, "stress", "-h")
	require.Equal(t, 0, st.Code())
	require.Contains(t, stderr, "-keep int")
}
