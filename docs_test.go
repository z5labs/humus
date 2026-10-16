// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docsContent is the content directory of the docs site, relative to this
// package.
const docsContent = "docs/content"

// TestDocs_code_samples checks that every Go code sample on the docs site is
// taken from a test or a tested example: each one must appear, line for line,
// in a _test.go file of this module. Indentation is compared relative to the
// sample, so a sample may quote the body of a function.
func TestDocs_code_samples(t *testing.T) {
	sources := testSources(t)
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
		for _, s := range goSamples(string(b)) {
			if !quotedIn(sources, s.lines) {
				t.Errorf("%s:%d: the Go sample is not taken from any _test.go file:\n%s",
					path, s.line, strings.Join(s.lines, "\n"))
			}
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

func TestGoSamples(t *testing.T) {
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
		"```go",
		"z := 3",
		"```",
	}, "\n")

	got := goSamples(page)

	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2: %v", len(got), got)
	}
	if got[0].line != 2 || strings.Join(got[0].lines, "|") != "x := 1||y := 2" {
		t.Errorf("first sample = %d %q, want line 2 and the dedented lines", got[0].line, got[0].lines)
	}
	if got[1].line != 10 || strings.Join(got[1].lines, "|") != "z := 3" {
		t.Errorf("second sample = %d %q", got[1].line, got[1].lines)
	}
}

func TestQuotedIn(t *testing.T) {
	sources := [][]string{strings.Split("func f() {\n\tif ok {\n\t\treturn\n\t}\n}", "\n")}

	cases := []struct {
		sample []string
		want   bool
	}{
		{[]string{"if ok {", "\treturn", "}"}, true},
		{[]string{"func f() {", "\tif ok {"}, true},
		{[]string{"if ok {", "return"}, false},
		{[]string{"if done {"}, false},
	}
	for _, c := range cases {
		if got := quotedIn(sources, c.sample); got != c.want {
			t.Errorf("quotedIn(%q) = %v, want %v", c.sample, got, c.want)
		}
	}
}

// sample is a Go code sample on a page: the line its fence opens on, and its
// lines with their common indentation removed.
type sample struct {
	line  int
	lines []string
}

// goSamples returns the ```go fenced code blocks in page.
func goSamples(page string) []sample {
	var samples []sample
	var cur *sample
	for i, l := range strings.Split(page, "\n") {
		trimmed := strings.TrimSpace(l)
		switch {
		case cur == nil && trimmed == "```go":
			cur = &sample{line: i + 1}
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

// testSources returns the lines of every _test.go file in this module.
func testSources(t *testing.T) [][]string {
	t.Helper()
	var sources [][]string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sources = append(sources, strings.Split(string(b), "\n"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sources
}

// quotedIn reports whether lines appear, consecutively and with the same
// relative indentation, in one of sources.
func quotedIn(sources [][]string, lines []string) bool {
	want := strings.Join(lines, "\n")
	for _, src := range sources {
		for i := 0; i+len(lines) <= len(src); i++ {
			if strings.Join(dedent(src[i:i+len(lines)]), "\n") == want {
				return true
			}
		}
	}
	return false
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
