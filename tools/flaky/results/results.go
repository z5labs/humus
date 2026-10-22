// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package results counts each test's failures and hangs over a sweep's runs,
// saves its failures' output, and writes the report.
package results

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/z5labs/humus/tools/flaky/discover"
	"github.com/z5labs/humus/tools/flaky/run"
)

// maxLines caps the lines of a first failure kept in the report.
const maxLines = 50

// Config is what a sweep ran.
type Config struct {
	Packages []discover.Package
	CPUs     []int
	Count    int
	Timeout  time.Duration
	// DefaultTimeout is -timeout's default, which the reproducing
	// commands leave out.
	DefaultTimeout time.Duration
	// Out is the directory failures' output is saved under.
	Out string
}

// Results counts a sweep's runs. It is safe for concurrent use.
type Results struct {
	cfg Config

	mu     sync.Mutex
	listed [][]string
	tests  map[key]*entry
}

type key struct {
	pkg  int
	test string
}

type entry struct {
	cpus     map[int]*counts
	first    []string
	firstCPU int
	saved    int
}

type counts struct {
	runs, failures, hangs int
}

// New returns empty results for the sweep cfg.
func New(cfg Config) *Results {
	return &Results{
		cfg:    cfg,
		listed: make([][]string, len(cfg.Packages)),
		tests:  map[key]*entry{},
	}
}

// Listed records the top-level tests that the binary of the package at
// index pkg lists.
func (r *Results) Listed(pkg int, tests []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listed[pkg] = tests
}

// Add counts one run of the package at index pkg at CPU count cpu. It saves
// the run's whole output under the directory of each test that failed or hung
// in it, and of the package when it failed outside any test.
func (r *Results) Add(pkg, cpu int, res run.Result) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range res.Tests {
		if err := r.count(key{pkg, t.Name}, cpu, t.Outcome, t.Output, res); err != nil {
			return err
		}
	}
	outcome := run.Pass
	if res.PackageFailed {
		outcome = run.Fail
	}
	return r.count(key{pkg, ""}, cpu, outcome, res.PackageOutput, res)
}

func (r *Results) count(k key, cpu int, outcome run.Outcome, output []string, res run.Result) error {
	e := r.tests[k]
	if e == nil {
		e = &entry{cpus: map[int]*counts{}}
		r.tests[k] = e
	}
	c := e.cpus[cpu]
	if c == nil {
		c = &counts{}
		e.cpus[cpu] = c
	}
	c.runs++
	switch outcome {
	case run.Fail:
		c.failures++
	case run.Hang:
		c.hangs++
	default:
		return nil
	}
	if e.first == nil {
		e.first = firstLines(outcome, output, res)
		e.firstCPU = cpu
	}
	e.saved++
	dir := filepath.Join(r.cfg.Out, r.dir(k))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("cpu%d-%d.txt", cpu, e.saved)
	return os.WriteFile(filepath.Join(dir, name), res.Output, 0o644)
}

func firstLines(outcome run.Outcome, output []string, res run.Result) []string {
	lines := slices.Clone(output)
	if outcome == run.Hang {
		switch {
		case res.TimedOut:
			lines = append(lines, "panic: test timed out")
		case res.Killed:
			lines = append(lines, "killed: still running after its timeout")
		}
	}
	if len(lines) == 0 && res.Killed {
		lines = append(lines, "killed: still running after its timeout")
	}
	if len(lines) > maxLines {
		lines = append(lines[:maxLines:maxLines], "…")
	}
	return lines
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_.,=+-]`)

// dir returns the directory, relative to Out, that k's failures are saved
// under: the package's directory, then one directory for each level of the
// test's name.
func (r *Results) dir(k key) string {
	parts := []string{r.cfg.Packages[k.pkg].Dir}
	if k.test == "" {
		return path.Join(append(parts, "outside-tests")...)
	}
	for _, level := range strings.Split(k.test, "/") {
		level = unsafeChars.ReplaceAllString(level, "_")
		if level == "" || level == "." || level == ".." {
			level = "_" + level
		}
		parts = append(parts, level)
	}
	return path.Join(parts...)
}

// Report is what a sweep found.
type Report struct {
	// Commit is the commit the sweep ran at.
	Commit string `json:"commit"`
	// Go is the version of the Go it ran with.
	Go string `json:"go"`
	// Cores is the number of cores it ran on.
	Cores int `json:"cores"`
	// CPUs are the CPU counts it ran each package at.
	CPUs []int `json:"cpus"`
	// Count is how many times it ran each package at each CPU count.
	Count int `json:"count"`
	// Packages are the packages it ran.
	Packages []discover.Package `json:"packages"`
	// Output is the directory failures' output was saved under.
	Output string `json:"output"`
	// Flaky are the tests that failed or hung in some runs, but not every
	// run at any CPU count.
	Flaky []Test `json:"flaky"`
	// Failing are the tests that failed or hung every run at some CPU
	// count.
	Failing []Test `json:"failing"`
	// Passed is the number of top-level tests that ran and passed every
	// run, with every subtest.
	Passed int `json:"passed"`
	// Phases are how long each phase took.
	Phases []Phase `json:"phases"`
}

// Failed reports whether any test failed or hung.
func (r Report) Failed() bool {
	return len(r.Flaky) > 0 || len(r.Failing) > 0
}

// Test is a test that failed or hung.
type Test struct {
	// Package is the directory of the test's package, relative to the
	// repository's root.
	Package    string `json:"package"`
	ImportPath string `json:"importPath"`
	// Test is the test's full name, or "" for a failure outside any test.
	Test     string `json:"test"`
	Runs     int    `json:"runs"`
	Failures int    `json:"failures"`
	Hangs    int    `json:"hangs"`
	// CPUs are its runs, failures and hangs at each CPU count.
	CPUs []CPU `json:"cpus"`
	// FirstFailure is its first failure's lines.
	FirstFailure []string `json:"firstFailure"`
	// FirstFailureCPU is the CPU count of its first failure.
	FirstFailureCPU int `json:"firstFailureCPU"`
	// Output is the directory its failures' whole output was saved to.
	Output string `json:"output"`
	// Reproduce is the command that reproduces it with flaky stress.
	Reproduce string `json:"reproduce"`
	// ReproduceGoTest is the command that reproduces it with go test,
	// under taskset.
	ReproduceGoTest string `json:"reproduceGoTest"`
}

// CPU is a test's runs, failures and hangs at one CPU count.
type CPU struct {
	CPU      int `json:"cpu"`
	Runs     int `json:"runs"`
	Failures int `json:"failures"`
	Hangs    int `json:"hangs"`
}

// Phase is how long one phase of a sweep took.
type Phase struct {
	Name    string        `json:"name"`
	Took    time.Duration `json:"-"`
	Seconds float64       `json:"seconds"`
}

// NewPhase returns the phase name, which took took.
func NewPhase(name string, took time.Duration) Phase {
	return Phase{Name: name, Took: took, Seconds: took.Seconds()}
}

// Report returns what the sweep found so far.
func (r *Results) Report() Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := Report{
		CPUs:     r.cfg.CPUs,
		Count:    r.cfg.Count,
		Packages: r.cfg.Packages,
		Output:   r.cfg.Out,
		Flaky:    []Test{},
		Failing:  []Test{},
	}
	bad := map[key]bool{}
	keys := make([]key, 0, len(r.tests))
	for k := range r.tests {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int {
		if a.pkg != b.pkg {
			return a.pkg - b.pkg
		}
		return strings.Compare(a.test, b.test)
	})
	for _, k := range keys {
		t, failing := r.test(k, r.tests[k])
		if t.Failures+t.Hangs == 0 {
			continue
		}
		bad[k] = true
		if failing {
			rep.Failing = append(rep.Failing, t)
		} else {
			rep.Flaky = append(rep.Flaky, t)
		}
	}
	for pkg, listed := range r.listed {
		for _, name := range listed {
			if _, ran := r.tests[key{pkg, name}]; !ran {
				continue
			}
			if !slices.ContainsFunc(keys, func(k key) bool {
				return k.pkg == pkg && bad[k] && (k.test == name || strings.HasPrefix(k.test, name+"/"))
			}) {
				rep.Passed++
			}
		}
	}
	return rep
}

func (r *Results) test(k key, e *entry) (Test, bool) {
	pkg := r.cfg.Packages[k.pkg]
	t := Test{
		Package:         pkg.Dir,
		ImportPath:      pkg.ImportPath,
		Test:            k.test,
		FirstFailure:    e.first,
		FirstFailureCPU: e.firstCPU,
		Output:          filepath.Join(r.cfg.Out, r.dir(k)),
	}
	failing := false
	worst, worstRate := 0, -1.0
	for _, cpu := range r.cfg.CPUs {
		c := e.cpus[cpu]
		if c == nil {
			c = &counts{}
		}
		t.CPUs = append(t.CPUs, CPU{CPU: cpu, Runs: c.runs, Failures: c.failures, Hangs: c.hangs})
		t.Runs += c.runs
		t.Failures += c.failures
		t.Hangs += c.hangs
		if c.runs > 0 && c.failures+c.hangs == c.runs {
			failing = true
		}
		if c.runs > 0 {
			if rate := float64(c.failures+c.hangs) / float64(c.runs); rate > worstRate {
				worst, worstRate = cpu, rate
			}
		}
	}
	t.Reproduce, t.ReproduceGoTest = r.reproduce(pkg, k.test, worst)
	return t, failing
}

// reproduce returns the commands that run test, or the whole package when
// test is "", at CPU count cpu, Count times.
func (r *Results) reproduce(pkg discover.Package, test string, cpu int) (string, string) {
	pattern := RunPattern(test)
	flaky := []string{"flaky", "stress"}
	if pattern != "" {
		flaky = append(flaky, "-run", quote(pattern))
	}
	flaky = append(flaky, "-cpu", strconv.Itoa(cpu), "-count", strconv.Itoa(r.cfg.Count))
	if r.cfg.Timeout != r.cfg.DefaultTimeout {
		flaky = append(flaky, "-timeout", r.cfg.Timeout.String())
	}
	flaky = append(flaky, "./"+pkg.Dir)

	rel := "."
	if pkg.Dir != pkg.Module {
		rel = "./" + strings.TrimPrefix(pkg.Dir, pkg.Module+"/")
		if pkg.Module == "." {
			rel = "./" + pkg.Dir
		}
	}
	cores := "0"
	if cpu > 1 {
		cores = "0-" + strconv.Itoa(cpu-1)
	}
	goTest := []string{"taskset", "-c", cores, "go", "-C", pkg.Module, "test", "-race", "-cover",
		"-count=" + strconv.Itoa(r.cfg.Count), "-cpu", strconv.Itoa(cpu), "-timeout", r.cfg.Timeout.String()}
	if pattern != "" {
		goTest = append(goTest, "-run", quote(pattern))
	}
	goTest = append(goTest, rel)
	return strings.Join(flaky, " "), strings.Join(goTest, " ")
}

// RunPattern returns the -run pattern that matches exactly the test name,
// such as ^TestX$/^a_case$ for TestX/a_case.
func RunPattern(name string) string {
	if name == "" {
		return ""
	}
	levels := strings.Split(name, "/")
	for i, l := range levels {
		levels[i] = "^" + regexp.QuoteMeta(l) + "$"
	}
	return strings.Join(levels, "/")
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./=,+-]+$`)

func quote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WriteJSON writes rep as JSON to w.
func WriteJSON(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// WriteText writes rep for a person to read to w.
func WriteText(w io.Writer, rep Report) error {
	p := &printer{w: w}
	if len(rep.Flaky) > 0 {
		p.printf("FLAKY: failed or hung in some runs\n")
		for _, t := range rep.Flaky {
			p.test(t)
		}
	}
	if len(rep.Failing) > 0 {
		if len(rep.Flaky) > 0 {
			p.printf("\n")
		}
		p.printf("FAILING: failed or hung every run at some CPU count\n")
		for _, t := range rep.Failing {
			p.test(t)
		}
	}
	if rep.Failed() {
		p.printf("\n")
	}
	p.printf("%s passed every run\n", plural(rep.Passed, "test", "tests"))
	phases := make([]string, len(rep.Phases))
	for i, ph := range rep.Phases {
		phases[i] = ph.Name + " " + ph.Took.Round(time.Millisecond).String()
	}
	p.printf("phases: %s\n", strings.Join(phases, ", "))
	return p.err
}

type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

func (p *printer) test(t Test) {
	name := t.Test
	if name == "" {
		name = "(outside any test)"
	}
	p.printf("\n%s  %s\n", t.Package, name)
	p.printf("    %-8s %9s %9s %9s\n", "", "failures", "hangs", "runs")
	for _, c := range t.CPUs {
		p.printf("    %-8s %9d %9d %9d\n", "cpu "+strconv.Itoa(c.CPU), c.Failures, c.Hangs, c.Runs)
	}
	p.printf("    %s, %s in %s\n", plural(t.Failures, "failure", "failures"), plural(t.Hangs, "hang", "hangs"), plural(t.Runs, "run", "runs"))
	p.printf("    first failure, at cpu %d:\n", t.FirstFailureCPU)
	for _, l := range t.FirstFailure {
		p.printf("        %s\n", l)
	}
	p.printf("    output:    %s\n", t.Output)
	p.printf("    reproduce: %s\n", t.Reproduce)
	p.printf("               %s\n", t.ReproduceGoTest)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
