// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package run_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/flaky/internal/env"
	"github.com/z5labs/humus/tools/flaky/run"
)

// runs is the module of packages whose tests fail or hang on cue.
const runs = "../testdata/runs"

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
	test2json string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "flaky-run-test-")
	if err != nil {
		panic(err)
	}
	binDir = dir
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			panic(err)
		}
	}()
	m.Run()
}

// binary returns the test binary of the package name under testdata/runs,
// building every one of them the first time it's called.
func binary(t *testing.T, name string) (string, string, run.Spec) {
	t.Helper()
	childEnv := env.Child(bedrock.OSEnvironment{})
	buildOnce.Do(func() {
		mod, err := filepath.Abs(runs)
		if err != nil {
			buildErr = err
			return
		}
		for _, pkg := range []string{"passes", "twocpus", "fourprocs", "testmain", "lingers", "order"} {
			if buildErr = run.Build(context.Background(), mod, "example.com/runs/"+pkg, "", filepath.Join(binDir, pkg+".test"), childEnv); buildErr != nil {
				return
			}
		}
		test2json, buildErr = run.Test2JSON(context.Background(), childEnv)
	})
	require.NoError(t, buildErr)
	dir, err := filepath.Abs(filepath.Join(runs, name))
	require.NoError(t, err)
	bin := filepath.Join(binDir, name+".test")
	return bin, dir, run.Spec{
		Binary:     bin,
		Dir:        dir,
		ImportPath: "example.com/runs/" + name,
		Timeout:    time.Minute,
		Grace:      30 * time.Second,
		Env:        childEnv,
		Test2JSON:  test2json,
	}
}

// pin returns the first n cores this process may run on, skipping the test
// when there are fewer.
func pin(t *testing.T, n int) []int {
	t.Helper()
	cores, err := run.Affinity()
	require.NoError(t, err)
	if len(cores) < n {
		t.Skipf("this process may run on %d cores, and the test needs %d", len(cores), n)
	}
	return cores[:n]
}

func outcomes(res run.Result) map[string]run.Outcome {
	out := map[string]run.Outcome{}
	for _, t := range res.Tests {
		out[t.Name] = t.Outcome
	}
	return out
}

func TestExec_passes_every_test_of_a_package_that_passes(t *testing.T) {
	_, _, spec := binary(t, "passes")
	spec.Cores, spec.CPU = pin(t, 1), 1
	res := run.Exec(t.Context(), spec)
	require.False(t, res.Failed(), "%s", res.Output)
	require.Equal(t, map[string]run.Outcome{
		"TestPasses": run.Pass, "TestPassesToo": run.Pass, "TestPassesToo/case": run.Pass,
	}, outcomes(res))
	require.Equal(t, []string{"=== RUN   TestPasses", "    passes_test.go:11: passes", "--- PASS: TestPasses (0.00s)"}, res.Tests[0].Output)
	require.Positive(t, res.Took)
}

func TestExec_pins_a_run_to_its_cores_so_runtime_NumCPU_is_its_CPU_count(t *testing.T) {
	_, _, spec := binary(t, "twocpus")
	spec.Cores, spec.CPU = pin(t, 2), 2
	res := run.Exec(t.Context(), spec)
	require.Equal(t, map[string]run.Outcome{
		"TestPasses":              run.Pass,
		"TestFailsOnTwoCPUs":      run.Pass,
		"TestFailsOnTwoCPUs/case": run.Fail,
	}, outcomes(res), "only the subtest that failed counts the failure:\n%s", res.Output)
	require.Contains(t, strings.Join(res.Tests[2].Output, "\n"), "runtime.NumCPU() is 2")
	require.False(t, res.PackageFailed)

	spec.Cores, spec.CPU = pin(t, 1), 1
	res = run.Exec(t.Context(), spec)
	require.False(t, res.Failed(), "%s", res.Output)
}

func TestExec_counts_a_hang_for_each_test_still_running_at_the_timeout(t *testing.T) {
	_, _, spec := binary(t, "fourprocs")
	spec.Cores, spec.CPU, spec.Timeout = pin(t, 1), 4, 2*time.Second
	res := run.Exec(t.Context(), spec)
	require.True(t, res.TimedOut, "%s", res.Output)
	require.False(t, res.Killed)
	require.Equal(t, map[string]run.Outcome{"TestPasses": run.Pass, "TestBlocksAtFourProcs": run.Hang}, outcomes(res))
	require.False(t, res.PackageFailed, "a hang isn't a failure outside any test")

	spec.CPU = 2
	res = run.Exec(t.Context(), spec)
	require.False(t, res.Failed(), "%s", res.Output)
}

func TestExec_counts_a_failure_in_TestMain_against_the_package(t *testing.T) {
	_, _, spec := binary(t, "testmain")
	spec.Cores, spec.CPU = pin(t, 1), 1
	res := run.Exec(t.Context(), spec)
	require.True(t, res.PackageFailed)
	require.Empty(t, res.Tests)
	require.Contains(t, res.PackageOutput, "setup failed")
}

func TestExec_kills_a_run_still_going_after_its_timeout_and_grace(t *testing.T) {
	_, _, spec := binary(t, "lingers")
	spec.Cores, spec.CPU, spec.Timeout, spec.Grace = pin(t, 1), 1, time.Second, time.Second
	res := run.Exec(t.Context(), spec)
	require.True(t, res.Killed)
	require.True(t, res.PackageFailed, "the tests passed, so the run failed outside them")
	require.Equal(t, map[string]run.Outcome{"TestPasses": run.Pass}, outcomes(res))
}

func TestExec_reports_a_run_whose_context_was_done_as_cancelled(t *testing.T) {
	_, _, spec := binary(t, "fourprocs")
	spec.Cores, spec.CPU = pin(t, 1), 4
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	res := run.Exec(ctx, spec)
	require.True(t, res.Cancelled)
}

func TestExec_fails_outside_any_test_when_the_run_cannot_start(t *testing.T) {
	_, _, spec := binary(t, "passes")
	spec.Cores, spec.CPU = pin(t, 1), 1
	spec.Dir = filepath.Join(t.TempDir(), "missing")
	res := run.Exec(t.Context(), spec)
	require.True(t, res.PackageFailed)
	require.Contains(t, string(res.Output), "running taskset")

	_, _, spec = binary(t, "passes")
	spec.Cores, spec.CPU = pin(t, 1), 1
	spec.Test2JSON = filepath.Join(t.TempDir(), "missing")
	res = run.Exec(t.Context(), spec)
	require.True(t, res.PackageFailed)
	require.Contains(t, string(res.Output), "test2json")
}

func TestExec_shuffles_a_run_with_its_seed_and_the_seed_reproduces_its_failure(t *testing.T) {
	_, _, spec := binary(t, "order")
	spec.Cores, spec.CPU = pin(t, 1), 1
	res := run.Exec(t.Context(), spec)
	require.False(t, res.Failed(), "in their order, TestFailsAfterTestDirties runs first:\n%s", res.Output)
	require.Nil(t, res.Seed)

	three := int64(3)
	spec.Seed = &three
	res = run.Exec(t.Context(), spec)
	three = 4
	require.Equal(t, int64(3), *res.Seed, "the result keeps a copy of the run's seed, not the spec's")

	var failing, passing *int64
	for seed := int64(1); seed <= 64 && (failing == nil || passing == nil); seed++ {
		spec.Seed = &seed
		res := run.Exec(t.Context(), spec)
		require.Equal(t, &seed, res.Seed, "the run's result keeps its seed")
		require.Contains(t, string(res.Output), "-test.shuffle "+strconv.FormatInt(seed, 10))
		if res.Failed() {
			failing = &seed
		} else {
			passing = &seed
		}
	}
	require.NotNil(t, failing, "no seed from 1 to 64 ran TestDirties first")
	require.NotNil(t, passing, "every seed from 1 to 64 ran TestDirties first")

	for range 5 {
		spec.Seed = failing
		res := run.Exec(t.Context(), spec)
		require.Equal(t, run.Fail, outcomes(res)["TestFailsAfterTestDirties"], "seed %d reproduces its failure:\n%s", *failing, res.Output)
		require.Contains(t, strings.Join(res.Tests[1].Output, "\n"), "TestDirties ran before it")

		spec.Seed = passing
		res = run.Exec(t.Context(), spec)
		require.False(t, res.Failed(), "seed %d runs the tests in an order that passes:\n%s", *passing, res.Output)
	}
}

func TestSpec_Command_pins_the_binary_with_taskset(t *testing.T) {
	spec := run.Spec{Binary: "/b/p.test", Cores: []int{4, 5}, CPU: 2, Timeout: 2 * time.Minute, Run: "^TestX$", Skip: "^TestY$"}
	require.Equal(t, []string{
		"taskset", "-c", "4,5", "/b/p.test",
		"-test.count=1", "-test.cpu=2", "-test.timeout=2m0s", "-test.v=test2json",
		"-test.run=^TestX$", "-test.skip=^TestY$",
	}, spec.Command())

	seed := int64(-42)
	spec.Seed = &seed
	require.Equal(t, []string{
		"taskset", "-c", "4,5", "/b/p.test",
		"-test.count=1", "-test.cpu=2", "-test.timeout=2m0s", "-test.v=test2json", "-test.shuffle=-42",
		"-test.run=^TestX$", "-test.skip=^TestY$",
	}, spec.Command(), "a seed shuffles the run")
}

func TestList_lists_a_binary_s_top_level_tests(t *testing.T) {
	bin, dir, spec := binary(t, "passes")
	tests, err := run.List(t.Context(), bin, dir, spec.Env)
	require.NoError(t, err)
	require.Equal(t, []string{"TestPasses", "TestPassesToo"}, tests)

	bin, dir, spec = binary(t, "testmain")
	_, err = run.List(t.Context(), bin, dir, spec.Env)
	require.ErrorContains(t, err, "setup failed", "TestMain runs before the list")
}

func TestBuild_builds_with_the_tag_it_is_given(t *testing.T) {
	mod, err := filepath.Abs(runs)
	require.NoError(t, err)
	dir := filepath.Join(mod, "tagged")
	childEnv := env.Child(bedrock.OSEnvironment{})
	list := func(tag string) []string {
		bin := filepath.Join(t.TempDir(), "tagged.test")
		require.NoError(t, run.Build(t.Context(), mod, "example.com/runs/tagged", tag, bin, childEnv))
		tests, err := run.List(t.Context(), bin, dir, childEnv)
		require.NoError(t, err)
		return tests
	}
	require.Equal(t, []string{"TestUntagged"}, list(""))
	require.Equal(t, []string{"TestTagged", "TestUntagged"}, list("testcontainers"))
}

func TestBuild_reports_a_package_that_does_not_build(t *testing.T) {
	mod, err := filepath.Abs(runs)
	require.NoError(t, err)
	err = run.Build(t.Context(), mod, "example.com/runs/missing", "", filepath.Join(t.TempDir(), "x.test"), env.Child(bedrock.OSEnvironment{}))
	require.ErrorContains(t, err, "building example.com/runs/missing")
}

func TestTest2JSON_fails_without_a_go_command(t *testing.T) {
	_, err := run.Test2JSON(t.Context(), []string{"PATH=" + t.TempDir()})
	require.ErrorContains(t, err, "finding test2json")
}

func TestParse(t *testing.T) {
	ev := func(action, test, output string) run.Event {
		return run.Event{Action: action, Test: test, Output: output}
	}
	timeout := []run.Event{
		ev("run", "TestA", ""),
		ev("run", "TestA/sub", ""),
		ev("output", "TestA/sub", "panic: test timed out after 2s\n"),
		ev("output", "TestA/sub", "\trunning tests:\n"),
		ev("output", "TestA/sub", "\t\tTestA (2s)\n"),
		ev("output", "TestA/sub", "\t\tTestA/sub (2s)\n"),
		ev("output", "TestA/sub", "\n"),
	}
	testCases := []struct {
		name    string
		events  []run.Event
		exited0 bool
		killed  bool
		want    map[string]run.Outcome
		pkg     bool
	}{
		{
			name:    "every test passes or skips",
			events:  []run.Event{ev("run", "TestA", ""), ev("pass", "TestA", ""), ev("run", "TestB", ""), ev("skip", "TestB", "")},
			exited0: true,
			want:    map[string]run.Outcome{"TestA": run.Pass, "TestB": run.Skip},
		},
		{
			name: "a subtest fails, and its parent doesn't count it again",
			events: []run.Event{
				ev("run", "TestA", ""), ev("run", "TestA/sub", ""), ev("fail", "TestA/sub", ""), ev("fail", "TestA", ""),
				ev("output", "", "FAIL\n"),
			},
			want: map[string]run.Outcome{"TestA": run.Pass, "TestA/sub": run.Fail},
		},
		{
			name:   "the innermost test running at the timeout hangs",
			events: timeout,
			want:   map[string]run.Outcome{"TestA": run.Pass, "TestA/sub": run.Hang},
		},
		{
			name:   "a test running when the run was killed hangs",
			events: []run.Event{ev("run", "TestA", "")},
			killed: true,
			want:   map[string]run.Outcome{"TestA": run.Hang},
		},
		{
			name:   "a test the binary exited under fails",
			events: []run.Event{ev("run", "TestA", ""), ev("output", "TestA", "fatal error: concurrent map writes\n")},
			want:   map[string]run.Outcome{"TestA": run.Fail},
		},
		{
			name:   "the binary fails after every test passed",
			events: []run.Event{ev("run", "TestA", ""), ev("pass", "TestA", ""), ev("output", "", "goleak: found goroutines\n")},
			want:   map[string]run.Outcome{"TestA": run.Pass},
			pkg:    true,
		},
		{
			name:   "an event for a test that never ran is ignored",
			events: []run.Event{ev("pass", "TestA", ""), ev("start", "", "")},
			want:   map[string]run.Outcome{},
			pkg:    true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res := run.Parse(tc.events, tc.exited0, tc.killed)
			require.Equal(t, tc.want, outcomes(res))
			require.Equal(t, tc.pkg, res.PackageFailed)
			require.Equal(t, tc.killed, res.Killed)
		})
	}
}

func TestTimedOut(t *testing.T) {
	require.Nil(t, run.TimedOut("ok\n"))
	require.Equal(t, []string{"TestA", "TestA/b c"}, run.TimedOut(
		"x\npanic: test timed out after 2m0s\n\trunning tests:\n\t\tTestA (2m0s)\n\t\tTestA/b c (2m0s)\n\ngoroutine 1\n"))
	require.Equal(t, []string{}, run.TimedOut("panic: test timed out after 1s\nsomething else\n"))
	require.Equal(t, []string{"TestA"}, run.TimedOut("panic: test timed out after 1s\n\trunning tests:\n\t\tTestA (1s)"))
}

func TestDecode_refuses_output_that_is_not_test2json_s(t *testing.T) {
	_, err := run.Decode(strings.NewReader("{\"Action\":\"run\"}\nnot json"))
	require.ErrorContains(t, err, "reading test2json's output")
}

func TestOutcome_String(t *testing.T) {
	require.Equal(t, "pass skip fail hang outcome(9)", strings.Join([]string{
		run.Pass.String(), run.Skip.String(), run.Fail.String(), run.Hang.String(), run.Outcome(9).String(),
	}, " "))
}

func TestCoreList(t *testing.T) {
	require.Equal(t, "3", run.CoreList([]int{3}))
	require.Equal(t, "8,9,10,11", run.CoreList([]int{8, 9, 10, 11}))
}

func TestAffinity_returns_the_cores_this_process_may_run_on(t *testing.T) {
	cores, err := run.Affinity()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), "nproc")
	cmd.Env = env.Child(bedrock.OSEnvironment{})
	out, err := cmd.Output()
	require.NoError(t, err, "nproc, which counts the cores the same way")
	require.Equal(t, strings.TrimSpace(string(out)), strconv.Itoa(len(cores)))
}
