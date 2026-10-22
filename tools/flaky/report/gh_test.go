// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package report_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/flaky/report"
)

// fakeGH returns a GH whose gh is a copy of testdata/gh/gh, which prints the
// files given, and the directory it keeps its state in.
func fakeGH(t *testing.T, files map[string]string) (report.GH, string) {
	dir := t.TempDir()
	script, err := os.ReadFile(filepath.Join("testdata", "gh", "gh"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gh"), script, 0o755))
	for name, contents := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644))
	}
	path, err := bedrock.OSEnvironment{}.Value("PATH")
	require.NoError(t, err)
	path = dir + string(os.PathListSeparator) + path
	// gh is looked up on the process's PATH, and runs with Env's.
	t.Setenv("PATH", path)
	return report.GH{Dir: t.TempDir(), Env: []string{"PATH=" + path}}, dir
}

// calls returns the arguments of each call the fake gh in dir got.
func calls(t *testing.T, dir string) [][]string {
	entries, err := os.ReadDir(filepath.Join(dir, "calls"))
	require.NoError(t, err)
	out := make([][]string, len(entries))
	for i := range entries {
		b, err := os.ReadFile(filepath.Join(dir, "calls", strconv.Itoa(i)))
		require.NoError(t, err)
		out[i] = strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	}
	return out
}

const repoJSON = `{"nameWithOwner":"z5labs/humus","url":"https://github.com/z5labs/humus"}`

func TestGH_Search_finds_the_Bugs_discussions_and_stories_that_may_name_the_test(t *testing.T) {
	gh, dir := fakeGH(t, map[string]string{
		"repo.json": repoJSON,
		"search.json": `{"data":{
		  "discussions":{"nodes":[
		    {"title":"a: TestX fails","body":"b1","url":"https://github.com/z5labs/humus/discussions/1","closed":false,"createdAt":"2026-10-01T00:00:00Z","category":{"name":"Bugs"}},
		    {"title":"Proposal","body":"TestX","url":"https://github.com/z5labs/humus/discussions/2","closed":false,"createdAt":"2026-10-02T00:00:00Z","category":{"name":"General"}},
		    {"title":"old","body":"TestX","url":"https://github.com/z5labs/humus/discussions/3","closed":true,"createdAt":"2026-09-01T00:00:00Z","category":{"name":"Bugs"}},
		    {}
		  ]},
		  "stories":{"nodes":[
		    {"title":"story(a): fix TestX","body":"b4","url":"https://github.com/z5labs/humus/issues/4","state":"OPEN","createdAt":"2026-10-03T00:00:00Z"},
		    {"title":"story(a): fixed","body":"b5","url":"https://github.com/z5labs/humus/issues/5","state":"CLOSED","createdAt":"2026-10-04T00:00:00Z"},
		    {}
		  ]}
		}}`,
	})
	found, err := gh.Search(t.Context(), `TestX/a "quoted" \case`)
	require.NoError(t, err)
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		require.NoError(t, err)
		return v
	}
	require.Equal(t, report.Found{
		Repository: "https://github.com/z5labs/humus",
		Reports: []report.Earlier{
			{Title: "a: TestX fails", Body: "b1", URL: "https://github.com/z5labs/humus/discussions/1", Open: true, Created: at("2026-10-01T00:00:00Z")},
			{Title: "old", Body: "TestX", URL: "https://github.com/z5labs/humus/discussions/3", Open: false, Created: at("2026-09-01T00:00:00Z")},
			{Title: "story(a): fix TestX", Body: "b4", URL: "https://github.com/z5labs/humus/issues/4", Open: true, Created: at("2026-10-03T00:00:00Z")},
			{Title: "story(a): fixed", Body: "b5", URL: "https://github.com/z5labs/humus/issues/5", Open: false, Created: at("2026-10-04T00:00:00Z")},
		},
	}, found, "a discussion outside Bugs is left out")

	got := calls(t, dir)
	require.Len(t, got, 2)
	require.Equal(t, []string{"repo", "view", "--json", "nameWithOwner,url"}, got[0])
	require.Equal(t, []string{"api", "graphql"}, got[1][:2])
	require.Contains(t, got[1], `discussions=repo:z5labs/humus "TestX/a  quoted   case"`, "a quote or backslash would end the phrase, so it is searched for as a space")
	require.Contains(t, got[1], `stories=repo:z5labs/humus is:issue label:story "TestX/a  quoted   case"`)
}

func TestGH_Create_creates_the_discussion_in_the_Bugs_category(t *testing.T) {
	gh, dir := fakeGH(t, map[string]string{
		"repo.json":       repoJSON,
		"categories.json": `{"data":{"repository":{"id":"R_1","discussionCategories":{"nodes":[{"id":"C_general","name":"General"},{"id":"C_bugs","name":"Bugs"}]}}}}`,
		"create.json":     `{"data":{"createDiscussion":{"discussion":{"url":"https://github.com/z5labs/humus/discussions/2000"}}}}`,
	})
	url, err := gh.Create(t.Context(), "a: TestX fails intermittently", "## What happens\n\n@everyone `x`\n")
	require.NoError(t, err)
	require.Equal(t, "https://github.com/z5labs/humus/discussions/2000", url)

	got := calls(t, dir)
	require.Len(t, got, 3)
	require.Contains(t, got[1], "owner=z5labs")
	require.Contains(t, got[1], "name=humus")
	require.Contains(t, got[2], "repository=R_1")
	require.Contains(t, got[2], "category=C_bugs")
	require.Contains(t, got[2], "title=a: TestX fails intermittently")
	require.Contains(t, got[2], "body=## What happens\n\n@everyone `x`\n", "the body is a raw field, so gh reads no file for an @")
}

func TestGH_Create_fails_with_no_Bugs_category(t *testing.T) {
	gh, _ := fakeGH(t, map[string]string{
		"repo.json":       repoJSON,
		"categories.json": `{"data":{"repository":{"id":"R_1","discussionCategories":{"nodes":[{"id":"C_general","name":"General"}]}}}}`,
	})
	_, err := gh.Create(t.Context(), "t", "b")
	require.EqualError(t, err, "z5labs/humus has no discussion category named Bugs")
}

func TestGH_Create_fails_when_no_discussion_comes_back(t *testing.T) {
	gh, _ := fakeGH(t, map[string]string{
		"repo.json":       repoJSON,
		"categories.json": `{"data":{"repository":{"id":"R_1","discussionCategories":{"nodes":[{"id":"C_bugs","name":"Bugs"}]}}}}`,
		"create.json":     `{"data":{"createDiscussion":null}}`,
	})
	_, err := gh.Create(t.Context(), "t", "b")
	require.EqualError(t, err, "gh api graphql created no discussion")
}

func TestGH_fails_when_gh_fails_or_prints_what_it_shouldn_t(t *testing.T) {
	gh, _ := fakeGH(t, map[string]string{"fail": "HTTP 401: Bad credentials\n"})
	_, err := gh.Search(t.Context(), "TestX")
	require.ErrorContains(t, err, "gh repo: exit status 1: HTTP 401: Bad credentials")
	_, err = gh.Create(t.Context(), "t", "b")
	require.ErrorContains(t, err, "HTTP 401: Bad credentials")

	gh, _ = fakeGH(t, map[string]string{"repo.json": `{}`})
	_, err = gh.Search(t.Context(), "TestX")
	require.EqualError(t, err, "gh repo view named no repository")

	gh, _ = fakeGH(t, map[string]string{"repo.json": repoJSON, "search.json": "not JSON"})
	_, err = gh.Search(t.Context(), "TestX")
	require.ErrorContains(t, err, "gh api printed what isn't JSON")

	gh, _ = fakeGH(t, map[string]string{"repo.json": repoJSON, "categories.json": "{"})
	_, err = gh.Create(t.Context(), "t", "b")
	require.ErrorContains(t, err, "gh api printed what isn't JSON")
}
