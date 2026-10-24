// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"go.yaml.in/yaml/v3"
)

// docsContent is the content directory of the docs site, relative to this
// package.
const docsContent = "docs/content"

// walkthroughs is the docs site's Walkthroughs section. The pages of the
// walkthrough of example/<type>/<name> are under walkthroughs/<type>/<name>/,
// so each walkthrough's pages mirror its example's path.
const walkthroughs = docsContent + "/walkthroughs"

// gettingStarted is the docs site's Getting started page, and
// gettingStartedExample the path under example/ of the service it builds.
const (
	gettingStarted        = docsContent + "/getting-started.md"
	gettingStartedExample = "job/hello"
)

// readme is the repository's README. Its service is the Getting started
// page's, so its samples are taken from the same example.
const readme = "README.md"

// TestDocs_code_samples checks that every code sample the docs site quotes is
// taken from the code it documents. A Go sample on any other page must appear,
// line for line, in a _test.go file of this module, so it is taken from a test
// or a tested example. A Go, YAML, JSON, Dockerfile or protobuf sample on a
// page under walkthroughs/<type>/<name>/ must appear, line for line, in a file
// under example/<type>/<name>/, its _test.go files included, so it is taken from the
// walkthrough's own code or tests. The Getting started page follows the same
// rule for the service it builds, under example/job/hello/, and so does the
// README, whose service is the same one. Files under hidden directories and
// node_modules are not read for either rule. Indentation is compared relative
// to the sample, so a sample may quote the body of a function or a nested
// block.
func TestDocs_code_samples(t *testing.T) {
	problems, err := sampleProblems(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestDocs_descriptions checks that every page on the docs site has a
// description in its front matter: /llms.txt describes each page with it, and
// the home page's is the summary at the top of /llms.txt.
func TestDocs_descriptions(t *testing.T) {
	pages := 0
	err := filepath.WalkDir(docsContent, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		pages++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fm, err := frontMatter(string(b))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			return nil
		}
		if strings.TrimSpace(fm.Description) == "" {
			t.Errorf("%s: the page has no description; /llms.txt describes each page with it", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages == 0 {
		t.Fatalf("no pages found under %s", docsContent)
	}
}

func TestFrontMatter(t *testing.T) {
	cases := []struct {
		name    string
		page    string
		want    string
		wantErr bool
	}{
		{"folded", "---\ntitle: T\ndescription: >\n  One\n  two.\n---\n\nBody\n", "One two.", false},
		{"inline", "---\ndescription: One line.\n---\n", "One line.", false},
		{"missing", "---\ntitle: T\n---\n\ndescription: not front matter\n", "", false},
		{"no front matter", "# T\n\ndescription: x\n", "", true},
		{"unclosed", "---\ndescription: x\n", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := frontMatter(c.page)
			if c.wantErr {
				if err == nil {
					t.Fatalf("frontMatter() = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Description != c.want {
				t.Errorf("description = %q, want %q", got.Description, c.want)
			}
		})
	}
}

// pageFrontMatter is the part of a page's YAML front matter the tests read.
type pageFrontMatter struct {
	Description string `yaml:"description"`
}

// frontMatter parses the YAML front matter that opens page between two ---
// lines.
func frontMatter(page string) (pageFrontMatter, error) {
	var fm pageFrontMatter
	rest, ok := strings.CutPrefix(page, "---\n")
	if !ok {
		return fm, errors.New("the page does not open with YAML front matter")
	}
	body, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return fm, errors.New("the page's front matter is not closed by a --- line")
	}
	if err := yaml.Unmarshal([]byte(body), &fm); err != nil {
		return fm, fmt.Errorf("parsing the front matter: %w", err)
	}
	return fm, nil
}

func TestSamples(t *testing.T) {
	page := strings.Join([]string{
		"# Title",
		"```go",
		"\tx := 1",
		"",
		"\ty := 2",
		"```",
		"```sh",
		"go test ./...",
		"```",
		"```",
		"```go",
		"```",
		"```YAML {hl_lines=[1]}",
		"z: 3",
		"```",
	}, "\n")

	got := samples(page)

	if len(got) != 4 {
		t.Fatalf("got %d samples, want 4: %v", len(got), got)
	}
	if got[0].line != 2 || got[0].lang != "go" || strings.Join(got[0].lines, "|") != "x := 1||y := 2" {
		t.Errorf("first sample = %d %q %q, want line 2, go and the dedented lines", got[0].line, got[0].lang, got[0].lines)
	}
	if got[1].line != 7 || got[1].lang != "sh" {
		t.Errorf("second sample = %d %q, want line 7 and sh", got[1].line, got[1].lang)
	}
	if got[2].line != 10 || got[2].lang != "" || strings.Join(got[2].lines, "|") != "```go" {
		t.Errorf("third sample = %d %q %q, want line 10, no language and the fence it quotes", got[2].line, got[2].lang, got[2].lines)
	}
	if got[3].line != 13 || got[3].lang != "yaml" || strings.Join(got[3].lines, "|") != "z: 3" {
		t.Errorf("fourth sample = %d %q %q, want line 13 and yaml", got[3].line, got[3].lang, got[3].lines)
	}
}

func TestSampleProblems(t *testing.T) {
	repo := fstest.MapFS{
		readme:                                       {Data: []byte("# Humus\n")},
		docsContent + "/_index.md":                   {Data: []byte("# Humus\n")},
		"app_test.go":                                {Data: []byte("func TestApp(t *testing.T) {\n\tt.Log(\"ok\")\n}\n")},
		"example/job/billing/main.go":                {Data: []byte("func main() {\n\thumus.Main(app.Init)\n}\n")},
		"example/job/billing/main_test.go":           {Data: []byte("func TestMain(t *testing.T) {\n\tt.Log(\"only in a test\")\n}\n")},
		"example/job/billing/deploy/job.yaml":        {Data: []byte("kind: CronJob\nspec:\n  schedule: \"0 2 * * *\"\n")},
		"example/job/billing/wiremock/pay.json":      {Data: []byte("{\n  \"request\": {\n    \"method\": \"POST\"\n  }\n}\n")},
		"example/job/billing/Dockerfile":             {Data: []byte("FROM gcr.io/distroless/static-debian12:nonroot\nUSER nonroot\n")},
		"example/job/billing/proto/billing.proto":    {Data: []byte("message Invoice {\n  string id = 1;\n}\n")},
		"example/job/hello/hello.go":                 {Data: []byte("func greet() {\n\tsay()\n}\n")},
		"example/job/hello/hello_test.go":            {Data: []byte("func TestGreet(t *testing.T) {\n\tt.Log(\"greeted\")\n}\n")},
		"example/rabbitmq/mailer/main.go":            {Data: []byte("func send() {\n\tdeliver()\n}\n")},
		"example/rabbitmq/mailer/main_test.go":       {Data: []byte("func TestSend(t *testing.T) {\n\tt.Log(\"only in the mailer's test\")\n}\n")},
		"example/rabbitmq/mailer/deploy/queue.yaml":  {Data: []byte("kind: Deployment\n")},
		"example/rabbitmq/mailer/wiremock/send.json": {Data: []byte("{\n  \"status\": 202\n}\n")},
		"example/rabbitmq/mailer/Dockerfile":         {Data: []byte("ENTRYPOINT [\"/mailer\"]\n")},
		"example/rabbitmq/mailer/proto/mail.proto":   {Data: []byte("message Mail {}\n")},
	}

	const (
		step    = walkthroughs + "/job/billing/step.md"
		feature = docsContent + "/services/jobs.md"
	)
	cases := []struct {
		name  string
		page  string
		fence string
		lines []string
		fails bool
	}{
		{"go from the walkthrough", step, "go", []string{"humus.Main(app.Init)"}, false},
		{"yaml from the walkthrough", step, "yaml", []string{"spec:", "  schedule: \"0 2 * * *\""}, false},
		{"json from the walkthrough", step, "json", []string{"\"request\": {", "  \"method\": \"POST\"", "}"}, false},
		{"dockerfile from the walkthrough", step, "dockerfile", []string{"USER nonroot"}, false},
		{"protobuf from the walkthrough", step, "protobuf", []string{"message Invoice {", "  string id = 1;", "}"}, false},
		{"yml from the walkthrough", step, "yml", []string{"kind: CronJob"}, false},
		{"proto from the walkthrough", step, "proto", []string{"string id = 1;"}, false},
		{"Dockerfile from the walkthrough", step, "Dockerfile", []string{"USER nonroot"}, false},
		{"go from another walkthrough", step, "go", []string{"deliver()"}, true},
		{"yaml from another walkthrough", step, "yaml", []string{"kind: Deployment"}, true},
		{"json from another walkthrough", step, "json", []string{"\"status\": 202"}, true},
		{"dockerfile from another walkthrough", step, "dockerfile", []string{"ENTRYPOINT [\"/mailer\"]"}, true},
		{"protobuf from another walkthrough", step, "protobuf", []string{"message Mail {}"}, true},
		{"go from the walkthrough's test", step, "go", []string{"t.Log(\"only in a test\")"}, false},
		{"go from another walkthrough's test", step, "go", []string{"t.Log(\"only in the mailer's test\")"}, true},
		{"go from a test outside example", step, "go", []string{"t.Log(\"ok\")"}, true},
		{"sh is unchecked on a walkthrough's page", step, "sh", []string{"./deploy/kind/up.sh"}, false},
		{"go from a test on a feature page", feature, "go", []string{"t.Log(\"ok\")"}, false},
		{"go from a walkthrough's test on a feature page", feature, "go", []string{"t.Log(\"only in a test\")"}, false},
		{"go from a walkthrough on a feature page", feature, "go", []string{"humus.Main(app.Init)"}, true},
		{"yaml is unchecked on a feature page", feature, "yaml", []string{"kind: Pod"}, false},
		{"go from the getting started service", gettingStarted, "go", []string{"say()"}, false},
		{"go from the getting started service's test", gettingStarted, "go", []string{"t.Log(\"greeted\")"}, false},
		{"go from a test outside example on the getting started page", gettingStarted, "go", []string{"t.Log(\"ok\")"}, true},
		{"go from a walkthrough on the getting started page", gettingStarted, "go", []string{"humus.Main(app.Init)"}, true},
		{"sh is unchecked on the getting started page", gettingStarted, "sh", []string{"go run ."}, false},
		{"go from the getting started service on a feature page", feature, "go", []string{"say()"}, true},
		{"go from the getting started service in the README", readme, "go", []string{"say()"}, false},
		{"go from the getting started service's test in the README", readme, "go", []string{"t.Log(\"greeted\")"}, false},
		{"go from a test outside example in the README", readme, "go", []string{"t.Log(\"ok\")"}, true},
		{"go from a walkthrough in the README", readme, "go", []string{"humus.Main(app.Init)"}, true},
		{"sh is unchecked in the README", readme, "sh", []string{"go get github.com/z5labs/humus@main"}, false},
		{"the section's index keeps the feature pages' rule", walkthroughs + "/_index.md", "yaml", []string{"kind: Pod"}, false},
		{"a service type's folder keeps the feature pages' rule", walkthroughs + "/job/_index.md", "yaml", []string{"kind: Pod"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := maps.Clone(repo)
			page := "# Step\n\n```" + c.fence + "\n" + strings.Join(c.lines, "\n") + "\n```\n"
			fsys[c.page] = &fstest.MapFile{Data: []byte(page)}

			problems, err := sampleProblems(fsys)
			if err != nil {
				t.Fatal(err)
			}

			if !c.fails {
				if len(problems) != 0 {
					t.Fatalf("problems = %q, want none", problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %q, want one", problems)
			}
			if want := c.page + ":3:"; !strings.HasPrefix(problems[0], want) {
				t.Errorf("problem = %q, want it to name the page as %q", problems[0], want)
			}
			if want := "\n" + strings.Join(c.lines, "\n"); !strings.HasSuffix(problems[0], want) {
				t.Errorf("problem = %q, want it to end with the sample", problems[0])
			}
		})
	}
}

func TestSampleProblems_missing_walkthrough(t *testing.T) {
	fsys := fstest.MapFS{
		readme:                                {Data: []byte("# Humus\n")},
		walkthroughs + "/job/billing/step.md": {Data: []byte("```go\nfunc main() {}\n```\n")},
	}

	_, err := sampleProblems(fsys)

	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestSampleProblems_no_pages(t *testing.T) {
	fsys := fstest.MapFS{"app_test.go": {Data: []byte("package humus_test\n")}}

	if _, err := sampleProblems(fsys); err == nil {
		t.Error("sampleProblems found no pages and returned no error")
	}
}

func TestQuotedIn(t *testing.T) {
	files := [][]string{
		strings.Split("func f() {\n\tif ok {\n\t\treturn\n\t}\n}", "\n"),
		strings.Split("func g() {\n\tx := 1\n\n\ty := 2\n\tz := 3\n}\n", "\n"),
		strings.Split("\ta()\n\t\tb()\n\tc()", "\n"),
		strings.Split("\t\tp()\n\tq()", "\n"),
		strings.Split("m()\n  \r\nn()", "\n"),
	}

	cases := []struct {
		name   string
		sample []string
		want   bool
	}{
		{"a nested block", []string{"if ok {", "\treturn", "}"}, true},
		{"the first lines of a file", []string{"func f() {", "\tif ok {"}, true},
		{"the last lines of a file", []string{"\t\treturn", "\t}", "}"}, true},
		{"a single line", []string{"return"}, true},
		{"at another relative indentation", []string{"if ok {", "return"}, false},
		{"a line indented deeper relative to the one before it than in the file", []string{"if ok {", "\t\treturn"}, false},
		{"a nested line at the same indentation as the line before it", []string{"a()", "b()", "c()"}, false},
		{"at the file's relative indentation", []string{"a()", "\tb()", "c()"}, true},
		{"a first line more indented than the second, as in the file", []string{"\tp()", "q()"}, true},
		{"a first line less indented than the second, unlike the file", []string{"p()", "\tq()"}, false},
		{"a line that is in no file", []string{"if done {"}, false},
		{"lines in a file but not consecutively", []string{"x := 1", "z := 3"}, false},
		{"lines in a file but out of order", []string{"y := 2", "x := 1"}, false},
		{"lines from two files", []string{"}", "a()"}, false},
		{"a blank line inside", []string{"x := 1", "", "y := 2"}, true},
		{"a blank line the file does not have", []string{"y := 2", "", "z := 3"}, false},
		{"no blank line where the file has one", []string{"x := 1", "y := 2"}, false},
		{"a blank line holding other whitespace", []string{"m()", "", "n()"}, true},
		{"a window that would start before its file", []string{"}", "func f() {"}, false},
		{"a window that would run past its file", []string{"}", "", "x"}, false},
		{"only a blank line", []string{""}, true},
		{"more blank lines than any file has together", []string{"", ""}, false},
	}
	c := newCorpus(files)
	for _, tc := range cases {
		if got := quotedIn(c, tc.sample); got != tc.want {
			t.Errorf("%s: quotedIn(%q) = %v, want %v", tc.name, tc.sample, got, tc.want)
		}
	}
}

// walkthroughLangs names, by the languages their fences give, the samples on a
// walkthrough's pages that must be taken from the walkthrough's files.
var walkthroughLangs = map[string]string{
	"go":         "Go",
	"yaml":       "YAML",
	"yml":        "YAML",
	"json":       "JSON",
	"dockerfile": "Dockerfile",
	"protobuf":   "protobuf",
	"proto":      "protobuf",
}

// sampleProblems checks the samples on every page of the docs site in repo,
// the repository's root, and in its README, and describes each one that is
// not taken from the files its page's rule allows.
func sampleProblems(repo fs.FS) ([]string, error) {
	tests, err := sources(repo, ".", func(p string) bool { return strings.HasSuffix(p, "_test.go") })
	if err != nil {
		return nil, err
	}
	examples := map[string]*corpus{}
	var problems []string
	pages := 0
	check := func(p string) error {
		b, err := fs.ReadFile(repo, p)
		if err != nil {
			return err
		}
		name := exampleName(p)
		for _, s := range samples(string(b)) {
			if name == "" {
				if s.lang == "go" && !quotedIn(tests, s.lines) {
					problems = append(problems, fmt.Sprintf("%s:%d: the Go sample is not taken from any _test.go file:\n%s",
						p, s.line, strings.Join(s.lines, "\n")))
				}
				continue
			}
			lang, ok := walkthroughLangs[s.lang]
			if !ok {
				continue
			}
			src, ok := examples[name]
			if !ok {
				src, err = sources(repo, "example/"+name, func(string) bool { return true })
				if err != nil {
					return fmt.Errorf("%s: reading its example's files: %w", p, err)
				}
				examples[name] = src
			}
			if !quotedIn(src, s.lines) {
				problems = append(problems, fmt.Sprintf("%s:%d: the %s sample is not taken from any file under example/%s:\n%s",
					p, s.line, lang, name, strings.Join(s.lines, "\n")))
			}
		}
		return nil
	}
	err = fs.WalkDir(repo, docsContent, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".md" {
			return err
		}
		pages++
		return check(p)
	})
	if err != nil {
		return nil, err
	}
	if pages == 0 {
		return nil, fmt.Errorf("no pages found under %s", docsContent)
	}
	if err := check(readme); err != nil {
		return nil, err
	}
	return problems, nil
}

// exampleName returns the path, <type>/<name>, under example/ of the example
// whose files the samples on page are taken from: the Getting started page's
// service, which is the README's too, or the walkthrough page's example. It
// returns "" for any other page, whose Go samples are taken from the _test.go
// files of this module.
func exampleName(page string) string {
	if page == gettingStarted || page == readme {
		return gettingStartedExample
	}
	return walkthroughName(page)
}

// walkthroughName returns the path, <type>/<name>, of the walkthrough whose
// pages include page, which is its example's path under example/, or "" if
// page is not one of a walkthrough's pages.
func walkthroughName(page string) string {
	rest, ok := strings.CutPrefix(page, walkthroughs+"/")
	if !ok {
		return ""
	}
	typ, rest, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	name, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return typ + "/" + name
}

// sample is a fenced code block on a page: the line its fence opens on, the
// language its fence gives, in lower case, and its lines with their common
// indentation removed.
type sample struct {
	line  int
	lang  string
	lines []string
}

// samples returns the fenced code blocks in page.
func samples(page string) []sample {
	var samples []sample
	var cur *sample
	for i, l := range strings.Split(page, "\n") {
		trimmed := strings.TrimSpace(l)
		switch {
		case cur == nil && strings.HasPrefix(trimmed, "```"):
			cur = &sample{line: i + 1}
			if fields := strings.Fields(strings.TrimPrefix(trimmed, "```")); len(fields) > 0 {
				cur.lang = strings.ToLower(fields[0])
			}
		case cur != nil && trimmed == "```":
			cur.lines = dedent(cur.lines)
			samples = append(samples, *cur)
			cur = nil
		case cur != nil:
			cur.lines = append(cur.lines, l)
		}
	}
	return samples
}

// sources returns a corpus of the lines of every file under root in repo whose path keep
// accepts, leaving out hidden directories and node_modules.
func sources(repo fs.FS, root string, keep func(path string) bool) (*corpus, error) {
	var sources [][]string
	err := fs.WalkDir(repo, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
			return fs.SkipDir
		}
		if d.IsDir() || !keep(p) {
			return nil
		}
		b, err := fs.ReadFile(repo, p)
		if err != nil {
			return err
		}
		sources = append(sources, strings.Split(string(b), "\n"))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return newCorpus(sources), nil
}

// corpus is the lines of a set of files, indexed by what each non-blank line
// holds after its indentation, so a sample is only compared where it could be
// quoted.
type corpus struct {
	files [][]string
	at    map[string][]linePos
}

// linePos is a line of one of a corpus's files.
type linePos struct {
	file, line int
}

// newCorpus indexes the lines of files.
func newCorpus(files [][]string) *corpus {
	c := &corpus{files: files, at: map[string][]linePos{}}
	for f, lines := range files {
		for i, l := range lines {
			if strings.TrimSpace(l) != "" {
				k := content(l)
				c.at[k] = append(c.at[k], linePos{f, i})
			}
		}
	}
	return c
}

// quotedIn reports whether lines appear, consecutively and with the same
// relative indentation, in one of c's files.
func quotedIn(c *corpus, lines []string) bool {
	anchor := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if anchor < 0 || len(c.at[content(l)]) < len(c.at[content(lines[anchor])]) {
			anchor = i
		}
	}
	if anchor < 0 {
		for _, src := range c.files {
			for i := 0; i+len(lines) <= len(src); i++ {
				if windowQuotes(src[i:i+len(lines)], lines) {
					return true
				}
			}
		}
		return false
	}
	for _, p := range c.at[content(lines[anchor])] {
		src := c.files[p.file]
		i := p.line - anchor
		if i >= 0 && i+len(lines) <= len(src) && windowQuotes(src[i:i+len(lines)], lines) {
			return true
		}
	}
	return false
}

// windowQuotes reports whether window, once dedented, is lines.
func windowQuotes(window, lines []string) bool {
	for i, l := range window {
		windowBlank := strings.TrimSpace(l) == ""
		sampleBlank := strings.TrimSpace(lines[i]) == ""
		if windowBlank != sampleBlank {
			return false
		}
		if !windowBlank && content(l) != content(lines[i]) {
			return false
		}
	}
	return slices.Equal(dedent(window), lines)
}

// content returns l without its indentation.
func content(l string) string {
	return strings.TrimLeft(l, " \t")
}

// dedent removes the leading whitespace every non-blank line of lines shares,
// and empties blank lines.
func dedent(lines []string) []string {
	prefix := ""
	first := true
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		indent := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if first {
			prefix, first = indent, false
			continue
		}
		for !strings.HasPrefix(indent, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			out[i] = strings.TrimPrefix(l, prefix)
		}
	}
	return out
}

// TestDocs_base_images checks that no page of the docs site names a tag or a
// digest that a FROM line of an example's Dockerfile pins. Renovate moves
// those pins and leaves docs/** alone, so a page links to its example's
// Dockerfile for the exact images instead.
func TestDocs_base_images(t *testing.T) {
	problems, err := pinProblems(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestDocs_code_samples_after_a_base_image_bump checks that every sample
// TestDocs_code_samples checks is still taken from its code once the tag, or
// the digest, on every FROM line of every example's Dockerfile has moved, and
// nothing else has, as a Renovate pull request moves them.
func TestDocs_code_samples_after_a_base_image_bump(t *testing.T) {
	for _, name := range slices.Sorted(maps.Keys(baseImageBumps)) {
		t.Run(name, func(t *testing.T) {
			repo, n, err := bumpBaseImages(os.DirFS("."), baseImageBumps[name])
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Skipf("no FROM line under example/ pins a %s", name)
			}

			problems, err := sampleProblems(repo)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range problems {
				t.Error(p)
			}
		})
	}
}

func TestFromImage(t *testing.T) {
	cases := []struct {
		line string
		want baseImage
		ok   bool
	}{
		{"FROM docker.io/library/golang:1.27.3-trixie@sha256:ab12 AS build", baseImage{"docker.io/library/golang", "1.27.3-trixie", "sha256:ab12"}, true},
		{"FROM gcr.io/distroless/static-debian13:nonroot", baseImage{"gcr.io/distroless/static-debian13", "nonroot", ""}, true},
		{"from --platform=$BUILDPLATFORM golang:1.27", baseImage{"golang", "1.27", ""}, true},
		{"FROM localhost:5000/app", baseImage{"localhost:5000/app", "", ""}, true},
		{"FROM localhost:5000/app:v1@sha256:cd34", baseImage{"localhost:5000/app", "v1", "sha256:cd34"}, true},
		{"FROM alpine@sha256:ef56", baseImage{"alpine", "", "sha256:ef56"}, true},
		{"  FROM build", baseImage{"build", "", ""}, true},
		{"RUN echo FROM alpine:3", baseImage{}, false},
		{"FROM", baseImage{}, false},
		{"", baseImage{}, false},
	}
	for _, c := range cases {
		got, ok := fromImage(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("fromImage(%q) = %+v, %v, want %+v, %v", c.line, got, ok, c.want, c.ok)
		}
		if ok && !strings.Contains(c.line, got.String()) {
			t.Errorf("fromImage(%q).String() = %q, want the reference as the line writes it", c.line, got.String())
		}
	}
}

func TestPinProblems(t *testing.T) {
	const page = docsContent + "/walkthroughs/job/billing/deploying.md"
	repo := fstest.MapFS{
		"example/job/billing/Dockerfile": {Data: []byte(strings.Join([]string{
			"FROM docker.io/library/golang:1.27.3-trixie@sha256:ab12 AS build",
			"FROM gcr.io/distroless/static-debian13:nonroot@sha256:cd34",
			"FROM build",
			"USER 65532:65532",
		}, "\n"))},
		"example/job/billing/.hidden/Dockerfile": {Data: []byte("FROM alpine:3.22\n")},
	}

	cases := []struct {
		name string
		text string
		line int
		want []string
	}{
		{"a link to the Dockerfile", "[`Dockerfile`](https://github.com/z5labs/humus/blob/main/example/job/billing/Dockerfile) names the images. It needs Go 1.27.", 3, nil},
		{"the golang tag", "It builds on golang:1.27.3-trixie.", 3, []string{"golang:1.27.3-trixie"}},
		{"the golang tag in full", "It builds on docker.io/library/golang:1.27.3-trixie.", 3, []string{"golang:1.27.3-trixie"}},
		{"the distroless tag", "It runs on `static-debian13:nonroot`.", 3, []string{"static-debian13:nonroot"}},
		{"a digest", "pinned to sha256:ab12", 3, []string{"sha256:ab12"}},
		{"a tag the Dockerfile does not pin", "It would build on golang:1.28.", 3, nil},
		{"a stage's name", "The build stage builds it.", 3, nil},
		{"a tag only a hidden directory's Dockerfile pins", "alpine:3.22", 3, nil},
		{"a quoted FROM line", "```dockerfile\nFROM gcr.io/distroless/static-debian13:nonroot@sha256:cd34\n```", 4, []string{"sha256:cd34", "static-debian13:nonroot"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := maps.Clone(repo)
			fsys[page] = &fstest.MapFile{Data: []byte("# Deploying\n\n" + c.text + "\n")}

			problems, err := pinProblems(fsys)
			if err != nil {
				t.Fatal(err)
			}

			if len(problems) != len(c.want) {
				t.Fatalf("problems = %q, want %d", problems, len(c.want))
			}
			for i, pin := range c.want {
				at := fmt.Sprintf("%s:%d: ", page, c.line)
				if !strings.HasPrefix(problems[i], at) || !strings.Contains(problems[i], " "+pin+",") {
					t.Errorf("problem = %q, want it to name %s at %s", problems[i], pin, at)
				}
			}
		})
	}
}

func TestPinProblems_no_dockerfiles(t *testing.T) {
	fsys := fstest.MapFS{docsContent + "/_index.md": {Data: []byte("# Humus\n")}}

	if _, err := pinProblems(fsys); err == nil {
		t.Error("pinProblems found no Dockerfile and returned no error")
	}
}

func TestBumpBaseImages(t *testing.T) {
	const dockerfile = "example/job/billing/Dockerfile"
	lines := []string{
		"FROM docker.io/library/golang:1.27.3-trixie@sha256:ab12 AS build",
		"RUN go build -o /out/billing .",
		"",
		"FROM build",
		"USER 65532:65532",
	}
	quote := func(lines ...string) []byte {
		return []byte("# Deploying\n\n```dockerfile\n" + strings.Join(lines, "\n") + "\n```\n")
	}
	repo := fstest.MapFS{
		readme:                     {Data: []byte("# Humus\n")},
		docsContent + "/_index.md": {Data: []byte("# Humus\n")},
		dockerfile:                 {Data: []byte(strings.Join(lines, "\n"))},
	}

	cases := []struct {
		name     string
		quote    []string
		problems int
	}{
		{"a quoted FROM line moves", lines[:2], 1},
		{"lines after a FROM line stay", lines[1:], 0},
	}
	want := map[string]string{
		"tag":    "FROM docker.io/library/golang:1.27.3-trixie-next@sha256:ab12 AS build",
		"digest": fmt.Sprintf("FROM docker.io/library/golang:1.27.3-trixie@sha256:%x AS build", sha256.Sum256([]byte("sha256:ab12"))),
	}
	for _, name := range slices.Sorted(maps.Keys(baseImageBumps)) {
		for _, c := range cases {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				fsys := maps.Clone(repo)
				fsys[walkthroughs+"/job/billing/deploying.md"] = &fstest.MapFile{Data: quote(c.quote...)}
				if problems, err := sampleProblems(fsys); err != nil || len(problems) != 0 {
					t.Fatalf("before the bump: problems = %q, err = %v, want neither", problems, err)
				}

				bumped, n, err := bumpBaseImages(fsys, baseImageBumps[name])
				if err != nil {
					t.Fatal(err)
				}

				if n != 1 {
					t.Errorf("bumped %d FROM lines, want 1", n)
				}
				b, err := fs.ReadFile(bumped, dockerfile)
				if err != nil {
					t.Fatal(err)
				}
				if got, wantLines := strings.Split(string(b), "\n"), append([]string{want[name]}, lines[1:]...); !slices.Equal(got, wantLines) {
					t.Errorf("bumped Dockerfile = %q, want %q", got, wantLines)
				}
				problems, err := sampleProblems(bumped)
				if err != nil {
					t.Fatal(err)
				}
				if len(problems) != c.problems {
					t.Errorf("after the bump: problems = %q, want %d", problems, c.problems)
				}
			})
		}
	}
}

// baseImageBumps moves, by what it moves, the tag or the digest a FROM line
// pins, as a Renovate pull request moves them, and leaves the image alone if
// it pins none.
var baseImageBumps = map[string]func(baseImage) baseImage{
	"tag": func(img baseImage) baseImage {
		if img.tag != "" {
			img.tag += "-next"
		}
		return img
	},
	"digest": func(img baseImage) baseImage {
		if img.digest != "" {
			img.digest = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(img.digest)))
		}
		return img
	},
}

// baseImage is the image a FROM line of a Dockerfile names: its name, and the
// tag and the digest it pins, either of which may be empty.
type baseImage struct {
	name, tag, digest string
}

// String returns img as a FROM line writes it.
func (img baseImage) String() string {
	s := img.name
	if img.tag != "" {
		s += ":" + img.tag
	}
	if img.digest != "" {
		s += "@" + img.digest
	}
	return s
}

// pins returns the tag and the digest img pins as a page would name them:
// the tag after the last element of the image's name, as in
// golang:1.27.3-trixie, and the digest whole.
func (img baseImage) pins() []string {
	var pins []string
	if img.tag != "" {
		pins = append(pins, path.Base(img.name)+":"+img.tag)
	}
	if img.digest != "" {
		pins = append(pins, img.digest)
	}
	return pins
}

// fromImage returns the image line names, if line is a FROM instruction.
func fromImage(line string) (baseImage, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "FROM") {
		return baseImage{}, false
	}
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "--") {
			continue
		}
		var img baseImage
		ref, digest, _ := strings.Cut(f, "@")
		img.digest = digest
		if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
			ref, img.tag = ref[:i], ref[i+1:]
		}
		img.name = ref
		return img, true
	}
	return baseImage{}, false
}

// exampleDockerfiles returns the lines of every Dockerfile under example/ in
// repo, by path, leaving out hidden directories and node_modules.
func exampleDockerfiles(repo fs.FS) (map[string][]string, error) {
	files := map[string][]string{}
	err := fs.WalkDir(repo, "example", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != "example" && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
			return fs.SkipDir
		}
		if d.IsDir() || d.Name() != "Dockerfile" {
			return nil
		}
		b, err := fs.ReadFile(repo, p)
		if err != nil {
			return err
		}
		files[p] = strings.Split(string(b), "\n")
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("no Dockerfile found under example/")
	}
	return files, nil
}

// pinProblems describes each line of a page of the docs site in repo that
// names a tag or a digest a FROM line of an example's Dockerfile pins.
func pinProblems(repo fs.FS) ([]string, error) {
	dockerfiles, err := exampleDockerfiles(repo)
	if err != nil {
		return nil, err
	}
	pinnedBy := map[string][]string{}
	for _, p := range slices.Sorted(maps.Keys(dockerfiles)) {
		for _, l := range dockerfiles[p] {
			if img, ok := fromImage(l); ok {
				for _, pin := range img.pins() {
					if !slices.Contains(pinnedBy[pin], p) {
						pinnedBy[pin] = append(pinnedBy[pin], p)
					}
				}
			}
		}
	}
	pins := slices.Sorted(maps.Keys(pinnedBy))
	var problems []string
	err = fs.WalkDir(repo, docsContent, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".md" {
			return err
		}
		b, err := fs.ReadFile(repo, p)
		if err != nil {
			return err
		}
		for i, l := range strings.Split(string(b), "\n") {
			for _, pin := range pins {
				if strings.Contains(l, pin) {
					problems = append(problems, fmt.Sprintf("%s:%d: names %s, pinned by %s; link to the Dockerfile instead",
						p, i+1, pin, strings.Join(pinnedBy[pin], ", ")))
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return problems, nil
}

// bumpBaseImages returns repo with every FROM line of every example's
// Dockerfile moved by bump, and nothing else, and how many lines moved.
func bumpBaseImages(repo fs.FS, bump func(baseImage) baseImage) (fs.FS, int, error) {
	dockerfiles, err := exampleDockerfiles(repo)
	if err != nil {
		return nil, 0, err
	}
	bumped := fstest.MapFS{}
	n := 0
	for p, lines := range dockerfiles {
		lines = slices.Clone(lines)
		for i, l := range lines {
			img, ok := fromImage(l)
			if !ok {
				continue
			}
			if next := bump(img); next != img {
				lines[i] = strings.Replace(l, img.String(), next.String(), 1)
				n++
			}
		}
		bumped[p] = &fstest.MapFile{Data: []byte(strings.Join(lines, "\n"))}
	}
	return overlayFS{repo, bumped}, n, nil
}

// overlayFS reads its files from FS, except those in files.
type overlayFS struct {
	fs.FS
	files fstest.MapFS
}

func (o overlayFS) Open(name string) (fs.File, error) {
	if _, ok := o.files[name]; ok {
		return o.files.Open(name)
	}
	return o.FS.Open(name)
}

// docsSite is the URL the docs site is published at.
const docsSite = "https://z5labs.github.io/humus/"

// TestReadme_links checks that every link in the README to the docs site
// names a page of it, and that every relative link names a file or folder of
// the repository. Other links are not followed.
func TestReadme_links(t *testing.T) {
	b, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	targets := readmeLinks(string(b))
	if len(targets) == 0 {
		t.Fatalf("found no links in %s", readme)
	}
	for _, target := range targets {
		if page, ok := strings.CutPrefix(target, docsSite); ok {
			page = strings.TrimSuffix(page, "/")
			found := false
			for _, f := range []string{page + ".md", path.Join(page, "_index.md")} {
				if _, err := os.Stat(path.Join(docsContent, f)); err == nil {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: %s names no page under %s", readme, target, docsContent)
			}
			continue
		}
		if strings.Contains(target, "://") {
			continue
		}
		if _, err := os.Stat(target); err != nil {
			t.Errorf("%s: %s: %v", readme, target, err)
		}
	}
}

func TestReadmeLinks(t *testing.T) {
	page := strings.Join([]string{
		`<source srcset="docs/a.svg">`,
		`<source srcset="docs/a1.svg 1x, docs/a2.svg 2x">`,
		`<img src="docs/b.svg" alt="">`,
		"[A](https://example.com/a) and [`b`](example/b), [![badge](c.svg)](https://d)",
		"```sh",
		"[not](a/link)",
		"```",
	}, "\n")

	got := readmeLinks(page)

	want := []string{"docs/a.svg", "docs/a1.svg", "docs/a2.svg", "docs/b.svg", "https://example.com/a", "example/b", "c.svg", "https://d"}
	if !slices.Equal(got, want) {
		t.Errorf("readmeLinks() = %q, want %q", got, want)
	}
}

// readmeLinks returns the targets of the Markdown links and images and of the
// src attributes in page, and the URL of each candidate in its srcset
// attributes, outside its fenced code blocks.
func readmeLinks(page string) []string {
	var targets []string
	fenced := false
	for _, l := range strings.Split(page, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		for _, open := range []string{"](", `src="`, `srcset="`} {
			closing := ")"
			if open != "](" {
				closing = `"`
			}
			rest := l
			for {
				_, after, ok := strings.Cut(rest, open)
				if !ok {
					break
				}
				target, after, ok := strings.Cut(after, closing)
				if !ok {
					break
				}
				rest = after
				if open != `srcset="` {
					targets = append(targets, target)
					continue
				}
				for candidate := range strings.SplitSeq(target, ",") {
					if fields := strings.Fields(candidate); len(fields) > 0 {
						targets = append(targets, fields[0])
					}
				}
			}
		}
	}
	return targets
}
