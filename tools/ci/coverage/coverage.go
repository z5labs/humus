// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package coverage gates packages on their statement coverage in a Go cover
// profile.
package coverage

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// Gate reports, on w, the statement coverage in profile of each of
// packages, import paths, in their order, and returns whether every one of
// them has at least min percent. profile is a Go cover profile, which may
// merge the profiles of several modules.
//
// No packages gates nothing and passes, reporting nothing. A package with no
// statements passes. A package with statements but no tests is in the
// profile with every block unexecuted, so it counts as uncovered rather than
// being skipped. Statements are compared exactly, so 79.96% does not pass by
// rounding to 80.0%. A block listed more than once, by more than one test
// binary, counts once, as covered if any listing covered it.
//
// A package below min is reported as a GitHub Actions error annotation.
func Gate(w io.Writer, min int, packages []string, profile io.Reader) (bool, error) {
	stmts, hit, err := blocks(profile)
	if err != nil {
		return false, err
	}
	total := map[string]int{}
	covered := map[string]int{}
	for block, n := range stmts {
		// An import path holds no colon, so the first one ends the file.
		file, _, _ := strings.Cut(block, ":")
		pkg := path.Dir(file)
		total[pkg] += n
		if hit[block] {
			covered[pkg] += n
		}
	}

	passed := true
	for _, pkg := range packages {
		if pkg == "" {
			continue
		}
		t, c := total[pkg], covered[pkg]
		if t == 0 {
			say(w, "%s: no statements\n", pkg)
			continue
		}
		pct := 100 * float64(c) / float64(t)
		if 100*c < min*t {
			say(w, "::error title=Coverage below %d%%::%s: %.1f%% of statements (%d of %d)\n", min, pkg, pct, c, t)
			passed = false
			continue
		}
		say(w, "%s: %.1f%% of statements (%d of %d)\n", pkg, pct, c, t)
	}
	return passed, nil
}

// blocks returns the statements of every block in profile, by its file and
// position, and whether any listing of it covered it.
func blocks(profile io.Reader) (stmts map[string]int, hit map[string]bool, err error) {
	stmts = map[string]int{}
	hit = map[string]bool{}
	s := bufio.NewScanner(profile)
	s.Buffer(nil, 1<<20)
	for n := 1; s.Scan(); n++ {
		line := s.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || !strings.Contains(f[0], ":") {
			return nil, nil, fmt.Errorf("line %d is not a cover profile block: %q", n, line)
		}
		num, err := strconv.Atoi(f[1])
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: statements: %w", n, err)
		}
		count, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: count: %w", n, err)
		}
		stmts[f[0]] = num
		if count > 0 {
			hit[f[0]] = true
		}
	}
	if err := s.Err(); err != nil {
		return nil, nil, err
	}
	return stmts, hit, nil
}

// say writes to w, which reports what the command does; a write that fails
// changes nothing the command does.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
