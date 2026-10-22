// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package report renders a flaky test's Bugs discussion from a fixed template,
// with the test's results from flaky stress -json, and posts it, unless an
// open report already names the test.
//
// The agent that calls it writes only a description, and any notes, to files.
// Every other part of the report is rendered from the results, so every report
// has the same title, sections and tables.
package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/z5labs/humus/tools/flaky/results"
)

// Request is what flaky report is asked to do.
type Request struct {
	// Results is the file flaky stress -json wrote.
	Results string
	// Package is the test's package: its directory, relative to the
	// repository's root, as the results give it, or its import path.
	Package string
	// Test is the test's full name, such as TestX or TestX/a_case.
	Test string
	// Description is the file holding the description, and Notes the file
	// holding the notes, or "" for none. Both are Markdown with no headings.
	Description string
	Notes       string
	// DryRun writes the title and body rather than creating the
	// discussion.
	DryRun bool
	// Root is the repository's root, where the test's function is found at
	// the results' commit, and Env the environment git runs with there.
	Root string
	Env  []string
}

// GitHub is the two calls flaky report makes to GitHub.
type GitHub interface {
	// Search returns the repository's URL, and the reports in it whose
	// title or body may name test: its Bugs discussions and its stories,
	// open and closed. A report it returns may not name test after all.
	Search(ctx context.Context, test string) (Found, error)
	// Create creates a discussion in the repository's category named Bugs,
	// with title and body, and returns its URL.
	Create(ctx context.Context, title, body string) (string, error)
}

// Found is what a search found.
type Found struct {
	// Repository is the repository's URL, such as
	// https://github.com/z5labs/humus.
	Repository string
	// Reports are its Bugs discussions and stories that may name the test.
	Reports []Earlier
}

// Earlier is a report on GitHub: a Bugs discussion or a story.
type Earlier struct {
	Title string
	Body  string
	URL   string
	// Open reports that the discussion or the story is open.
	Open bool
	// Created is when it was created.
	Created time.Time
}

// OpenError is the error File returns when an open report already names the
// test, so nothing is posted.
type OpenError struct {
	Test   string
	Report Earlier
}

func (e *OpenError) Error() string {
	return fmt.Sprintf("an open report already names %s: %s", e.Test, e.Report.URL)
}

// File checks req, renders the test's report and creates it with gh, then
// writes its URL to w. With req.DryRun, it writes the title and the body to w
// instead, and creates nothing.
//
// It posts nothing, and returns an error saying why, when the description or
// the notes is empty or has a heading, when the results don't have the test
// failing or hanging, when the test failed every run at some pair of CPU count
// and load, or when an open report already names the test. That last error is
// an *OpenError. A closed report that names the test is linked under
// References.
func File(ctx context.Context, gh GitHub, req Request, w io.Writer) error {
	description, err := readText("-description", req.Description)
	if err != nil {
		return err
	}
	var notes string
	if req.Notes != "" {
		if notes, err = readText("-notes", req.Notes); err != nil {
			return err
		}
	}
	rep, err := readResults(req.Results)
	if err != nil {
		return err
	}
	t, err := Find(rep, req.Package, req.Test)
	if err != nil {
		return err
	}
	file, line, err := Function(ctx, req.Root, rep.Commit, t, req.Env)
	if err != nil {
		return err
	}
	found, err := gh.Search(ctx, t.Test)
	if err != nil {
		return fmt.Errorf("searching for an earlier report of %s: %w", t.Test, err)
	}
	open, closed := Reports(found.Reports, t.Test)
	if open != nil {
		return &OpenError{Test: t.Test, Report: *open}
	}
	repo := strings.TrimSuffix(found.Repository, "/")
	title, body := Render(Bug{
		Test:        t,
		Commit:      rep.Commit,
		Go:          rep.Go,
		Cores:       rep.Cores,
		Repository:  repo,
		Function:    fmt.Sprintf("%s/blob/%s/%s#L%d", repo, rep.Commit, file, line),
		Description: description,
		Notes:       notes,
		Closed:      closed,
	})
	if req.DryRun {
		_, err := fmt.Fprintf(w, "Title: %s\n\n%s", title, body)
		return err
	}
	url, err := gh.Create(ctx, title, body)
	if err != nil {
		return fmt.Errorf("creating the discussion: %w", err)
	}
	_, err = fmt.Fprintln(w, url)
	return err
}

func readText(flag, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%s is required", flag)
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", flag, err)
	}
	if err := CheckText(string(b)); err != nil {
		return "", fmt.Errorf("%s %s: %w", flag, name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func readResults(name string) (results.Report, error) {
	var rep results.Report
	if name == "" {
		return rep, errors.New("-results is required")
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return rep, fmt.Errorf("-results: %w", err)
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return rep, fmt.Errorf("-results %s isn't the output of flaky stress -json: %w", name, err)
	}
	if rep.Commit == "" {
		return rep, fmt.Errorf("-results %s gives no commit, so it isn't the output of flaky stress -json", name)
	}
	return rep, nil
}

var (
	atxHeading   = regexp.MustCompile(`^ {0,3}#{1,6}(?:[ \t]|$)`)
	setextLine   = regexp.MustCompile(`^ {0,3}(?:=+|-+)[ \t]*$`)
	htmlHeading  = regexp.MustCompile(`(?i)<h[1-6](?:[\s>/]|$)`)
	fenceOpen    = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	notParagraph = regexp.MustCompile(`^ {0,3}(?:[-+*]|\d{1,9}[.)])(?:[ \t]|$)|^ {0,3}>`)
	identifier   = regexp.MustCompile(`[A-Za-z0-9_]`)
)

// CheckText returns an error when text, a description or notes, is empty or
// has a heading: an ATX heading, such as ## Why, a setext heading, a line
// underlined with = or -, or an HTML one. A line inside a fenced code block is
// code, not a heading.
func CheckText(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("it is empty")
	}
	var fence string
	prev := ""
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if fence != "" {
			if strings.HasPrefix(strings.TrimLeft(line, " "), fence) && strings.Trim(strings.TrimSpace(line), fence[:1]) == "" {
				fence = ""
			}
			prev = ""
			continue
		}
		if m := fenceOpen.FindStringSubmatch(line); m != nil {
			fence = m[1]
			prev = ""
			continue
		}
		switch {
		case atxHeading.MatchString(line):
			return fmt.Errorf("line %d is a heading, and a report's headings are its own: %q", i+1, line)
		case setextLine.MatchString(line) && strings.TrimSpace(prev) != "" && !notParagraph.MatchString(prev):
			return fmt.Errorf("line %d underlines line %d as a heading, and a report's headings are its own: %q", i+1, i, line)
		case htmlHeading.MatchString(line):
			return fmt.Errorf("line %d has an HTML heading, and a report's headings are its own: %q", i+1, line)
		}
		prev = line
	}
	return nil
}

// Find returns the test named test, of the package pkg, from rep. pkg is the
// package's directory, relative to the repository's root, as the results give
// it, or its import path.
//
// It returns an error when the results don't have the package, when the test
// didn't fail or hang in any of their runs, when it passed every one of its
// confirm runs, or when it failed every run at some pair of CPU count and
// load, since that test is failing rather than flaky.
func Find(rep results.Report, pkg, test string) (results.Test, error) {
	if test == "" {
		return results.Test{}, errors.New("-test is required")
	}
	if pkg == "" {
		return results.Test{}, errors.New("-package is required")
	}
	dir := path.Clean(strings.TrimPrefix(pkg, "./"))
	is := func(dirOf, importPath string) bool { return dirOf == dir || importPath == pkg }
	known := false
	for _, p := range rep.Packages {
		if is(p.Dir, p.ImportPath) {
			known = true
		}
	}
	if !known {
		return results.Test{}, fmt.Errorf("the results don't have the package %s", pkg)
	}
	for _, t := range rep.Failing {
		if is(t.Package, t.ImportPath) && t.Test == test {
			at := slices.IndexFunc(t.Pairs, func(p results.Pair) bool { return p.Runs > 0 && p.Failures+p.Hangs == p.Runs })
			where := ""
			if at >= 0 {
				where = fmt.Sprintf(" at cpu %d load %d", t.Pairs[at].CPU, t.Pairs[at].Load)
			}
			return t, fmt.Errorf("%s failed every run%s, so it is failing rather than flaky", test, where)
		}
	}
	for _, t := range rep.Flaky {
		if !is(t.Package, t.ImportPath) || t.Test != test {
			continue
		}
		if t.Failures+t.Hangs == 0 {
			return t, fmt.Errorf("%s passed every one of its %s alone, so the results have no failure of it to report", test, plural(t.Runs, "confirm run", "confirm runs"))
		}
		return t, nil
	}
	return results.Test{}, fmt.Errorf("the results don't have %s failing or hanging in %s: it passed every run, or didn't run", test, pkg)
}

// Function returns the file, relative to root, and the line of the function
// of t's top-level test at commit, which it finds with git grep among the
// test files of t's package.
func Function(ctx context.Context, root, commit string, t results.Test, env []string) (string, int, error) {
	top, _, _ := strings.Cut(t.Test, "/")
	files := ":(glob)*_test.go"
	if t.Package != "." {
		files = ":(glob)" + t.Package + "/*_test.go"
	}
	out, err := git(ctx, root, env, "grep", "-n", "-E", "^func "+regexp.QuoteMeta(top)+`\(`, commit, "--", files)
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return "", 0, fmt.Errorf("there is no func %s in the test files of %s at %s", top, t.Package, commit)
	case err != nil:
		return "", 0, fmt.Errorf("finding the function %s in %s at %s: %w", top, t.Package, commit, err)
	}
	first, _, _ := strings.Cut(out, "\n")
	rest, ok := strings.CutPrefix(first, commit+":")
	file, rest, ok2 := strings.Cut(rest, ":")
	n, _, _ := strings.Cut(rest, ":")
	line, err := strconv.Atoi(n)
	if !ok || !ok2 || err != nil {
		return "", 0, fmt.Errorf("finding the function %s in %s at %s: git grep printed %q", top, t.Package, commit, first)
	}
	return file, line, nil
}

// Reports returns, of reports, the open one that names test, and the closed
// one that last named it, or nil for each when there is none. Of several open
// ones, it returns the earliest.
func Reports(reports []Earlier, test string) (open, closed *Earlier) {
	for i := range reports {
		r := &reports[i]
		if !Names(r.Title, test) && !Names(r.Body, test) {
			continue
		}
		switch {
		case r.Open && (open == nil || r.Created.Before(open.Created)):
			open = r
		case !r.Open && (closed == nil || r.Created.After(closed.Created)):
			closed = r
		}
	}
	return open, closed
}

// Names reports whether text names test: has it, with no letter, digit or
// underscore just before or after it, so TestX isn't named by TestXY. A
// subtest's name, such as TestX/a_case, names TestX too.
func Names(text, test string) bool {
	if test == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(text[i:], test)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(test)
		before := start > 0 && identifier.MatchString(text[start-1:start])
		after := end < len(text) && identifier.MatchString(text[end:end+1])
		if !before && !after {
			return true
		}
		i = start + 1
	}
}

// Bug is what a test's report is rendered from.
type Bug struct {
	// Test is the test, from the results.
	Test results.Test
	// Commit is the commit the results are of, Go the version of the Go
	// they ran with, and Cores the number of cores they ran on.
	Commit string
	Go     string
	Cores  int
	// Repository is the repository's URL, which the commit is linked
	// under.
	Repository string
	// Function is the URL of the test's function at the commit.
	Function string
	// Description and Notes are the agent's, with Notes "" for none.
	Description string
	Notes       string
	// Closed is the closed report that last named the test, or nil.
	Closed *Earlier
}

// Title returns the title of t's report: <package>: <test> fails
// intermittently. The package is its directory, relative to the repository's
// root, or the last element of its import path when it is the root.
func Title(t results.Test) string {
	pkg := t.Package
	if pkg == "." || pkg == "" {
		pkg = path.Base(t.ImportPath)
	}
	return fmt.Sprintf("%s: %s fails intermittently", pkg, t.Test)
}

// Render returns the title and the body of b's report.
func Render(b Bug) (string, string) {
	t := b.Test
	var s strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&s, format, args...) }

	p("## What happens\n\n")
	p("%s\n\n", b.Description)
	short := b.Commit[:min(len(b.Commit), 8)]
	p("At [%s](%s/commit/%s), with %s on %d cores, `flaky stress` ran it %s times:\n\n", short, b.Repository, b.Commit, b.Go, b.Cores, count(t.Runs))
	table(&s, t.Pairs)
	p("\nThat is %s and %s. Its first failure:\n\n", plural(t.Failures, "failure", "failures"), plural(t.Hangs, "hang", "hangs"))
	first := t.FirstFailure
	if len(first) == 0 {
		first = []string{"(it printed nothing)"}
	}
	block(&s, "text", first)
	p("\nIt reproduces with:\n\n")
	block(&s, "sh", []string{t.Reproduce, t.ReproduceGoTest})
	if b.Notes != "" {
		p("\n%s\n", b.Notes)
	}
	p("\n## Why\n\nThis hasn't been triaged.\n")
	p("\n## Expected\n\n`%s` passes on every run, at every CPU count and load.\n", t.Test)
	p("\n## References\n\n")
	p("- [`%s`](%s)\n", t.Test, b.Function)
	if b.Closed != nil {
		p("- %s, the closed report that last named it\n", b.Closed.URL)
	}
	return Title(t), s.String()
}

// table writes the failures and hangs out of the runs at each pair, with a
// row for each CPU count and a column for each load.
func table(s *strings.Builder, pairs []results.Pair) {
	var cpus, loads []int
	cells := map[[2]int]results.Pair{}
	for _, p := range pairs {
		if !slices.Contains(cpus, p.CPU) {
			cpus = append(cpus, p.CPU)
		}
		if !slices.Contains(loads, p.Load) {
			loads = append(loads, p.Load)
		}
		cells[[2]int{p.CPU, p.Load}] = p
	}
	slices.Sort(cpus)
	slices.Sort(loads)
	rows := [][]string{{""}}
	for _, l := range loads {
		rows[0] = append(rows[0], "load "+strconv.Itoa(l))
	}
	for _, c := range cpus {
		row := []string{"cpu " + strconv.Itoa(c)}
		for _, l := range loads {
			cell := "–"
			if p, ok := cells[[2]int{c, l}]; ok {
				cell = count(p.Failures+p.Hangs) + " / " + count(p.Runs)
			}
			row = append(row, cell)
		}
		rows = append(rows, row)
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len([]rune(cell)), 3)
		}
	}
	line := func(cells []string) {
		s.WriteString("|")
		for i, cell := range cells {
			fmt.Fprintf(s, " %s%s |", cell, strings.Repeat(" ", widths[i]-len([]rune(cell))))
		}
		s.WriteString("\n")
	}
	line(rows[0])
	s.WriteString("|")
	for _, w := range widths {
		s.WriteString(strings.Repeat("-", w+2) + "|")
	}
	s.WriteString("\n")
	for _, row := range rows[1:] {
		line(row)
	}
}

// block writes lines in a fenced code block of info, fenced with more
// backticks than any run of them in lines, so no line can close it.
func block(s *strings.Builder, info string, lines []string) {
	longest := 0
	for _, l := range lines {
		run := 0
		for _, r := range l {
			if r == '`' {
				run++
				longest = max(longest, run)
			} else {
				run = 0
			}
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	fmt.Fprintf(s, "%s%s\n%s\n%s\n", fence, info, strings.Join(lines, "\n"), fence)
}

// count writes n with a comma between each group of three digits, such as
// 1,000.
func count(n int) string {
	digits := strconv.Itoa(n)
	if n < 0 {
		return "-" + count(-n)
	}
	var out []string
	for len(digits) > 3 {
		out = append([]string{digits[len(digits)-3:]}, out...)
		digits = digits[:len(digits)-3]
	}
	return strings.Join(append([]string{digits}, out...), ",")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return count(n) + " " + many
}

func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return command(ctx, dir, env, "git", args...)
}
