// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package report_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/flaky/discover"
	"github.com/z5labs/humus/tools/flaky/internal/env"
	"github.com/z5labs/humus/tools/flaky/internal/repotest"
	"github.com/z5labs/humus/tools/flaky/report"
	"github.com/z5labs/humus/tools/flaky/results"
	"github.com/z5labs/humus/tools/flaky/run"
)

const commit = "6351c86978a51659d1d1dae16a9f4553bd64a712"

func grid(cells ...[5]int) []results.Pair {
	pairs := make([]results.Pair, len(cells))
	for i, c := range cells {
		pairs[i] = results.Pair{CPU: c[0], Load: c[1], Runs: c[2], Failures: c[3], Hangs: c[4]}
	}
	return pairs
}

func flakyTest() results.Test {
	return results.Test{
		Package:    "grpc/grpchealth",
		ImportPath: "github.com/z5labs/humus/grpc/grpchealth",
		Level:      "unit",
		Test:       "TestServe_reports_serving/after_a_restart",
		Confirmed:  true,
		Runs:       6000,
		Failures:   6,
		Hangs:      0,
		Pairs: grid(
			[5]int{1, 1, 1000, 0, 0}, [5]int{1, 2, 1000, 0, 0},
			[5]int{2, 1, 1000, 1, 0}, [5]int{2, 2, 1000, 3, 0},
			[5]int{4, 1, 1000, 0, 0}, [5]int{4, 2, 1000, 2, 0},
		),
		FirstFailure: []string{
			"--- FAIL: TestServe_reports_serving/after_a_restart (0.02s)",
			"    serve_test.go:88: got NOT_SERVING, want SERVING",
		},
		FirstFailureCPU:  2,
		FirstFailureLoad: 2,
		Reproduce:        "flaky stress -tags unit -run '^TestServe_reports_serving$/^after_a_restart$' -cpu 2 -load 2 -count 1000 ./grpc/grpchealth",
		ReproduceGoTest:  "taskset -c 0-1 go -C grpc test -race -cover -count=1000 -cpu 2 -timeout 2m0s -run '^TestServe_reports_serving$/^after_a_restart$' ./grpchealth",
	}
}

func TestRender_matches_the_golden_files(t *testing.T) {
	hanging := results.Test{
		Package:    ".",
		ImportPath: "github.com/z5labs/humus",
		Level:      "testcontainers",
		Test:       "TestMain_drains",
		Runs:       150,
		Failures:   1,
		Hangs:      1,
		Pairs: grid(
			[5]int{1, 1, 25, 0, 0}, [5]int{1, 2, 25, 0, 0},
			[5]int{2, 1, 25, 0, 1}, [5]int{2, 2, 25, 1, 0},
			[5]int{4, 1, 25, 0, 0}, [5]int{4, 2, 25, 0, 0},
		),
		FirstFailure: []string{
			"--- FAIL: TestMain_drains (0.31s)",
			"    main_test.go:40: the output had ``` in it",
		},
		FirstFailureCPU:  2,
		FirstFailureLoad: 1,
		Reproduce:        "flaky stress -tags testcontainers -run '^TestMain_drains$' -shuffle=7 -cpu 2 -load 1 -count 25 ./.",
		ReproduceGoTest:  "taskset -c 0-1 go -C . test -race -cover -tags testcontainers -count=25 -cpu 2 -timeout 10m0s -run '^TestMain_drains$' -shuffle=7 .",
	}
	cases := []struct {
		name string
		bug  report.Bug
	}{
		{
			name: "flaky",
			bug: report.Bug{
				Test:        flakyTest(),
				Commit:      commit,
				Go:          "go1.27.0",
				Cores:       16,
				Repository:  "https://github.com/z5labs/humus",
				Function:    "https://github.com/z5labs/humus/blob/" + commit + "/grpc/grpchealth/serve_test.go#L61",
				Description: "The test restarts the server and checks it reports `SERVING`.\n\nNow and then it reads `NOT_SERVING` instead.",
			},
		},
		{
			name: "notes_and_closed_report",
			bug: report.Bug{
				Test:        hanging,
				Commit:      commit,
				Go:          "go1.27.2",
				Cores:       4,
				Repository:  "https://github.com/z5labs/humus",
				Function:    "https://github.com/z5labs/humus/blob/" + commit + "/main_test.go#L12",
				Description: "The test drains the app and checks every Service returned.",
				Notes:       "It failed only at cpu 2, at either load.",
				Closed:      &report.Earlier{Title: "humus: TestMain_drains fails intermittently", URL: "https://github.com/z5labs/humus/discussions/959"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title, body := report.Render(c.bug)
			wantTitle, err := os.ReadFile(filepath.Join("testdata", c.name+".title.golden"))
			require.NoError(t, err)
			wantBody, err := os.ReadFile(filepath.Join("testdata", c.name+".body.golden"))
			require.NoError(t, err)
			require.Equal(t, string(wantTitle), title+"\n")
			require.Equal(t, string(wantBody), body)
		})
	}
}

// The ways a sweep can be interrupted, which interruptedResults adds the runs
// of.
const (
	// duringTheSweep is interrupted during the sweep runs, so no test has
	// confirm runs.
	duringTheSweep = iota
	// duringTheConfirms is interrupted during TestAToo's confirm runs,
	// three of whose sixteen end.
	duringTheConfirms
	// beforeAnyConfirmEnded is interrupted before any of TestAToo's
	// confirm runs ends.
	beforeAnyConfirmEnded
	// beforeAConfirmRanIt is interrupted after one of TestAToo's confirm
	// runs ends without running it.
	beforeAConfirmRanIt
	// afterTheConfirmsPassed is interrupted after two of TestAToo's
	// confirm runs end, both passing.
	afterTheConfirmsPassed
)

func runOf(outcomes ...run.Outcome) run.Result {
	res := run.Result{Output: []byte("PASS\n")}
	for i, o := range outcomes {
		t := run.Test{Name: []string{"TestA", "TestAToo"}[i], Outcome: o}
		if o == run.Fail {
			t.Output = []string{"    a_test.go:16: boom"}
		}
		res.Tests = append(res.Tests, t)
	}
	return res
}

// interruptedResults returns the results, at the commit head, of a sweep of
// the package a that was interrupted the way how says. TestAToo fails in the
// sweep once at cpu 1 load 1, and TestA passes every run.
func interruptedResults(t *testing.T, head string, how int) results.Report {
	r := results.New(results.Config{
		Packages: []discover.Package{{Module: ".", Dir: "a", ImportPath: "example.com/repo/a", Level: "unit"}},
		CPUs:     []int{1, 2},
		Loads:    []int{1, 2},
		Out:      t.TempDir(),
		Levels:   []results.Level{{Name: "unit", Count: 3, Confirm: 4, Timeout: 2 * time.Minute, DefaultTimeout: 2 * time.Minute}},
	})
	r.Listed(0, []string{"TestA", "TestAToo"})
	r.Planned(12)
	cancelled := runOf(run.Pass, run.Fail)
	cancelled.Cancelled = true
	if how == duringTheSweep {
		require.NoError(t, r.Add(0, 1, 1, runOf(run.Pass, run.Fail)))
		require.NoError(t, r.Add(0, 1, 1, runOf(run.Pass, run.Pass)))
		require.NoError(t, r.Add(0, 1, 2, runOf(run.Pass, run.Pass)))
		require.NoError(t, r.Add(0, 2, 1, cancelled))
	} else {
		for _, cpu := range []int{1, 2} {
			for _, load := range []int{1, 2} {
				for i := range 3 {
					outcome := run.Pass
					if cpu == 1 && load == 1 && i == 0 {
						outcome = run.Fail
					}
					require.NoError(t, r.Add(0, cpu, load, runOf(run.Pass, outcome)))
				}
			}
		}
		r.Planned(16)
		r.Confirming(0, "TestAToo")
		confirm := func(cpu, load int, res run.Result) {
			res.Tests = slices.DeleteFunc(res.Tests, func(t run.Test) bool { return t.Name != "TestAToo" })
			require.NoError(t, r.Confirmed(0, "TestAToo", cpu, load, res))
		}
		switch how {
		case duringTheConfirms:
			confirm(1, 1, runOf(run.Pass, run.Fail))
			confirm(1, 1, runOf(run.Pass, run.Pass))
			confirm(1, 2, runOf(run.Pass, run.Pass))
		case beforeAConfirmRanIt:
			confirm(1, 1, runOf())
		case afterTheConfirmsPassed:
			confirm(1, 1, runOf(run.Pass, run.Pass))
			confirm(1, 2, runOf(run.Pass, run.Pass))
		}
		confirm(2, 1, cancelled)
	}
	rep := r.Report()
	rep.Interrupted = true
	rep.Commit, rep.Go, rep.Cores = head, "go1.27.0", 8
	return rep
}

func TestRender_of_an_interrupted_sweep_matches_the_golden_files(t *testing.T) {
	cases := []struct {
		name string
		how  int
	}{
		{"interrupted_during_the_sweep", duringTheSweep},
		{"interrupted_during_the_confirms", duringTheConfirms},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := interruptedResults(t, commit, c.how)
			test, err := report.Find(rep, "a", "TestAToo")
			require.NoError(t, err)
			title, body := report.Render(report.Bug{
				Test:        test,
				Commit:      rep.Commit,
				Go:          rep.Go,
				Cores:       rep.Cores,
				Interrupted: rep.Interrupted,
				PlannedRuns: rep.PlannedRuns,
				EndedRuns:   rep.EndedRuns,
				Repository:  "https://github.com/example/repo",
				Function:    "https://github.com/example/repo/blob/" + commit + "/a/a_test.go#L16",
				Description: "It checks nothing, and fails anyway.",
			})
			wantTitle, err := os.ReadFile(filepath.Join("testdata", c.name+".title.golden"))
			require.NoError(t, err)
			wantBody, err := os.ReadFile(filepath.Join("testdata", c.name+".body.golden"))
			require.NoError(t, err)
			require.Equal(t, string(wantTitle), title+"\n")
			require.Equal(t, string(wantBody), body)
		})
	}
}

func TestRender_of_an_interrupted_sweep_says_which_runs_the_counts_are(t *testing.T) {
	cases := []struct {
		name string
		how  int
		want string
	}{
		{"no confirm runs", duringTheSweep, "The sweep was interrupted, when 3 of its 12 planned runs had ended, so the counts below are of the runs that ended. It has no confirm runs, so they are its sweep runs.\n\nAt ["},
		{"confirm runs that ended", duringTheConfirms, "The sweep was interrupted, when 15 of its 28 planned runs had ended, so the counts below are of the runs that ended. Its confirm runs didn't all end: 3 of its 16 did, and they are of those.\n\nAt ["},
		{"no confirm run ended", beforeAnyConfirmEnded, "The sweep was interrupted, when 12 of its 28 planned runs had ended, so the counts below are of the runs that ended. Its confirm runs didn't all end: 0 of its 16 did, so they are its sweep runs.\n\nAt ["},
		{"no confirm run that ended ran it", beforeAConfirmRanIt, "The sweep was interrupted, when 13 of its 28 planned runs had ended, so the counts below are of the runs that ended. Its confirm runs didn't all end: 1 of its 16 did, and none of those ran it, so they are its sweep runs.\n\nAt ["},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := interruptedResults(t, commit, c.how)
			test, err := report.Find(rep, "a", "TestAToo")
			require.NoError(t, err)
			_, body := report.Render(report.Bug{
				Test: test, Commit: commit, Repository: "https://github.com/o/r", Description: "d",
				Interrupted: rep.Interrupted, PlannedRuns: rep.PlannedRuns, EndedRuns: rep.EndedRuns,
			})
			require.Contains(t, body, "## What happens\n\nd\n\n"+c.want)
		})
	}
}

func TestRender_of_a_sweep_that_ran_to_the_end_says_nothing_of_an_interruption(t *testing.T) {
	_, body := report.Render(report.Bug{Test: flakyTest(), Commit: commit, Repository: "https://github.com/o/r", Description: "d", PlannedRuns: 6024, EndedRuns: 6024})
	require.NotContains(t, body, "interrupted")
	require.Contains(t, body, "## What happens\n\nd\n\nAt [")
}

func TestRender_gives_a_level_run_at_one_pair_a_table_of_one_cell(t *testing.T) {
	test := flakyTest()
	test.Pairs = grid([5]int{4, 1, 50, 2, 0})
	_, body := report.Render(report.Bug{Test: test, Commit: commit, Repository: "https://github.com/o/r", Description: "d"})
	require.Contains(t, body, "|       | load 1 |\n|-------|--------|\n| cpu 4 | 2 / 50 |\n")
}

func TestCheckText_refuses_an_empty_text(t *testing.T) {
	for _, text := range []string{"", " \n\t\n"} {
		require.EqualError(t, report.CheckText(text), "it is empty")
	}
}

func TestCheckText_refuses_a_heading(t *testing.T) {
	for _, text := range []string{
		"## Why\n\nbecause",
		"# Title",
		"   ### indented up to three spaces",
		"Some words\n\n#",
		"A setext heading\n================",
		"A setext heading\n---",
		"Before\n<h2>An HTML heading</h2>",
		"```go\nfunc f() {}\n```\n## after the fence",
	} {
		require.ErrorContains(t, report.CheckText(text), "heading", "%q", text)
	}
}

func TestCheckText_takes_Markdown_with_no_heading(t *testing.T) {
	for _, text := range []string{
		"It fails at cpu 2.",
		"#hashtag and #1254 aren't headings",
		"    # indented four spaces is code",
		"```sh\n# a comment in a fence\n```",
		"~~~~\n## in a tilde fence\n~~~~\n\nafter",
		"A paragraph.\n\n---\n\nA thematic break after a blank line.",
		"- a list item\n---",
		"> a quote\n---",
	} {
		require.NoError(t, report.CheckText(text), "%q", text)
	}
}

func reportWith(flaky, failing []results.Test) results.Report {
	return results.Report{
		Commit: commit,
		Packages: []discover.Package{
			{Module: "grpc", Dir: "grpc/grpchealth", ImportPath: "github.com/z5labs/humus/grpc/grpchealth", Level: "unit"},
		},
		Flaky:   flaky,
		Failing: failing,
	}
}

func TestFind_finds_the_test_by_its_package_s_directory_or_import_path(t *testing.T) {
	rep := reportWith([]results.Test{flakyTest()}, nil)
	for _, pkg := range []string{"grpc/grpchealth", "./grpc/grpchealth", "grpc/grpchealth/", "github.com/z5labs/humus/grpc/grpchealth"} {
		got, err := report.Find(rep, pkg, "TestServe_reports_serving/after_a_restart")
		require.NoError(t, err, pkg)
		require.Equal(t, flakyTest(), got)
	}
}

func TestFind_refuses_a_package_the_results_don_t_have(t *testing.T) {
	_, err := report.Find(reportWith([]results.Test{flakyTest()}, nil), "health", "TestServe_reports_serving/after_a_restart")
	require.EqualError(t, err, "the results don't have the package health")
}

func TestFind_refuses_a_test_that_passed_every_run(t *testing.T) {
	_, err := report.Find(reportWith([]results.Test{flakyTest()}, nil), "grpc/grpchealth", "TestServe_reports_serving")
	require.EqualError(t, err, "the results don't have TestServe_reports_serving failing or hanging in grpc/grpchealth: it passed every run, or didn't run")
}

func TestFind_refuses_a_test_that_passed_every_confirm_run(t *testing.T) {
	test := flakyTest()
	test.Failures, test.Hangs = 0, 0
	_, err := report.Find(reportWith([]results.Test{test}, nil), "grpc/grpchealth", test.Test)
	require.EqualError(t, err, "TestServe_reports_serving/after_a_restart passed every one of its 6,000 confirm runs alone, so the results have no failure of it to report")
}

func TestFind_refuses_a_test_whose_confirm_runs_that_ended_all_passed(t *testing.T) {
	rep := interruptedResults(t, commit, afterTheConfirmsPassed)
	_, err := report.Find(rep, "a", "TestAToo")
	require.EqualError(t, err, "TestAToo passed every one of the 2 of its 16 confirm runs that ended, alone, so the results have no failure of it to report")
}

func TestFind_refuses_a_test_of_an_interrupted_sweep_that_failed_every_run_that_ended_at_some_pair(t *testing.T) {
	rep := interruptedResults(t, commit, duringTheSweep)
	rep.Flaky, rep.Failing = nil, rep.Flaky
	rep.Failing[0].Pairs[0].Failures = rep.Failing[0].Pairs[0].Runs
	_, err := report.Find(rep, "a", "TestAToo")
	require.EqualError(t, err, "TestAToo failed every run at cpu 1 load 1, so it is failing rather than flaky")
}

func TestFind_refuses_a_test_that_failed_every_run_at_some_pair(t *testing.T) {
	test := flakyTest()
	test.Pairs[3].Failures = test.Pairs[3].Runs
	_, err := report.Find(reportWith(nil, []results.Test{test}), "grpc/grpchealth", test.Test)
	require.EqualError(t, err, "TestServe_reports_serving/after_a_restart failed every run at cpu 2 load 2, so it is failing rather than flaky")
}

func TestNames_needs_the_whole_name(t *testing.T) {
	require.True(t, report.Names("`TestX` fails", "TestX"))
	require.True(t, report.Names("TestX", "TestX"))
	require.True(t, report.Names("see TestX/a_case.", "TestX"), "a subtest's name names its test")
	require.True(t, report.Names("TestXY and then TestX", "TestX"), "a later whole match counts")
	require.False(t, report.Names("TestXY fails", "TestX"))
	require.False(t, report.Names("ATestX fails", "TestX"))
	require.False(t, report.Names("TestX_too fails", "TestX"))
	require.False(t, report.Names("TestX fails", "TestX/a_case"))
	require.False(t, report.Names("anything", ""))
}

func TestReports_picks_the_earliest_open_report_and_the_last_closed_one(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	reports := []report.Earlier{
		{URL: "open-later", Body: "TestX fails", Open: true, Created: day(5)},
		{URL: "open-earlier", Title: "pkg: TestX fails intermittently", Open: true, Created: day(3)},
		{URL: "open-other", Body: "TestXY fails", Open: true, Created: day(1)},
		{URL: "closed-earlier", Body: "TestX", Created: day(2)},
		{URL: "closed-later", Body: "TestX", Created: day(4)},
		{URL: "closed-other", Body: "TestXY", Created: day(6)},
	}
	open, closed := report.Reports(reports, "TestX")
	require.Equal(t, "open-earlier", open.URL)
	require.Equal(t, "closed-later", closed.URL)

	open, closed = report.Reports(reports[3:], "TestX")
	require.Nil(t, open)
	require.Equal(t, "closed-later", closed.URL)

	open, closed = report.Reports(nil, "TestX")
	require.Nil(t, open)
	require.Nil(t, closed)
}

// repo returns a repository holding testdata/repo, with a test in its root
// package, and its commit.
func repo(t *testing.T) (string, string) {
	dir := repotest.New(t, "1.27.0")
	repotest.Write(t, dir, "root_test.go", "package repo\n\nimport \"testing\"\n\nfunc TestRoot(t *testing.T) {}\n")
	repotest.Commit(t, dir)
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	return dir, strings.TrimSpace(string(out))
}

func childEnv() []string { return env.Child(bedrock.OSEnvironment{}) }

func TestFunction_finds_the_top_level_test_s_function_at_the_commit(t *testing.T) {
	dir, head := repo(t)
	file, line, err := report.Function(t.Context(), dir, head, results.Test{Package: "a", Test: "TestAToo/a_case"}, childEnv())
	require.NoError(t, err)
	require.Equal(t, "a/a_test.go", file)
	require.Equal(t, 16, line)

	file, line, err = report.Function(t.Context(), dir, head, results.Test{Package: ".", Test: "TestRoot"}, childEnv())
	require.NoError(t, err)
	require.Equal(t, "root_test.go", file)
	require.Equal(t, 5, line)
}

func TestFunction_refuses_a_test_with_no_function(t *testing.T) {
	dir, head := repo(t)
	_, _, err := report.Function(t.Context(), dir, head, results.Test{Package: "a", Test: "TestA_"}, childEnv())
	require.ErrorContains(t, err, "there is no func TestA_ in the test files of a at "+head)

	_, _, err = report.Function(t.Context(), dir, head, results.Test{Package: ".", Test: "TestA"}, childEnv())
	require.ErrorContains(t, err, "there is no func TestA in the test files of .", "a subdirectory's test files aren't the package's")
}

// fakeGitHub is a GitHub that finds the reports it holds, and records what
// it is asked.
type fakeGitHub struct {
	reports   []report.Earlier
	searchErr error
	searched  []string
	created   [][2]string
}

func (f *fakeGitHub) Search(_ context.Context, test string) (report.Found, error) {
	f.searched = append(f.searched, test)
	return report.Found{Repository: "https://github.com/z5labs/humus/", Reports: f.reports}, f.searchErr
}

func (f *fakeGitHub) Create(_ context.Context, title, body string) (string, error) {
	f.created = append(f.created, [2]string{title, body})
	return "https://github.com/z5labs/humus/discussions/2000", nil
}

// request returns a request for TestAToo in package a of a new repository,
// whose results say it flaked at that repository's commit, with a
// description and the notes when notes isn't "".
func request(t *testing.T, notes string) report.Request {
	return requestFor(t, notes, func(head string) results.Report {
		test := flakyTest()
		test.Package, test.ImportPath, test.Test = "a", "example.com/repo/a", "TestAToo"
		return results.Report{
			Commit: head, Go: "go1.27.0", Cores: 8,
			Packages: []discover.Package{{Module: ".", Dir: "a", ImportPath: "example.com/repo/a", Level: "unit"}},
			Flaky:    []results.Test{test},
		}
	})
}

// interruptedRequest returns a request like request's, whose results are of
// a sweep interrupted the way how says.
func interruptedRequest(t *testing.T, notes string, how int) report.Request {
	return requestFor(t, notes, func(head string) results.Report { return interruptedResults(t, head, how) })
}

// requests are the requests for TestAToo that every check applies to: from a
// sweep that ran to the end, and from sweeps interrupted during the sweep and
// during the confirms.
var requests = []struct {
	name string
	new  func(t *testing.T, notes string) report.Request
}{
	{"ran to the end", request},
	{"interrupted during the sweep", func(t *testing.T, notes string) report.Request {
		return interruptedRequest(t, notes, duringTheSweep)
	}},
	{"interrupted during the confirms", func(t *testing.T, notes string) report.Request {
		return interruptedRequest(t, notes, duringTheConfirms)
	}},
}

// requestFor returns a request for TestAToo in package a of a new repository,
// whose results are what resultsAt returns for that repository's commit.
func requestFor(t *testing.T, notes string, resultsAt func(head string) results.Report) report.Request {
	dir, head := repo(t)
	b, err := json.Marshal(resultsAt(head))
	require.NoError(t, err)
	files := t.TempDir()
	req := report.Request{
		Results:     filepath.Join(files, "results.json"),
		Package:     "a",
		Test:        "TestAToo",
		Description: filepath.Join(files, "description.md"),
		Root:        dir,
		Env:         childEnv(),
	}
	require.NoError(t, os.WriteFile(req.Results, b, 0o644))
	require.NoError(t, os.WriteFile(req.Description, []byte("It checks nothing, and fails anyway.\n"), 0o644))
	if notes != "" {
		req.Notes = filepath.Join(files, "notes.md")
		require.NoError(t, os.WriteFile(req.Notes, []byte(notes), 0o644))
	}
	return req
}

func TestFile_creates_the_discussion_and_prints_its_URL(t *testing.T) {
	req := request(t, "Only at cpu 2.\n")
	gh := &fakeGitHub{}
	var out bytes.Buffer
	require.NoError(t, report.File(t.Context(), gh, req, &out))
	require.Equal(t, "https://github.com/z5labs/humus/discussions/2000\n", out.String())
	require.Equal(t, []string{"TestAToo"}, gh.searched)
	require.Len(t, gh.created, 1)
	require.Equal(t, "a: TestAToo fails intermittently", gh.created[0][0])
	body := gh.created[0][1]
	require.Contains(t, body, "## What happens\n\nIt checks nothing, and fails anyway.\n\n")
	require.Contains(t, body, "with go1.27.0 on 8 cores")
	require.Contains(t, body, "\nOnly at cpu 2.\n\n## Why\n")
	require.Contains(t, body, "- [`TestAToo`](https://github.com/z5labs/humus/blob/"+gitHead(t, req.Root)+"/a/a_test.go#L16)\n")
	require.NotContains(t, body, "the closed report")
	require.NotContains(t, body, "interrupted", "a sweep that ran to the end renders as before")
}

func TestFile_posts_a_test_of_an_interrupted_sweep_and_says_so(t *testing.T) {
	cases := []struct {
		name  string
		how   int
		want  []string
		table string
	}{
		{
			name:  "during the sweep",
			how:   duringTheSweep,
			want:  []string{"The sweep was interrupted, when 3 of its 12 planned runs had ended", "It has no confirm runs, so they are its sweep runs.", "`flaky stress` ran it 3 times:"},
			table: "|       | load 1 | load 2 |\n|-------|--------|--------|\n| cpu 1 | 1 / 2  | 0 / 1  |\n| cpu 2 | 0 / 0  | 0 / 0  |\n",
		},
		{
			name:  "during the confirms",
			how:   duringTheConfirms,
			want:  []string{"The sweep was interrupted, when 15 of its 28 planned runs had ended", "Its confirm runs didn't all end: 3 of its 16 did, and they are of those.", "`flaky stress` ran it 3 times:"},
			table: "|       | load 1 | load 2 |\n|-------|--------|--------|\n| cpu 1 | 1 / 2  | 0 / 1  |\n| cpu 2 | 0 / 0  | 0 / 0  |\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := interruptedRequest(t, "", c.how)
			gh := &fakeGitHub{}
			var out bytes.Buffer
			require.NoError(t, report.File(t.Context(), gh, req, &out))
			require.Equal(t, "https://github.com/z5labs/humus/discussions/2000\n", out.String())
			require.Len(t, gh.created, 1)
			title, body := gh.created[0][0], gh.created[0][1]
			require.Equal(t, "a: TestAToo fails intermittently", title)
			for _, want := range c.want {
				require.Contains(t, body, want)
			}
			require.Contains(t, body, c.table, "its table gives the runs that ended")

			req.DryRun = true
			dry := &fakeGitHub{}
			out.Reset()
			require.NoError(t, report.File(t.Context(), dry, req, &out))
			require.Empty(t, dry.created)
			require.Equal(t, "Title: "+title+"\n\n"+body, out.String(), "-dry-run prints what it would post")
		})
	}
}

func gitHead(t *testing.T, dir string) string {
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func TestFile_keeps_the_indentation_of_the_description_s_first_line(t *testing.T) {
	req := request(t, "")
	require.NoError(t, os.WriteFile(req.Description, []byte("\n  \n    panic: boom\n\nIt panics.\n\n"), 0o644))
	gh := &fakeGitHub{}
	require.NoError(t, report.File(t.Context(), gh, req, &bytes.Buffer{}))
	require.Len(t, gh.created, 1)
	require.Contains(t, gh.created[0][1], "## What happens\n\n    panic: boom\n\nIt panics.\n\nAt [", "an indented code block stays code")
}

func TestFile_with_DryRun_prints_the_title_and_body_and_creates_nothing(t *testing.T) {
	req := request(t, "")
	req.DryRun = true
	gh := &fakeGitHub{}
	var out bytes.Buffer
	require.NoError(t, report.File(t.Context(), gh, req, &out))
	require.Empty(t, gh.created)
	require.True(t, strings.HasPrefix(out.String(), "Title: a: TestAToo fails intermittently\n\n## What happens\n"), out.String())
	require.Contains(t, out.String(), "## References\n")
}

func TestFile_refuses_when_an_open_report_names_the_test(t *testing.T) {
	for _, r := range requests {
		t.Run(r.name, func(t *testing.T) {
			gh := &fakeGitHub{reports: []report.Earlier{
				{URL: "https://github.com/z5labs/humus/discussions/1999", Title: "a: TestAToo fails intermittently", Open: true},
			}}
			err := report.File(t.Context(), gh, r.new(t, ""), &bytes.Buffer{})
			var open *report.OpenError
			require.ErrorAs(t, err, &open)
			require.Equal(t, "https://github.com/z5labs/humus/discussions/1999", open.Report.URL)
			require.EqualError(t, err, "an open report already names TestAToo: https://github.com/z5labs/humus/discussions/1999")
			require.Empty(t, gh.created)
		})
	}
}

func TestFile_links_a_closed_report_that_names_the_test(t *testing.T) {
	gh := &fakeGitHub{reports: []report.Earlier{
		{URL: "https://github.com/z5labs/humus/issues/1998", Body: "Fix `TestAToo`.", Open: false},
		{URL: "https://github.com/z5labs/humus/discussions/1997", Body: "TestATooMuch", Open: true},
	}}
	require.NoError(t, report.File(t.Context(), gh, request(t, ""), &bytes.Buffer{}))
	require.Len(t, gh.created, 1)
	require.Contains(t, gh.created[0][1], "\n- https://github.com/z5labs/humus/issues/1998, the closed report that last named it\n")
}

func TestFile_posts_nothing_when_a_check_fails(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, req *report.Request)
		want   string
	}{
		{"an empty description", func(t *testing.T, req *report.Request) {
			require.NoError(t, os.WriteFile(req.Description, nil, 0o644))
		}, "it is empty"},
		{"a description with a heading", func(t *testing.T, req *report.Request) {
			require.NoError(t, os.WriteFile(req.Description, []byte("## Cause\n"), 0o644))
		}, "line 1 is a heading"},
		{"empty notes", func(t *testing.T, req *report.Request) {
			req.Notes = filepath.Join(t.TempDir(), "notes.md")
			require.NoError(t, os.WriteFile(req.Notes, []byte("\n"), 0o644))
		}, "-notes"},
		{"notes with a heading", func(t *testing.T, req *report.Request) {
			req.Notes = filepath.Join(t.TempDir(), "notes.md")
			require.NoError(t, os.WriteFile(req.Notes, []byte("Notes\n=====\n"), 0o644))
		}, "underlines line 1 as a heading"},
		{"a test the results don't have", func(t *testing.T, req *report.Request) {
			req.Test = "TestA"
		}, "the results don't have TestA failing or hanging in a"},
		{"a test that failed every run at some pair", func(t *testing.T, req *report.Request) {
			var rep results.Report
			b, err := os.ReadFile(req.Results)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(b, &rep))
			rep.Failing, rep.Flaky = rep.Flaky, nil
			b, err = json.Marshal(rep)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(req.Results, b, 0o644))
		}, "failing rather than flaky"},
		{"results that aren't flaky stress -json's", func(t *testing.T, req *report.Request) {
			require.NoError(t, os.WriteFile(req.Results, []byte("FLAKY: failed or hung in some runs\n"), 0o644))
		}, "isn't the output of flaky stress -json"},
	}
	for _, r := range requests {
		for _, c := range cases {
			t.Run(r.name+"/"+c.name, func(t *testing.T) {
				req := r.new(t, "")
				c.change(t, &req)
				gh := &fakeGitHub{}
				var out bytes.Buffer
				err := report.File(t.Context(), gh, req, &out)
				require.ErrorContains(t, err, c.want)
				var open *report.OpenError
				require.False(t, errors.As(err, &open))
				require.Empty(t, gh.searched, "it checks before it calls GitHub")
				require.Empty(t, gh.created)
				require.Empty(t, out.String())
			})
		}
	}
}

func TestFile_posts_nothing_when_the_search_fails(t *testing.T) {
	gh := &fakeGitHub{searchErr: errors.New("gh: HTTP 502")}
	err := report.File(t.Context(), gh, request(t, ""), &bytes.Buffer{})
	require.EqualError(t, err, "searching for an earlier report of TestAToo: gh: HTTP 502")
	require.Empty(t, gh.created)
}
