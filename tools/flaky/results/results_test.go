// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package results_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/tools/flaky/discover"
	"github.com/z5labs/humus/tools/flaky/results"
	"github.com/z5labs/humus/tools/flaky/run"
)

var pkgs = []discover.Package{
	{Module: "grpc", Dir: "grpc/grpchealth", ImportPath: "github.com/z5labs/humus/grpc/grpchealth", Level: "unit"},
	{Module: ".", Dir: "health", ImportPath: "github.com/z5labs/humus/health", Level: "unit"},
}

func newResults(t *testing.T) (*results.Results, string) {
	out := t.TempDir()
	return results.New(results.Config{
		Packages: pkgs,
		CPUs:     []int{1, 2, 4},
		Loads:    []int{1, 2},
		Out:      out,
		Levels:   []results.Level{{Name: "unit", Count: 3, Timeout: 2 * time.Minute, DefaultTimeout: 2 * time.Minute}},
	}), out
}

func passed(names ...string) run.Result {
	var res run.Result
	for _, n := range names {
		res.Tests = append(res.Tests, run.Test{Name: n, Outcome: run.Pass})
	}
	res.Output = []byte("PASS\n")
	return res
}

// sweep adds three runs of each package at each pair of CPU count and load.
// TestX/a_case fails once at cpu 2 load 2 and hangs once at cpu 4 load 1;
// TestY fails every run at cpu 1, at either load; health fails outside any
// test once at cpu 4 load 2.
func sweep(t *testing.T, r *results.Results) {
	r.Listed(0, []string{"TestX", "TestY", "TestZ", "TestNeverRan"})
	r.Listed(1, []string{"TestH"})
	for _, cpu := range []int{1, 2, 4} {
		for _, load := range []int{1, 2} {
			for i := range 3 {
				res := passed("TestX", "TestX/a_case", "TestY", "TestZ")
				if cpu == 2 && load == 2 && i == 1 {
					res.Tests[1] = run.Test{Name: "TestX/a_case", Outcome: run.Fail, Output: []string{"    x_test.go:12: boom"}}
					res.Output = []byte("--- FAIL: TestX/a_case\n    x_test.go:12: boom\n")
				}
				if cpu == 4 && load == 1 && i == 2 {
					res.Tests[1] = run.Test{Name: "TestX/a_case", Outcome: run.Hang, Output: []string{"=== RUN   TestX/a_case"}}
					res.TimedOut = true
				}
				if cpu == 1 {
					res.Tests[2] = run.Test{Name: "TestY", Outcome: run.Fail, Output: []string{"    y_test.go:3: always"}}
				}
				require.NoError(t, r.Add(0, cpu, load, res))

				h := passed("TestH")
				if cpu == 4 && load == 2 && i == 0 {
					h = run.Result{
						Tests:         []run.Test{{Name: "TestH", Outcome: run.Pass}},
						PackageFailed: true,
						PackageOutput: []string{"goleak: found unexpected goroutines"},
						Output:        []byte("goleak: found unexpected goroutines\n"),
					}
				}
				require.NoError(t, r.Add(1, cpu, load, h))
			}
		}
	}
}

func TestReport_counts_each_test_s_failures_and_hangs_at_each_pair_of_CPU_count_and_load(t *testing.T) {
	r, out := newResults(t)
	sweep(t, r)
	rep := r.Report()
	require.True(t, rep.Failed())

	require.Len(t, rep.Flaky, 2)
	x := rep.Flaky[0]
	require.Equal(t, "grpc/grpchealth", x.Package)
	require.Equal(t, "TestX/a_case", x.Test)
	require.Equal(t, []results.Pair{
		{CPU: 1, Load: 1, Runs: 3},
		{CPU: 1, Load: 2, Runs: 3},
		{CPU: 2, Load: 1, Runs: 3},
		{CPU: 2, Load: 2, Runs: 3, Failures: 1},
		{CPU: 4, Load: 1, Runs: 3, Hangs: 1},
		{CPU: 4, Load: 2, Runs: 3},
	}, x.Pairs)
	require.Equal(t, 18, x.Runs)
	require.Equal(t, 1, x.Failures)
	require.Equal(t, 1, x.Hangs)
	require.Equal(t, []string{"    x_test.go:12: boom"}, x.FirstFailure)
	require.Equal(t, 2, x.FirstFailureCPU)
	require.Equal(t, 2, x.FirstFailureLoad)
	require.Equal(t, filepath.Join(out, "grpc", "grpchealth", "TestX", "a_case"), x.Output)
	require.Equal(t, "flaky stress -tags unit -run '^TestX$/^a_case$' -cpu 2 -load 2 -count 3 ./grpc/grpchealth", x.Reproduce)
	require.Equal(t, "taskset -c 0-1 go -C grpc test -race -cover -count=3 -cpu 2 -timeout 2m0s -run '^TestX$/^a_case$' ./grpchealth", x.ReproduceGoTest)

	h := rep.Flaky[1]
	require.Equal(t, "health", h.Package)
	require.Empty(t, h.Test, "a failure outside any test counts against its package")
	require.Equal(t, filepath.Join(out, "health", "outside-tests"), h.Output)
	require.Equal(t, "flaky stress -tags unit -cpu 4 -load 2 -count 3 ./health", h.Reproduce)
	require.Equal(t, "taskset -c 0-3 go -C . test -race -cover -count=3 -cpu 4 -timeout 2m0s ./health", h.ReproduceGoTest)
	require.Equal(t, []string{"goleak: found unexpected goroutines"}, h.FirstFailure)

	require.Len(t, rep.Failing, 1, "a test that failed every run at some pair is listed apart")
	y := rep.Failing[0]
	require.Equal(t, "TestY", y.Test)
	require.Equal(t, 6, y.Failures)
	require.Equal(t, "flaky stress -tags unit -run '^TestY$' -cpu 1 -load 1 -count 3 ./grpc/grpchealth", y.Reproduce)
	require.Equal(t, "taskset -c 0 go -C grpc test -race -cover -count=3 -cpu 1 -timeout 2m0s -run '^TestY$' ./grpchealth", y.ReproduceGoTest)

	// TestX failed through its subtest, TestY failed, and TestNeverRan
	// never ran; TestZ and TestH passed every run.
	require.Equal(t, 2, rep.Passed)
}

func TestReport_reproduces_a_test_at_the_pair_where_it_failed_most(t *testing.T) {
	r := results.New(results.Config{
		Packages: pkgs, CPUs: []int{1, 2}, Loads: []int{1, 2}, Out: t.TempDir(),
		Levels: []results.Level{{Name: "unit", Count: 4, Timeout: 2 * time.Minute, DefaultTimeout: 2 * time.Minute}},
	})
	fail := run.Result{Tests: []run.Test{{Name: "TestH", Outcome: run.Fail, Output: []string{"boom"}}}}
	for _, cpu := range []int{1, 2} {
		for _, load := range []int{1, 2} {
			for i := range 4 {
				res := passed("TestH")
				if (cpu == 1 && load == 2 && i == 0) || (cpu == 2 && load == 1 && i < 2) {
					res = fail
				}
				require.NoError(t, r.Add(1, cpu, load, res))
			}
		}
	}
	rep := r.Report()
	require.Len(t, rep.Flaky, 1)
	h := rep.Flaky[0]
	require.Equal(t, 1, h.FirstFailureCPU)
	require.Equal(t, 2, h.FirstFailureLoad)
	require.Equal(t, "flaky stress -tags unit -run '^TestH$' -cpu 2 -load 1 -count 4 ./health", h.Reproduce, "2 of 4 runs failed at cpu 2 load 1")
	require.Equal(t, "taskset -c 0-1 go -C . test -race -cover -count=4 -cpu 2 -timeout 2m0s -run '^TestH$' ./health", h.ReproduceGoTest)
}

func TestReport_lists_a_test_apart_when_it_failed_every_run_at_one_load(t *testing.T) {
	r := results.New(results.Config{Packages: pkgs, CPUs: []int{4}, Loads: []int{1, 2}, Out: t.TempDir(), Levels: []results.Level{{Name: "unit", Count: 2}}})
	fail := run.Result{Tests: []run.Test{{Name: "TestH", Outcome: run.Fail}}}
	require.NoError(t, r.Add(1, 4, 1, passed("TestH")))
	require.NoError(t, r.Add(1, 4, 1, passed("TestH")))
	require.NoError(t, r.Add(1, 4, 2, fail))
	require.NoError(t, r.Add(1, 4, 2, fail))
	rep := r.Report()
	require.Empty(t, rep.Flaky)
	require.Len(t, rep.Failing, 1)
	require.Equal(t, []results.Pair{{CPU: 4, Load: 1, Runs: 2}, {CPU: 4, Load: 2, Runs: 2, Failures: 2}}, rep.Failing[0].Pairs)
}

func TestAdd_saves_each_failure_s_whole_output(t *testing.T) {
	r, out := newResults(t)
	sweep(t, r)
	saved, err := os.ReadFile(filepath.Join(out, "grpc", "grpchealth", "TestX", "a_case", "cpu2-load2-1.txt"))
	require.NoError(t, err)
	require.Equal(t, "--- FAIL: TestX/a_case\n    x_test.go:12: boom\n", string(saved))
	entries, err := os.ReadDir(filepath.Join(out, "grpc", "grpchealth", "TestY"))
	require.NoError(t, err)
	require.Len(t, entries, 6)
	_, err = os.Stat(filepath.Join(out, "grpc", "grpchealth", "TestX", "a_case", "cpu4-load1-2.txt"))
	require.NoError(t, err)
}

func TestAdd_fails_when_the_output_cannot_be_saved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	r := results.New(results.Config{Packages: pkgs, CPUs: []int{1}, Loads: []int{1}, Out: file, Levels: []results.Level{{Name: "unit", Count: 1}}})
	err := r.Add(0, 1, 1, run.Result{Tests: []run.Test{{Name: "TestX", Outcome: run.Fail}}})
	require.Error(t, err)
	err = r.Add(1, 1, 1, run.Result{PackageFailed: true})
	require.Error(t, err)
}

func TestAdd_keeps_a_hang_s_or_a_kill_s_cause_in_its_first_failure(t *testing.T) {
	r, _ := newResults(t)
	require.NoError(t, r.Add(0, 1, 1, run.Result{Tests: []run.Test{{Name: "TestA", Outcome: run.Hang}}, Killed: true}))
	require.NoError(t, r.Add(1, 1, 1, run.Result{PackageFailed: true, Killed: true}))
	long := make([]string, 80)
	for i := range long {
		long[i] = "line"
	}
	require.NoError(t, r.Add(0, 2, 1, run.Result{Tests: []run.Test{{Name: "TestB/c d", Outcome: run.Fail, Output: long}}}))
	rep := r.Report()
	require.Len(t, rep.Failing, 3, "each failed its one run")
	require.Equal(t, []string{"killed: still running after its timeout"}, rep.Failing[0].FirstFailure)
	require.Len(t, rep.Failing[1].FirstFailure, 51, "a first failure is cut at 50 lines")
	require.Equal(t, "…", rep.Failing[1].FirstFailure[50])
	require.Equal(t, filepath.Join("grpc", "grpchealth", "TestB", "c_d"), rel(t, rep.Output, rep.Failing[1].Output))
	require.Equal(t, []string{"killed: still running after its timeout"}, rep.Failing[2].FirstFailure)
}

func rel(t *testing.T, base, path string) string {
	r, err := filepath.Rel(base, path)
	require.NoError(t, err)
	return r
}

func TestReport_names_a_non_default_timeout_in_the_flaky_command(t *testing.T) {
	r := results.New(results.Config{
		Packages: pkgs, CPUs: []int{1}, Loads: []int{1}, Out: t.TempDir(),
		Levels: []results.Level{{Name: "unit", Count: 5, Timeout: 10 * time.Second, DefaultTimeout: 2 * time.Minute}},
	})
	require.NoError(t, r.Add(1, 1, 1, run.Result{Tests: []run.Test{{Name: "TestH", Outcome: run.Fail}, {Name: "TestH2", Outcome: run.Pass}}}))
	require.NoError(t, r.Add(1, 1, 1, passed("TestH", "TestH2")))
	rep := r.Report()
	require.Equal(t, "flaky stress -tags unit -run '^TestH$' -cpu 1 -load 1 -count 5 -timeout 10s ./health", rep.Flaky[0].Reproduce)
}

func TestReport_reproduces_a_test_at_its_level_with_the_level_s_tag_count_and_timeout(t *testing.T) {
	tagged := []discover.Package{{Module: "kube", Dir: "kube", ImportPath: "github.com/z5labs/humus/kube", Level: "testcontainers"}}
	r := results.New(results.Config{
		Packages: tagged, CPUs: []int{1, 2}, Loads: []int{1}, Out: t.TempDir(),
		Levels: []results.Level{
			{Name: "unit", Count: 500, Timeout: 2 * time.Minute, DefaultTimeout: 2 * time.Minute},
			{Name: "testcontainers", Count: 25, Timeout: 10 * time.Minute, DefaultTimeout: 10 * time.Minute},
		},
	})
	require.NoError(t, r.Add(0, 2, 1, run.Result{Tests: []run.Test{{Name: "TestK", Outcome: run.Fail, Output: []string{"boom"}}}}))
	require.NoError(t, r.Add(0, 2, 1, passed("TestK")))
	rep := r.Report()
	require.Len(t, rep.Flaky, 1)
	k := rep.Flaky[0]
	require.Equal(t, "testcontainers", k.Level)
	require.Equal(t, "flaky stress -tags testcontainers -run '^TestK$' -cpu 2 -load 1 -count 25 ./kube", k.Reproduce, "the level's default timeout is left out")
	require.Equal(t, "taskset -c 0-1 go -C kube test -race -cover -tags testcontainers -count=25 -cpu 2 -timeout 10m0s -run '^TestK$' .", k.ReproduceGoTest)

	var buf bytes.Buffer
	require.NoError(t, results.WriteText(&buf, rep))
	require.Contains(t, buf.String(), "\nkube  TestK  (testcontainers)\n", "a test at a tagged level names its level")
}

func TestRunPattern(t *testing.T) {
	require.Empty(t, results.RunPattern(""))
	require.Equal(t, "^TestX$", results.RunPattern("TestX"))
	require.Equal(t, `^TestX$/^a\.b\(c\)$`, results.RunPattern("TestX/a.b(c)"))
}

func TestWriteText(t *testing.T) {
	r, out := newResults(t)
	sweep(t, r)
	rep := r.Report()
	rep.Phases = []results.Phase{results.NewPhase("discovery", time.Second), results.NewPhase("build", time.Minute)}
	var buf bytes.Buffer
	require.NoError(t, results.WriteText(&buf, rep))
	want := `FLAKY: failed or hung in some runs

grpc/grpchealth  TestX/a_case
                    failures     hangs      runs
    cpu 1 load 1           0         0         3
    cpu 1 load 2           0         0         3
    cpu 2 load 1           0         0         3
    cpu 2 load 2           1         0         3
    cpu 4 load 1           0         1         3
    cpu 4 load 2           0         0         3
    1 failure, 1 hang in 18 runs
    first failure, at cpu 2 load 2:
            x_test.go:12: boom
    output:    ` + filepath.Join(out, "grpc/grpchealth/TestX/a_case") + `
    reproduce: flaky stress -tags unit -run '^TestX$/^a_case$' -cpu 2 -load 2 -count 3 ./grpc/grpchealth
               taskset -c 0-1 go -C grpc test -race -cover -count=3 -cpu 2 -timeout 2m0s -run '^TestX$/^a_case$' ./grpchealth

health  (outside any test)
                    failures     hangs      runs
    cpu 1 load 1           0         0         3
    cpu 1 load 2           0         0         3
    cpu 2 load 1           0         0         3
    cpu 2 load 2           0         0         3
    cpu 4 load 1           0         0         3
    cpu 4 load 2           1         0         3
    1 failure, 0 hangs in 18 runs
    first failure, at cpu 4 load 2:
        goleak: found unexpected goroutines
    output:    ` + filepath.Join(out, "health/outside-tests") + `
    reproduce: flaky stress -tags unit -cpu 4 -load 2 -count 3 ./health
               taskset -c 0-3 go -C . test -race -cover -count=3 -cpu 4 -timeout 2m0s ./health

FAILING: failed or hung every run at some pair of CPU count and load

grpc/grpchealth  TestY
                    failures     hangs      runs
    cpu 1 load 1           3         0         3
    cpu 1 load 2           3         0         3
    cpu 2 load 1           0         0         3
    cpu 2 load 2           0         0         3
    cpu 4 load 1           0         0         3
    cpu 4 load 2           0         0         3
    6 failures, 0 hangs in 18 runs
    first failure, at cpu 1 load 1:
            y_test.go:3: always
    output:    ` + filepath.Join(out, "grpc/grpchealth/TestY") + `
    reproduce: flaky stress -tags unit -run '^TestY$' -cpu 1 -load 1 -count 3 ./grpc/grpchealth
               taskset -c 0 go -C grpc test -race -cover -count=3 -cpu 1 -timeout 2m0s -run '^TestY$' ./grpchealth

2 tests passed every run
phases: discovery 1s, build 1m0s
`
	require.Equal(t, want, buf.String())
}

func TestWriteText_of_a_sweep_that_passed(t *testing.T) {
	r, _ := newResults(t)
	r.Listed(1, []string{"TestH"})
	require.NoError(t, r.Add(1, 1, 1, passed("TestH")))
	var buf bytes.Buffer
	require.NoError(t, results.WriteText(&buf, r.Report()))
	require.Equal(t, "1 test passed every run\nphases: \n", buf.String())
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken") }

func TestWriteText_reports_a_failed_write(t *testing.T) {
	r, _ := newResults(t)
	sweep(t, r)
	require.ErrorContains(t, results.WriteText(failingWriter{}, r.Report()), "broken")
}

func TestWriteJSON_writes_the_same_report(t *testing.T) {
	r, _ := newResults(t)
	sweep(t, r)
	rep := r.Report()
	rep.Commit, rep.Go, rep.Cores = "abc123", "go1.27.2", 32
	rep.Phases = []results.Phase{results.NewPhase("sweep", 90*time.Second)}
	var buf bytes.Buffer
	require.NoError(t, results.WriteJSON(&buf, rep))

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Equal(t, "abc123", got["commit"])
	require.Equal(t, "go1.27.2", got["go"])
	require.Equal(t, []any{1.0, 2.0}, got["loads"])
	require.InDelta(t, 32, got["cores"], 0)
	require.InDelta(t, 2, got["passed"], 0)
	require.Equal(t, []any{map[string]any{"name": "sweep", "seconds": 90.0}}, got["phases"])
	flaky := got["flaky"].([]any)
	require.Len(t, flaky, 2)
	x := flaky[0].(map[string]any)
	require.Equal(t, "TestX/a_case", x["test"])
	require.InDelta(t, 2, x["firstFailureLoad"], 0)
	pairs := x["pairs"].([]any)
	require.Len(t, pairs, 6, "every pair of CPU count and load")
	require.Equal(t, map[string]any{"cpu": 2.0, "load": 2.0, "runs": 3.0, "failures": 1.0, "hangs": 0.0}, pairs[3])
	require.Len(t, got["failing"], 1)
}

func newConfirmResults(t *testing.T) (*results.Results, string) {
	out := t.TempDir()
	return results.New(results.Config{
		Packages: pkgs,
		CPUs:     []int{1, 2},
		Loads:    []int{1, 2},
		Out:      out,
		Levels:   []results.Level{{Name: "unit", Count: 3, Confirm: 4, Timeout: 2 * time.Minute, DefaultTimeout: 2 * time.Minute}},
	}), out
}

// confirmSweep adds a sweep in which TestX/a_case fails once at cpu 1 load 1,
// and TestH passes every run, then four confirm runs of TestX/a_case at each
// pair. Its confirm runs fail twice at cpu 2 load 2 and hang once at cpu 2
// load 1.
func confirmSweep(t *testing.T, r *results.Results) {
	r.Listed(0, []string{"TestX"})
	r.Listed(1, []string{"TestH"})
	for _, cpu := range []int{1, 2} {
		for _, load := range []int{1, 2} {
			for i := range 3 {
				res := passed("TestX", "TestX/a_case")
				if cpu == 1 && load == 1 && i == 0 {
					res.Tests[1] = run.Test{Name: "TestX/a_case", Outcome: run.Fail, Output: []string{"    x_test.go:12: boom"}}
				}
				require.NoError(t, r.Add(0, cpu, load, res))
				require.NoError(t, r.Add(1, cpu, load, passed("TestH")))
			}
		}
	}
	for _, cpu := range []int{1, 2} {
		for _, load := range []int{1, 2} {
			for i := range 4 {
				res := passed("TestX", "TestX/a_case")
				if cpu == 2 && load == 2 && i < 2 {
					res.Tests[1] = run.Test{Name: "TestX/a_case", Outcome: run.Fail}
					res.Output = []byte("--- FAIL: TestX/a_case\n")
				}
				if cpu == 2 && load == 1 && i == 3 {
					res.Tests[1] = run.Test{Name: "TestX/a_case", Outcome: run.Hang}
					res.TimedOut = true
				}
				require.NoError(t, r.Confirmed(0, "TestX/a_case", cpu, load, res))
			}
		}
	}
}

func TestReport_gives_a_confirmed_test_s_confirm_runs(t *testing.T) {
	r, out := newConfirmResults(t)
	confirmSweep(t, r)
	rep := r.Report()
	require.Equal(t, 4, rep.Levels[0].Confirm)
	require.Equal(t, 1, rep.Confirmed)
	require.Equal(t, 16, rep.ConfirmRuns)
	require.Empty(t, rep.Failing)
	require.Len(t, rep.Flaky, 1)
	x := rep.Flaky[0]
	require.True(t, x.Confirmed)
	require.Equal(t, []results.Pair{
		{CPU: 1, Load: 1, Runs: 4},
		{CPU: 1, Load: 2, Runs: 4},
		{CPU: 2, Load: 1, Runs: 4, Hangs: 1},
		{CPU: 2, Load: 2, Runs: 4, Failures: 2},
	}, x.Pairs, "the table gives its confirm runs, not its sweep runs")
	require.Equal(t, 16, x.Runs)
	require.Equal(t, 2, x.Failures)
	require.Equal(t, 1, x.Hangs)
	require.Equal(t, []string{"    x_test.go:12: boom"}, x.FirstFailure, "the first failure is the sweep's")
	require.Equal(t, 1, x.FirstFailureCPU)
	require.Equal(t, "flaky stress -tags unit -run '^TestX$/^a_case$' -cpu 2 -load 2 -count 4 ./grpc/grpchealth", x.Reproduce, "the pair where its confirm runs failed most, at the confirm count")
	require.Equal(t, "taskset -c 0-1 go -C grpc test -race -cover -count=4 -cpu 2 -timeout 2m0s -run '^TestX$/^a_case$' ./grpchealth", x.ReproduceGoTest)
	require.Equal(t, 1, rep.Passed, "TestH")

	saved, err := os.ReadFile(filepath.Join(out, "grpc", "grpchealth", "TestX", "a_case", "confirm-cpu2-load2-2.txt"))
	require.NoError(t, err)
	require.Equal(t, "--- FAIL: TestX/a_case\n", string(saved), "a failed confirm run's whole output is saved")
	entries, err := os.ReadDir(filepath.Join(out, "grpc", "grpchealth", "TestX", "a_case"))
	require.NoError(t, err)
	require.Len(t, entries, 4, "one sweep failure and three failed or hung confirm runs")
}

func TestReport_lists_a_test_apart_when_it_failed_every_confirm_run_at_some_pair(t *testing.T) {
	r, _ := newConfirmResults(t)
	require.NoError(t, r.Add(1, 1, 1, run.Result{Tests: []run.Test{{Name: "TestH", Outcome: run.Fail}}}))
	require.NoError(t, r.Add(1, 1, 1, passed("TestH")))
	for range 4 {
		require.NoError(t, r.Confirmed(1, "TestH", 2, 1, run.Result{Tests: []run.Test{{Name: "TestH", Outcome: run.Fail}}}))
		require.NoError(t, r.Confirmed(1, "TestH", 1, 1, passed("TestH")))
	}
	rep := r.Report()
	require.Empty(t, rep.Flaky)
	require.Len(t, rep.Failing, 1)
	require.True(t, rep.Failing[0].Confirmed)
}

func TestConfirmed_counts_a_run_by_the_test_and_its_subtests(t *testing.T) {
	testCases := []struct {
		name string
		res  run.Result
		want results.Pair
	}{
		{"the test passed", passed("TestX", "TestX/a"), results.Pair{CPU: 1, Load: 1, Runs: 1}},
		{"the test failed", run.Result{Tests: []run.Test{{Name: "TestX", Outcome: run.Fail}}}, results.Pair{CPU: 1, Load: 1, Runs: 1, Failures: 1}},
		{"a subtest failed", run.Result{Tests: []run.Test{{Name: "TestX"}, {Name: "TestX/a", Outcome: run.Fail}}}, results.Pair{CPU: 1, Load: 1, Runs: 1, Failures: 1}},
		{"a subtest hung and another failed", run.Result{Tests: []run.Test{{Name: "TestX"}, {Name: "TestX/a", Outcome: run.Hang}, {Name: "TestX/b", Outcome: run.Fail}}}, results.Pair{CPU: 1, Load: 1, Runs: 1, Hangs: 1}},
		{"the run failed outside any test", run.Result{Tests: []run.Test{{Name: "TestX"}}, PackageFailed: true}, results.Pair{CPU: 1, Load: 1, Runs: 1, Failures: 1}},
		{"the run failed before the test started", run.Result{PackageFailed: true}, results.Pair{CPU: 1, Load: 1, Runs: 1, Failures: 1}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := results.New(results.Config{Packages: pkgs, CPUs: []int{1}, Loads: []int{1}, Out: t.TempDir(), Levels: []results.Level{{Name: "unit", Count: 1, Confirm: 1}}})
			require.NoError(t, r.Add(0, 1, 1, run.Result{Tests: []run.Test{{Name: "TestX", Outcome: run.Fail}}}))
			require.NoError(t, r.Confirmed(0, "TestX", 1, 1, tc.res))
			rep := r.Report()
			tests := append(rep.Flaky, rep.Failing...)
			require.Len(t, tests, 1)
			require.True(t, tests[0].Confirmed)
			require.Equal(t, []results.Pair{tc.want}, tests[0].Pairs)
		})
	}
}

func TestConfirmed_counts_no_run_in_which_the_test_never_started(t *testing.T) {
	r := results.New(results.Config{Packages: pkgs, CPUs: []int{1}, Loads: []int{1}, Out: t.TempDir(), Levels: []results.Level{{Name: "unit", Count: 1, Confirm: 1}}})
	require.NoError(t, r.Add(0, 1, 1, run.Result{Tests: []run.Test{{Name: "TestX", Outcome: run.Fail}}}))
	require.NoError(t, r.Confirmed(0, "TestX", 1, 1, passed("TestXY")))
	rep := r.Report()
	require.Zero(t, rep.Confirmed)
	require.Len(t, rep.Failing, 1)
	require.False(t, rep.Failing[0].Confirmed, "a run that never started the test says nothing about it")
	require.Equal(t, []results.Pair{{CPU: 1, Load: 1, Runs: 1, Failures: 1}}, rep.Failing[0].Pairs, "so the table gives its sweep runs")
}

func TestConfirmed_fails_when_the_output_cannot_be_saved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	r := results.New(results.Config{Packages: pkgs, CPUs: []int{1}, Loads: []int{1}, Out: file, Levels: []results.Level{{Name: "unit", Count: 1, Confirm: 1}}})
	require.Error(t, r.Confirmed(0, "TestX", 1, 1, run.Result{Tests: []run.Test{{Name: "TestX", Outcome: run.Fail}}}))
}

func TestWriteText_of_a_confirmed_test(t *testing.T) {
	r, out := newConfirmResults(t)
	confirmSweep(t, r)
	rep := r.Report()
	rep.Phases = []results.Phase{results.NewPhase("sweep", time.Minute), results.NewPhase("confirm", 2*time.Minute)}
	var buf bytes.Buffer
	require.NoError(t, results.WriteText(&buf, rep))
	want := `FLAKY: failed or hung in some runs

grpc/grpchealth  TestX/a_case
                    failures     hangs      runs
    cpu 1 load 1           0         0         4
    cpu 1 load 2           0         0         4
    cpu 2 load 1           0         1         4
    cpu 2 load 2           2         0         4
    2 failures, 1 hang in 16 confirm runs
    first failure, at cpu 1 load 1:
            x_test.go:12: boom
    output:    ` + filepath.Join(out, "grpc/grpchealth/TestX/a_case") + `
    reproduce: flaky stress -tags unit -run '^TestX$/^a_case$' -cpu 2 -load 2 -count 4 ./grpc/grpchealth
               taskset -c 0-1 go -C grpc test -race -cover -count=4 -cpu 2 -timeout 2m0s -run '^TestX$/^a_case$' ./grpchealth

1 test passed every run
confirmed 1 test in 16 runs
phases: sweep 1m0s, confirm 2m0s
`
	require.Equal(t, want, buf.String())
}

func TestWriteJSON_gives_the_confirm_runs(t *testing.T) {
	r, _ := newConfirmResults(t)
	confirmSweep(t, r)
	var buf bytes.Buffer
	require.NoError(t, results.WriteJSON(&buf, r.Report()))
	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Equal(t, []any{map[string]any{"name": "unit", "count": 3.0, "confirm": 4.0, "timeoutSeconds": 120.0}}, got["levels"])
	require.InDelta(t, 1, got["confirmed"], 0)
	require.InDelta(t, 16, got["confirmRuns"], 0)
	x := got["flaky"].([]any)[0].(map[string]any)
	require.Equal(t, "unit", x["level"])
	require.Equal(t, true, x["confirmed"])
	require.InDelta(t, 16, x["runs"], 0)
}

func seeded(seed int64, res run.Result) run.Result {
	res.Seed = &seed
	return res
}

// orderSweep adds a shuffled sweep of two runs at each pair, in which
// TestX fails at cpu 2 load 1, with seed 7 and then seed 8, and passes every
// other run, then two confirm runs of TestX alone at each pair, which pass.
func orderSweep(t *testing.T, r *results.Results) {
	r.Listed(0, []string{"TestX", "TestY"})
	seed := int64(0)
	for _, cpu := range []int{1, 2} {
		for _, load := range []int{1, 2} {
			for range 2 {
				seed++
				res := passed("TestY", "TestX")
				if cpu == 2 && load == 1 {
					res.Tests[1] = run.Test{Name: "TestX", Outcome: run.Fail, Output: []string{"    x_test.go:5: TestY ran before it"}}
				}
				require.NoError(t, r.Add(0, cpu, load, seeded(100+seed, res)))
			}
		}
	}
	for _, cpu := range []int{1, 2} {
		for _, load := range []int{1, 2} {
			for range 2 {
				seed++
				require.NoError(t, r.Confirmed(0, "TestX", cpu, load, seeded(100+seed, passed("TestX"))))
			}
		}
	}
}

func TestReport_keeps_a_failure_s_seed_and_reproduces_what_its_run_ran_with_it(t *testing.T) {
	testCases := []struct {
		name       string
		run, skip  string
		wantFlaky  string
		wantGoTest string
	}{
		{
			name:       "the sweep ran every test",
			wantFlaky:  "flaky stress -tags unit -run . -shuffle=105 -cpu 2 -load 1 -count 2 ./grpc/grpchealth",
			wantGoTest: "taskset -c 0-1 go -C grpc test -race -cover -count=2 -cpu 2 -timeout 2m0s -run . -shuffle=105 ./grpchealth",
		},
		{
			name:       "the sweep ran what -run matched, and skipped what -skip matched",
			run:        "^Test[XY]$",
			skip:       "^TestZ$",
			wantFlaky:  "flaky stress -tags unit -run '^Test[XY]$' -skip '^TestZ$' -shuffle=105 -cpu 2 -load 1 -count 2 ./grpc/grpchealth",
			wantGoTest: "taskset -c 0-1 go -C grpc test -race -cover -count=2 -cpu 2 -timeout 2m0s -run '^Test[XY]$' -skip '^TestZ$' -shuffle=105 ./grpchealth",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := results.New(results.Config{
				Packages: pkgs, CPUs: []int{1, 2}, Loads: []int{1, 2},
				Out: t.TempDir(),
				Run: tc.run, Skip: tc.skip,
				Levels: []results.Level{{Name: "unit", Count: 3, Confirm: 2, Timeout: 2 * time.Minute, DefaultTimeout: 2 * time.Minute}},
			})
			orderSweep(t, r)
			rep := r.Report()
			require.Len(t, rep.Flaky, 1)
			x := rep.Flaky[0]
			require.True(t, x.Confirmed)
			require.Zero(t, x.Failures, "alone, it passed every confirm run")
			require.Equal(t, int64(105), *x.FirstFailureSeed, "the first failure's seed")
			require.Equal(t, int64(105), *x.Seed, "the seed of the first failure at the pair it failed most, in the sweep")
			require.Equal(t, tc.wantFlaky, x.Reproduce, "a test that fails after another passes alone, so it reproduces what the sweep ran")
			require.Equal(t, tc.wantGoTest, x.ReproduceGoTest)
		})
	}
}

func TestReport_reproduces_a_test_that_failed_alone_with_its_confirm_run_s_seed(t *testing.T) {
	r, _ := newConfirmResults(t)
	require.NoError(t, r.Add(0, 1, 1, seeded(1, run.Result{Tests: []run.Test{{Name: "TestX", Outcome: run.Fail}}})))
	require.NoError(t, r.Confirmed(0, "TestX", 2, 2, seeded(2, passed("TestX"))))
	require.NoError(t, r.Confirmed(0, "TestX", 2, 2, seeded(3, run.Result{Tests: []run.Test{{Name: "TestX", Outcome: run.Fail}}})))
	require.NoError(t, r.Confirmed(0, "TestX", 2, 2, seeded(4, run.Result{Tests: []run.Test{{Name: "TestX", Outcome: run.Fail}}})))
	rep := r.Report()
	require.Len(t, rep.Flaky, 1)
	x := rep.Flaky[0]
	require.Equal(t, int64(1), *x.FirstFailureSeed)
	require.Equal(t, int64(3), *x.Seed, "the first failed confirm run's seed")
	require.Equal(t, "flaky stress -tags unit -run '^TestX$' -shuffle=3 -cpu 2 -load 2 -count 4 ./grpc/grpchealth", x.Reproduce)
	require.Equal(t, "taskset -c 0-1 go -C grpc test -race -cover -count=4 -cpu 2 -timeout 2m0s -run '^TestX$' -shuffle=3 ./grpchealth", x.ReproduceGoTest)
}

func TestWriteText_gives_a_shuffled_failure_s_seed(t *testing.T) {
	r, _ := newConfirmResults(t)
	orderSweep(t, r)
	var buf bytes.Buffer
	require.NoError(t, results.WriteText(&buf, r.Report()))
	require.Contains(t, buf.String(), "    first failure, at cpu 2 load 1, seed 105:\n")
	require.Contains(t, buf.String(), "    reproduce: flaky stress -tags unit -run . -shuffle=105 -cpu 2 -load 1 -count 4 ./grpc/grpchealth\n")
	require.Contains(t, buf.String(), " -run . -shuffle=105 ./grpchealth\n")
}

func TestWriteJSON_gives_a_shuffled_failure_s_seed(t *testing.T) {
	r, _ := newConfirmResults(t)
	orderSweep(t, r)
	var buf bytes.Buffer
	require.NoError(t, results.WriteJSON(&buf, r.Report()))
	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	x := got["flaky"].([]any)[0].(map[string]any)
	require.InDelta(t, 105, x["firstFailureSeed"], 0)
	require.InDelta(t, 105, x["seed"], 0)

	r, _ = newConfirmResults(t)
	confirmSweep(t, r)
	buf.Reset()
	require.NoError(t, results.WriteJSON(&buf, r.Report()))
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	x = got["flaky"].([]any)[0].(map[string]any)
	require.NotContains(t, x, "firstFailureSeed", "a run that wasn't shuffled has no seed")
	require.NotContains(t, x, "seed")
}
