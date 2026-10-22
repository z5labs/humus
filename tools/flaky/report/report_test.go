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
	dir, head := repo(t)
	test := flakyTest()
	test.Package, test.ImportPath, test.Test = "a", "example.com/repo/a", "TestAToo"
	rep := results.Report{
		Commit: head, Go: "go1.27.0", Cores: 8,
		Packages: []discover.Package{{Module: ".", Dir: "a", ImportPath: "example.com/repo/a", Level: "unit"}},
		Flaky:    []results.Test{test},
	}
	b, err := json.Marshal(rep)
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
	gh := &fakeGitHub{reports: []report.Earlier{
		{URL: "https://github.com/z5labs/humus/discussions/1999", Title: "a: TestAToo fails intermittently", Open: true},
	}}
	err := report.File(t.Context(), gh, request(t, ""), &bytes.Buffer{})
	var open *report.OpenError
	require.ErrorAs(t, err, &open)
	require.Equal(t, "https://github.com/z5labs/humus/discussions/1999", open.Report.URL)
	require.EqualError(t, err, "an open report already names TestAToo: https://github.com/z5labs/humus/discussions/1999")
	require.Empty(t, gh.created)
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
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := request(t, "")
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

func TestFile_posts_nothing_when_the_search_fails(t *testing.T) {
	gh := &fakeGitHub{searchErr: errors.New("gh: HTTP 502")}
	err := report.File(t.Context(), gh, request(t, ""), &bytes.Buffer{})
	require.EqualError(t, err, "searching for an earlier report of TestAToo: gh: HTTP 502")
	require.Empty(t, gh.created)
}
