// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package results counts each test's failures and hangs over a sweep's runs,
// and over its confirm runs, saves its failures' output, and writes the
// report.
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
	// Packages are the packages it ran, each at the level of Levels that
	// its Level names.
	Packages []discover.Package
	CPUs     []int
	Loads    []int
	Levels   []Level
	// Walkthroughs are the walkthroughs whose clusters it brought up, which
	// UpFailed names by their index.
	Walkthroughs []discover.Walkthrough
	// Run and Skip are the sweep runs' -run and -skip, when set.
	Run, Skip string
	// Out is the directory failures' output is saved under.
	Out string
}

// Level is how a sweep ran the packages at one level.
type Level struct {
	// Name is the level's name, such as unit or testcontainers.
	Name string `json:"name"`
	// Count is how many times each package ran at each pair of CPU count
	// and load.
	Count int `json:"count"`
	// Confirm is how many times each test that failed or hung in the sweep
	// ran alone at each pair of CPU count and load, or 0 for no confirm
	// runs.
	Confirm int `json:"confirm"`
	// Timeout is each run's -test.timeout.
	Timeout time.Duration `json:"-"`
	// DefaultTimeout is the level's default -timeout, which the
	// reproducing commands leave out.
	DefaultTimeout time.Duration `json:"-"`
	// TimeoutSeconds is Timeout in seconds.
	TimeoutSeconds float64 `json:"timeoutSeconds"`
	// CPU and Load are the one CPU count and load its packages ran at, as
	// a cluster's do, or 0 when they ran at each pair of the sweep's.
	CPU  int `json:"cpu,omitempty"`
	Load int `json:"load,omitempty"`
}

// pairs returns the pairs of CPU count and load that the packages at level ran
// at.
func (r *Results) pairs(level Level) []pair {
	if level.CPU > 0 {
		return []pair{{level.CPU, level.Load}}
	}
	pairs := make([]pair, 0, len(r.cfg.CPUs)*len(r.cfg.Loads))
	for _, cpu := range r.cfg.CPUs {
		for _, load := range r.cfg.Loads {
			pairs = append(pairs, pair{cpu, load})
		}
	}
	return pairs
}

// Results counts a sweep's runs. It is safe for concurrent use.
type Results struct {
	cfg Config

	mu     sync.Mutex
	listed [][]string
	tests  map[key]*entry
	ups    []UpFailure
	// planned and ended are how many runs the sweep planned, and how many
	// of them ended.
	planned, ended int
}

type key struct {
	pkg  int
	test string
}

type entry struct {
	pairs     map[pair]*counts
	first     []string
	firstPair pair
	firstSeed *int64
	saved     int
	// confirm are the test's confirm runs, or nil when it wasn't
	// confirmed.
	confirm      map[pair]*counts
	confirmSaved int
	// confirming reports that the test's confirm runs were planned, and
	// confirmEnded is how many of them ended, counted or not.
	confirming   bool
	confirmEnded int
}

// failed reports whether the test failed or hung in any run.
func (e *entry) failed() bool {
	for _, table := range []map[pair]*counts{e.pairs, e.confirm} {
		for _, c := range table {
			if c.failures+c.hangs > 0 {
				return true
			}
		}
	}
	return false
}

// pair is a CPU count and a load.
type pair struct {
	cpu, load int
}

type counts struct {
	runs, failures, hangs int
	// seed is the -test.shuffle seed of the first run that failed or
	// hung, or nil when that run wasn't shuffled.
	seed *int64
}

// New returns empty results for the sweep cfg.
func New(cfg Config) *Results {
	cfg.Levels = slices.Clone(cfg.Levels)
	for i := range cfg.Levels {
		cfg.Levels[i].TimeoutSeconds = cfg.Levels[i].Timeout.Seconds()
	}
	return &Results{
		cfg:    cfg,
		listed: make([][]string, len(cfg.Packages)),
		tests:  map[key]*entry{},
	}
}

// level returns the level of the package at index pkg.
func (r *Results) level(pkg int) Level {
	name := r.cfg.Packages[pkg].Level
	for _, l := range r.cfg.Levels {
		if l.Name == name {
			return l
		}
	}
	return Level{Name: name}
}

// Listed records the top-level tests that the binary of the package at
// index pkg lists.
func (r *Results) Listed(pkg int, tests []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listed[pkg] = tests
}

// Planned records that n more runs are planned: a pass of the sweep's runs, or
// of its confirm runs.
func (r *Results) Planned(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.planned += n
}

// Confirming records that the confirm runs of the test named test, of the
// package at index pkg, are planned: its level's confirm count at each of its
// pairs of CPU count and load. A test whose confirm runs don't all end is
// reported as such.
func (r *Results) Confirming(pkg int, test string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entry(key{pkg, test}).confirming = true
}

// Add counts one run of the package at index pkg at CPU count cpu and load
// load. It saves the run's whole output under the directory of each test that
// failed or hung in it, and of the package when it failed outside any test.
//
// A cancelled run says nothing about the tests, so it is neither counted nor
// one of the runs that ended.
func (r *Results) Add(pkg, cpu, load int, res run.Result) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if res.Cancelled {
		return nil
	}
	r.ended++
	at := pair{cpu, load}
	for _, t := range res.Tests {
		if err := r.count(key{pkg, t.Name}, at, t.Outcome, t.Output, res); err != nil {
			return err
		}
	}
	outcome := run.Pass
	if res.PackageFailed {
		outcome = run.Fail
	}
	return r.count(key{pkg, ""}, at, outcome, res.PackageOutput, res)
}

func (r *Results) count(k key, at pair, outcome run.Outcome, output []string, res run.Result) error {
	e := r.entry(k)
	if !tally(e.pairs, at, outcome, res.Seed) {
		return nil
	}
	if e.first == nil {
		e.first = firstLines(outcome, output, res)
		e.firstPair = at
		e.firstSeed = res.Seed
	}
	e.saved++
	return r.save(k, fmt.Sprintf("cpu%d-load%d-%d.txt", at.cpu, at.load, e.saved), res.Output)
}

// Confirmed counts one confirm run of the test named test, of the package at
// index pkg, at CPU count cpu and load load: a run of that test alone.
//
// The run fails when the test or one of its subtests failed, or when it
// failed outside any test, and hangs when the test or one of its subtests
// hung. A run in which the test never started, and that didn't fail, says
// nothing about the test, so it isn't counted, though it ended. The whole
// output of a run that failed or hung is saved under the test's directory. A
// cancelled run is neither counted nor one of the runs that ended.
func (r *Results) Confirmed(pkg int, test string, cpu, load int, res run.Result) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if res.Cancelled {
		return nil
	}
	r.ended++
	k := key{pkg, test}
	e := r.entry(k)
	e.confirmEnded++
	started := false
	outcome := run.Pass
	for _, t := range res.Tests {
		if t.Name != test && !strings.HasPrefix(t.Name, test+"/") {
			continue
		}
		started = true
		switch {
		case t.Outcome == run.Hang:
			outcome = run.Hang
		case t.Outcome == run.Fail && outcome != run.Hang:
			outcome = run.Fail
		}
	}
	if outcome == run.Pass && res.PackageFailed {
		outcome = run.Fail
	}
	if !started && outcome == run.Pass {
		return nil
	}
	if e.confirm == nil {
		e.confirm = map[pair]*counts{}
	}
	at := pair{cpu, load}
	if !tally(e.confirm, at, outcome, res.Seed) {
		return nil
	}
	e.confirmSaved++
	return r.save(k, fmt.Sprintf("confirm-cpu%d-load%d-%d.txt", at.cpu, at.load, e.confirmSaved), res.Output)
}

// UpFailed records that the cluster of the walkthrough at index w didn't come
// up, with err, and saves err's whole text under the walkthrough's directory.
func (r *Results) UpFailed(w int, err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	wt := r.cfg.Walkthroughs[w]
	text := err.Error()
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) > maxLines {
		lines = append([]string{"…"}, lines[len(lines)-maxLines:]...)
	}
	dir := filepath.Join(r.cfg.Out, wt.Module, "up")
	file := filepath.Join(dir, fmt.Sprintf("up-%d.txt", len(r.ups)+1))
	r.ups = append(r.ups, UpFailure{Walkthrough: wt.Name, Module: wt.Module, Error: lines, Output: file})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, []byte(text+"\n"), 0o644)
}

func (r *Results) entry(k key) *entry {
	e := r.tests[k]
	if e == nil {
		e = &entry{pairs: map[pair]*counts{}}
		r.tests[k] = e
	}
	return e
}

// tally counts a run at at with outcome and seed in table, and reports
// whether it failed or hung.
func tally(table map[pair]*counts, at pair, outcome run.Outcome, seed *int64) bool {
	c := table[at]
	if c == nil {
		c = &counts{}
		table[at] = c
	}
	c.runs++
	switch outcome {
	case run.Fail:
		c.failures++
	case run.Hang:
		c.hangs++
	default:
		return false
	}
	if c.failures+c.hangs == 1 {
		c.seed = seed
	}
	return true
}

// save writes a run's whole output to the file name under k's directory.
func (r *Results) save(k key, name string, output []byte) error {
	dir := filepath.Join(r.cfg.Out, r.dir(k))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), output, 0o644)
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
	// Loads are the loads it ran each package at.
	Loads []int `json:"loads"`
	// Levels are the levels it ran, and how: how many times it ran each
	// of their packages at each pair of CPU count and load, and how many
	// times it ran each of their tests that failed or hung in the sweep
	// alone, or 0 times when it had no confirm runs, as with -run.
	Levels []Level `json:"levels"`
	// Confirmed is the number of tests it confirmed, and ConfirmRuns the
	// number of their confirm runs, which counts a test whose confirm runs
	// didn't all end when any did. IncompleteConfirms is the number of
	// tests whose confirm runs didn't all end, confirmed or not.
	Confirmed          int `json:"confirmed"`
	ConfirmRuns        int `json:"confirmRuns"`
	IncompleteConfirms int `json:"incompleteConfirms"`
	// Interrupted reports that the sweep was stopped, by SIGINT or SIGTERM,
	// before every run it planned had ended, so the report is of the runs
	// that ended. No confirm run started after the interrupt.
	Interrupted bool `json:"interrupted"`
	// PlannedRuns is how many runs it planned: every sweep run, and once
	// the sweep had ended, every confirm run. EndedRuns is how many of them
	// ended. A run cut short by the interrupt didn't end, nor did a run on a
	// cluster that didn't come up.
	PlannedRuns int `json:"plannedRuns"`
	EndedRuns   int `json:"endedRuns"`
	// Packages are the packages it ran.
	Packages []discover.Package `json:"packages"`
	// Output is the directory failures' output was saved under.
	Output string `json:"output"`
	// Flaky are the tests that failed or hung in some runs, but not every
	// run at any pair of CPU count and load.
	Flaky []Test `json:"flaky"`
	// Failing are the tests that failed or hung every run at some pair of
	// CPU count and load.
	Failing []Test `json:"failing"`
	// UpFailures are the clusters that didn't come up, so their
	// walkthroughs' tests didn't run on them.
	UpFailures []UpFailure `json:"upFailures"`
	// Passed is the number of top-level tests that ran and passed every
	// run, with every subtest.
	Passed int `json:"passed"`
	// Phases are how long each phase took.
	Phases []Phase `json:"phases"`
}

// Failed reports whether any test failed or hung, or any cluster didn't come
// up.
func (r Report) Failed() bool {
	return len(r.Flaky) > 0 || len(r.Failing) > 0 || len(r.UpFailures) > 0
}

// UpFailure is a walkthrough's cluster that didn't come up.
type UpFailure struct {
	// Walkthrough is the walkthrough's name, such as wallet.
	Walkthrough string `json:"walkthrough"`
	// Module is its module's directory, relative to the repository's root.
	Module string `json:"module"`
	// Error is the error's last lines.
	Error []string `json:"error"`
	// Output is the file its whole text was saved to.
	Output string `json:"output"`
}

// Test is a test that failed or hung.
type Test struct {
	// Package is the directory of the test's package, relative to the
	// repository's root.
	Package    string `json:"package"`
	ImportPath string `json:"importPath"`
	// Level is the level the test ran at.
	Level string `json:"level"`
	// Test is the test's full name, or "" for a failure outside any test.
	Test string `json:"test"`
	// Confirmed reports that the test was confirmed, so Runs, Failures,
	// Hangs and Pairs are its confirm runs, not its sweep runs. It is
	// confirmed fully only when ConfirmIncomplete is false too.
	Confirmed bool `json:"confirmed"`
	// ConfirmIncomplete reports that its confirm runs were planned, but
	// didn't all end, as when the sweep was interrupted during them. Then
	// its counts are of the confirm runs that ended, when any did, and of
	// its sweep runs otherwise, so it isn't a fully confirmed test.
	// ConfirmPlanned and ConfirmEnded are how many of its confirm runs were
	// planned and ended, or 0 when none were planned.
	ConfirmIncomplete bool `json:"confirmIncomplete"`
	ConfirmPlanned    int  `json:"confirmPlanned"`
	ConfirmEnded      int  `json:"confirmEnded"`
	Runs              int  `json:"runs"`
	Failures          int  `json:"failures"`
	Hangs             int  `json:"hangs"`
	// Pairs are its runs, failures and hangs at each pair of CPU count
	// and load.
	Pairs []Pair `json:"pairs"`
	// FirstFailure is its first failure's lines.
	FirstFailure []string `json:"firstFailure"`
	// FirstFailureCPU and FirstFailureLoad are the CPU count and load of
	// its first failure.
	FirstFailureCPU  int `json:"firstFailureCPU"`
	FirstFailureLoad int `json:"firstFailureLoad"`
	// FirstFailureSeed is its first failure's -test.shuffle seed, or nil
	// when that run wasn't shuffled.
	FirstFailureSeed *int64 `json:"firstFailureSeed,omitempty"`
	// Output is the directory its failures' whole output was saved to.
	Output string `json:"output"`
	// Reproduce is the command that reproduces it with flaky stress, at
	// the pair where it failed most: in its confirm runs, when it failed
	// in any of them.
	Reproduce string `json:"reproduce"`
	// ReproduceGoTest is the command that reproduces it with go test,
	// under taskset, at the CPU count of that pair. It can't reproduce a
	// load.
	ReproduceGoTest string `json:"reproduceGoTest"`
	// Seed is the -test.shuffle seed that both commands name: that of the
	// first run at that pair that failed or hung. It is nil when that run
	// wasn't shuffled.
	Seed *int64 `json:"seed,omitempty"`
}

// Pair is a test's runs, failures and hangs at one pair of CPU count and
// load.
type Pair struct {
	CPU      int `json:"cpu"`
	Load     int `json:"load"`
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
		CPUs:        r.cfg.CPUs,
		Loads:       r.cfg.Loads,
		Levels:      r.cfg.Levels,
		PlannedRuns: r.planned,
		EndedRuns:   r.ended,
		Packages:    r.cfg.Packages,
		Output:      r.cfg.Out,
		Flaky:       []Test{},
		Failing:     []Test{},
		// The slice is copied, since a cluster may yet fail to come up
		// once the report is returned.
		UpFailures: append([]UpFailure{}, r.ups...),
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
		e := r.tests[k]
		if !e.failed() {
			continue
		}
		t, failing := r.test(k, e)
		if t.Confirmed {
			rep.Confirmed++
			rep.ConfirmRuns += t.Runs
		}
		if t.ConfirmIncomplete {
			rep.IncompleteConfirms++
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
	level := r.level(k.pkg)
	t := Test{
		Package:          pkg.Dir,
		ImportPath:       pkg.ImportPath,
		Level:            level.Name,
		Test:             k.test,
		FirstFailure:     e.first,
		FirstFailureCPU:  e.firstPair.cpu,
		FirstFailureLoad: e.firstPair.load,
		FirstFailureSeed: e.firstSeed,
		Output:           filepath.Join(r.cfg.Out, r.dir(k)),
	}
	table, count := e.pairs, level.Count
	if e.confirm != nil {
		table, count = e.confirm, level.Confirm
		t.Confirmed = true
	}
	if e.confirming {
		t.ConfirmPlanned = level.Confirm * len(r.pairs(level))
		t.ConfirmEnded = e.confirmEnded
		t.ConfirmIncomplete = t.ConfirmEnded < t.ConfirmPlanned
	}
	failing := false
	for _, at := range r.pairs(level) {
		c := table[at]
		if c == nil {
			c = &counts{}
		}
		t.Pairs = append(t.Pairs, Pair{CPU: at.cpu, Load: at.load, Runs: c.runs, Failures: c.failures, Hangs: c.hangs})
		t.Runs += c.runs
		t.Failures += c.failures
		t.Hangs += c.hangs
		if c.runs > 0 && c.failures+c.hangs == c.runs {
			failing = true
		}
	}
	worst, rate := r.worst(level, table)
	sweep := e.confirm == nil
	if rate <= 0 {
		// It passed every confirm run, so its sweep runs say where it
		// fails.
		table, sweep = e.pairs, true
		worst, _ = r.worst(level, table)
	}
	if c := table[worst]; c != nil {
		t.Seed = c.seed
	}
	t.Reproduce, t.ReproduceGoTest = r.reproduce(pkg, level, k.test, worst, count, t.Seed, sweep)
	return t, failing
}

// worst returns the pair of level's at which the runs in table failed or hung
// most often, and that rate, or -1 when table has no runs.
func (r *Results) worst(level Level, table map[pair]*counts) (pair, float64) {
	var worst pair
	worstRate := -1.0
	for _, at := range r.pairs(level) {
		c := table[at]
		if c == nil || c.runs == 0 {
			continue
		}
		if rate := float64(c.failures+c.hangs) / float64(c.runs); rate > worstRate {
			worst, worstRate = at, rate
		}
	}
	return worst, worstRate
}

// reproduce returns the commands that run test, or the whole package when
// test is "", at level and at the pair at, count times. The go test command
// runs at at's CPU count, since it can't reproduce a load, and with the
// level's tag.
//
// With a seed, both commands shuffle with it. A seed reproduces the order of
// the tests that ran, so when it is a sweep run's, from the sweep, they run
// what the sweep run ran, by its -run and -skip, rather than test alone: a
// test that fails only after another passes alone. The flaky command's -run
// is then "." when the sweep had none, so it runs the package count times
// rather than sweeping and confirming.
//
// At a level whose packages run at one pair, as a cluster's do, the flaky
// command gives no -cpu or -load, since its runs ignore them.
func (r *Results) reproduce(pkg discover.Package, level Level, test string, at pair, count int, seed *int64, sweep bool) (string, string) {
	cpu := at.cpu
	pattern, skip := RunPattern(test), ""
	shuffle := ""
	if seed != nil {
		shuffle = "-shuffle=" + strconv.FormatInt(*seed, 10)
		if sweep {
			pattern, skip = r.cfg.Run, r.cfg.Skip
			if pattern == "" {
				pattern = "."
			}
		}
	}
	flaky := []string{"flaky", "stress", "-tags", level.Name}
	if pattern != "" {
		flaky = append(flaky, "-run", quote(pattern))
	}
	if skip != "" {
		flaky = append(flaky, "-skip", quote(skip))
	}
	if shuffle != "" {
		flaky = append(flaky, shuffle)
	}
	if level.CPU == 0 {
		flaky = append(flaky, "-cpu", strconv.Itoa(cpu), "-load", strconv.Itoa(at.load))
	}
	flaky = append(flaky, "-count", strconv.Itoa(count))
	if level.Timeout != level.DefaultTimeout {
		flaky = append(flaky, "-timeout", level.Timeout.String())
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
	goTest := []string{"taskset", "-c", cores, "go", "-C", pkg.Module, "test", "-race", "-cover"}
	if tag := discover.Tag(level.Name); tag != "" {
		goTest = append(goTest, "-tags", tag)
	}
	goTest = append(goTest, "-count="+strconv.Itoa(count), "-cpu", strconv.Itoa(cpu), "-timeout", level.Timeout.String())
	if pattern != "" {
		goTest = append(goTest, "-run", quote(pattern))
	}
	if skip != "" {
		goTest = append(goTest, "-skip", quote(skip))
	}
	if shuffle != "" {
		goTest = append(goTest, shuffle)
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
	if len(rep.UpFailures) > 0 {
		p.printf("UP FAILED: a walkthrough's cluster didn't come up, so its tests didn't run on it\n")
		for _, u := range rep.UpFailures {
			p.printf("\n%s  %s\n", u.Module, u.Walkthrough)
			for _, l := range u.Error {
				p.printf("        %s\n", l)
			}
			p.printf("    output:    %s\n", u.Output)
		}
		if len(rep.Flaky) > 0 || len(rep.Failing) > 0 {
			p.printf("\n")
		}
	}
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
		p.printf("FAILING: failed or hung every run at some pair of CPU count and load\n")
		for _, t := range rep.Failing {
			p.test(t)
		}
	}
	if rep.Failed() {
		p.printf("\n")
	}
	if rep.Interrupted {
		p.printf("INTERRUPTED: %d of %d planned runs ended, and these results are of those\n", rep.EndedRuns, rep.PlannedRuns)
	}
	p.printf("%s passed every run\n", plural(rep.Passed, "test", "tests"))
	if slices.ContainsFunc(rep.Levels, func(l Level) bool { return l.Confirm > 0 }) {
		incomplete := ""
		if rep.IncompleteConfirms > 0 {
			incomplete = fmt.Sprintf(", and the confirm runs of %s didn't all end", plural(rep.IncompleteConfirms, "test", "tests"))
		}
		p.printf("confirmed %s in %s%s\n", plural(rep.Confirmed, "test", "tests"), plural(rep.ConfirmRuns, "run", "runs"), incomplete)
	}
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
	if t.Level != "" && t.Level != discover.Unit {
		name += "  (" + t.Level + ")"
	}
	p.printf("\n%s  %s\n", t.Package, name)
	p.printf("    %-14s %9s %9s %9s\n", "", "failures", "hangs", "runs")
	for _, c := range t.Pairs {
		p.printf("    %-14s %9d %9d %9d\n", fmt.Sprintf("cpu %d load %d", c.CPU, c.Load), c.Failures, c.Hangs, c.Runs)
	}
	runs := plural(t.Runs, "run", "runs")
	if t.Confirmed {
		runs = plural(t.Runs, "confirm run", "confirm runs")
	}
	p.printf("    %s, %s in %s\n", plural(t.Failures, "failure", "failures"), plural(t.Hangs, "hang", "hangs"), runs)
	if t.ConfirmIncomplete {
		p.printf("    not fully confirmed: %d of its %d confirm runs ended\n", t.ConfirmEnded, t.ConfirmPlanned)
	}
	seed := ""
	if t.FirstFailureSeed != nil {
		seed = ", seed " + strconv.FormatInt(*t.FirstFailureSeed, 10)
	}
	p.printf("    first failure, at cpu %d load %d%s:\n", t.FirstFailureCPU, t.FirstFailureLoad, seed)
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
