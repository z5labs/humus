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
	{Module: "grpc", Dir: "grpc/grpchealth", ImportPath: "github.com/z5labs/humus/grpc/grpchealth"},
	{Module: ".", Dir: "health", ImportPath: "github.com/z5labs/humus/health"},
}

func newResults(t *testing.T) (*results.Results, string) {
	out := t.TempDir()
	return results.New(results.Config{
		Packages:       pkgs,
		CPUs:           []int{1, 2, 4},
		Count:          3,
		Timeout:        2 * time.Minute,
		DefaultTimeout: 2 * time.Minute,
		Out:            out,
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

// sweep adds three runs of each package at each CPU count. TestX/a_case
// fails once at cpu 2 and hangs once at cpu 4; TestY fails every run at cpu
// 1; health fails outside any test once at cpu 4.
func sweep(t *testing.T, r *results.Results) {
	r.Listed(0, []string{"TestX", "TestY", "TestZ", "TestNeverRan"})
	r.Listed(1, []string{"TestH"})
	for _, cpu := range []int{1, 2, 4} {
		for i := range 3 {
			res := passed("TestX", "TestX/a_case", "TestY", "TestZ")
			if cpu == 2 && i == 1 {
				res.Tests[1] = run.Test{Name: "TestX/a_case", Outcome: run.Fail, Output: []string{"    x_test.go:12: boom"}}
				res.Output = []byte("--- FAIL: TestX/a_case\n    x_test.go:12: boom\n")
			}
			if cpu == 4 && i == 2 {
				res.Tests[1] = run.Test{Name: "TestX/a_case", Outcome: run.Hang, Output: []string{"=== RUN   TestX/a_case"}}
				res.TimedOut = true
			}
			if cpu == 1 {
				res.Tests[2] = run.Test{Name: "TestY", Outcome: run.Fail, Output: []string{"    y_test.go:3: always"}}
			}
			require.NoError(t, r.Add(0, cpu, res))

			h := passed("TestH")
			if cpu == 4 && i == 0 {
				h = run.Result{
					Tests:         []run.Test{{Name: "TestH", Outcome: run.Pass}},
					PackageFailed: true,
					PackageOutput: []string{"goleak: found unexpected goroutines"},
					Output:        []byte("goleak: found unexpected goroutines\n"),
				}
			}
			require.NoError(t, r.Add(1, cpu, h))
		}
	}
}

func TestReport_counts_each_test_s_failures_and_hangs_at_each_CPU_count(t *testing.T) {
	r, out := newResults(t)
	sweep(t, r)
	rep := r.Report()
	require.True(t, rep.Failed())

	require.Len(t, rep.Flaky, 2)
	x := rep.Flaky[0]
	require.Equal(t, "grpc/grpchealth", x.Package)
	require.Equal(t, "TestX/a_case", x.Test)
	require.Equal(t, []results.CPU{
		{CPU: 1, Runs: 3},
		{CPU: 2, Runs: 3, Failures: 1},
		{CPU: 4, Runs: 3, Hangs: 1},
	}, x.CPUs)
	require.Equal(t, 9, x.Runs)
	require.Equal(t, 1, x.Failures)
	require.Equal(t, 1, x.Hangs)
	require.Equal(t, []string{"    x_test.go:12: boom"}, x.FirstFailure)
	require.Equal(t, 2, x.FirstFailureCPU)
	require.Equal(t, filepath.Join(out, "grpc", "grpchealth", "TestX", "a_case"), x.Output)
	require.Equal(t, "flaky stress -run '^TestX$/^a_case$' -cpu 2 -count 3 ./grpc/grpchealth", x.Reproduce)
	require.Equal(t, "taskset -c 0-1 go -C grpc test -race -cover -count=3 -cpu 2 -timeout 2m0s -run '^TestX$/^a_case$' ./grpchealth", x.ReproduceGoTest)

	h := rep.Flaky[1]
	require.Equal(t, "health", h.Package)
	require.Empty(t, h.Test, "a failure outside any test counts against its package")
	require.Equal(t, filepath.Join(out, "health", "outside-tests"), h.Output)
	require.Equal(t, "flaky stress -cpu 4 -count 3 ./health", h.Reproduce)
	require.Equal(t, "taskset -c 0-3 go -C . test -race -cover -count=3 -cpu 4 -timeout 2m0s ./health", h.ReproduceGoTest)
	require.Equal(t, []string{"goleak: found unexpected goroutines"}, h.FirstFailure)

	require.Len(t, rep.Failing, 1, "a test that failed every run at some CPU count is listed apart")
	y := rep.Failing[0]
	require.Equal(t, "TestY", y.Test)
	require.Equal(t, 3, y.Failures)
	require.Equal(t, "taskset -c 0 go -C grpc test -race -cover -count=3 -cpu 1 -timeout 2m0s -run '^TestY$' ./grpchealth", y.ReproduceGoTest)

	// TestX failed through its subtest, TestY failed, and TestNeverRan
	// never ran; TestZ and TestH passed every run.
	require.Equal(t, 2, rep.Passed)
}

func TestAdd_saves_each_failure_s_whole_output(t *testing.T) {
	r, out := newResults(t)
	sweep(t, r)
	saved, err := os.ReadFile(filepath.Join(out, "grpc", "grpchealth", "TestX", "a_case", "cpu2-1.txt"))
	require.NoError(t, err)
	require.Equal(t, "--- FAIL: TestX/a_case\n    x_test.go:12: boom\n", string(saved))
	entries, err := os.ReadDir(filepath.Join(out, "grpc", "grpchealth", "TestY"))
	require.NoError(t, err)
	require.Len(t, entries, 3)
	_, err = os.Stat(filepath.Join(out, "grpc", "grpchealth", "TestX", "a_case", "cpu4-2.txt"))
	require.NoError(t, err)
}

func TestAdd_fails_when_the_output_cannot_be_saved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	r := results.New(results.Config{Packages: pkgs, CPUs: []int{1}, Count: 1, Out: file})
	err := r.Add(0, 1, run.Result{Tests: []run.Test{{Name: "TestX", Outcome: run.Fail}}})
	require.Error(t, err)
	err = r.Add(1, 1, run.Result{PackageFailed: true})
	require.Error(t, err)
}

func TestAdd_keeps_a_hang_s_or_a_kill_s_cause_in_its_first_failure(t *testing.T) {
	r, _ := newResults(t)
	require.NoError(t, r.Add(0, 1, run.Result{Tests: []run.Test{{Name: "TestA", Outcome: run.Hang}}, Killed: true}))
	require.NoError(t, r.Add(1, 1, run.Result{PackageFailed: true, Killed: true}))
	long := make([]string, 80)
	for i := range long {
		long[i] = "line"
	}
	require.NoError(t, r.Add(0, 2, run.Result{Tests: []run.Test{{Name: "TestB/c d", Outcome: run.Fail, Output: long}}}))
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
		Packages: pkgs, CPUs: []int{1}, Count: 5, Out: t.TempDir(),
		Timeout: 10 * time.Second, DefaultTimeout: 2 * time.Minute,
	})
	require.NoError(t, r.Add(1, 1, run.Result{Tests: []run.Test{{Name: "TestH", Outcome: run.Fail}, {Name: "TestH2", Outcome: run.Pass}}}))
	require.NoError(t, r.Add(1, 1, passed("TestH", "TestH2")))
	rep := r.Report()
	require.Equal(t, "flaky stress -run '^TestH$' -cpu 1 -count 5 -timeout 10s ./health", rep.Flaky[0].Reproduce)
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
    cpu 1            0         0         3
    cpu 2            1         0         3
    cpu 4            0         1         3
    1 failure, 1 hang in 9 runs
    first failure, at cpu 2:
            x_test.go:12: boom
    output:    ` + filepath.Join(out, "grpc/grpchealth/TestX/a_case") + `
    reproduce: flaky stress -run '^TestX$/^a_case$' -cpu 2 -count 3 ./grpc/grpchealth
               taskset -c 0-1 go -C grpc test -race -cover -count=3 -cpu 2 -timeout 2m0s -run '^TestX$/^a_case$' ./grpchealth

health  (outside any test)
              failures     hangs      runs
    cpu 1            0         0         3
    cpu 2            0         0         3
    cpu 4            1         0         3
    1 failure, 0 hangs in 9 runs
    first failure, at cpu 4:
        goleak: found unexpected goroutines
    output:    ` + filepath.Join(out, "health/outside-tests") + `
    reproduce: flaky stress -cpu 4 -count 3 ./health
               taskset -c 0-3 go -C . test -race -cover -count=3 -cpu 4 -timeout 2m0s ./health

FAILING: failed or hung every run at some CPU count

grpc/grpchealth  TestY
              failures     hangs      runs
    cpu 1            3         0         3
    cpu 2            0         0         3
    cpu 4            0         0         3
    3 failures, 0 hangs in 9 runs
    first failure, at cpu 1:
            y_test.go:3: always
    output:    ` + filepath.Join(out, "grpc/grpchealth/TestY") + `
    reproduce: flaky stress -run '^TestY$' -cpu 1 -count 3 ./grpc/grpchealth
               taskset -c 0 go -C grpc test -race -cover -count=3 -cpu 1 -timeout 2m0s -run '^TestY$' ./grpchealth

2 tests passed every run
phases: discovery 1s, build 1m0s
`
	require.Equal(t, want, buf.String())
}

func TestWriteText_of_a_sweep_that_passed(t *testing.T) {
	r, _ := newResults(t)
	r.Listed(1, []string{"TestH"})
	require.NoError(t, r.Add(1, 1, passed("TestH")))
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
	require.InDelta(t, 32, got["cores"], 0)
	require.InDelta(t, 2, got["passed"], 0)
	require.Equal(t, []any{map[string]any{"name": "sweep", "seconds": 90.0}}, got["phases"])
	flaky := got["flaky"].([]any)
	require.Len(t, flaky, 2)
	require.Equal(t, "TestX/a_case", flaky[0].(map[string]any)["test"])
	require.Len(t, got["failing"], 1)
}
