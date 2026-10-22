// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command flaky runs the repository's tests until they fail, so a flaky test
// is found before it turns CI red. A maintainer runs it by hand, on Linux.
//
//	flaky stress [-tags unit,testcontainers] [-cpu 1,2,4] [-load 1,2]
//	             [-count n] [-confirm n] [-timeout d] [-keep n] [-containers n]
//	             [-shuffle] [-run regexp] [-skip regexp] [-o dir] [-json]
//	             [packages]
//
// -tags picks the levels, both by default. The unit level runs the untagged
// tests, as CI's Test jobs do. The testcontainers level builds with that tag,
// as CI's testcontainers jobs do, and runs only the tests its binary lists and
// the untagged binary doesn't, by -test.run, so no unit test runs at both.
//
// flaky stress builds every package's test binary at each level with go test
// -c -race -cover, then runs each one -count times at each pair of -cpu and
// -load values. Each run is pinned with taskset to as many cores as its CPU count,
// so both GOMAXPROCS and runtime.NumCPU() are that count, as on a GitHub
// runner with that many vCPUs. At load 1 a run has its cores to itself; at
// load L, up to L runs are pinned to each of its cores, so they compete for
// them.
//
// -keep cores, the first ones, are kept back from the runs, for flaky,
// dockerd and the testcontainers containers. flaky watches Docker's events,
// and pins each container labelled org.testcontainers to them as it starts,
// so only the code under test gets each run's CPU count and load.
// -containers caps how many testcontainers runs go at once.
//
// -count, -confirm and -timeout are 500, 1000 and 2m at the unit level, and
// 25, 50 and 10m at the testcontainers level. Given, each applies to every
// level.
//
// Once every one of those runs has ended, it confirms each test that failed
// or hung: it runs the test alone, by its own -run pattern, -confirm times at
// each pair, including the pairs where it never failed. With -run, there are
// no confirm runs: the matching tests run -count times at each pair.
//
// With -shuffle, each run shuffles its tests with a seed of its own, as
// go test -shuffle does, so a test that fails only after another is found.
// -shuffle=<seed> shuffles every run with that seed. A failure's seed is kept
// with its result, and the commands that reproduce it name it.
//
// It reports each test that failed or hung, at each pair, and exits 1 if any
// did.
//
// Packages are patterns from the repository's root, such as ./grpc/... or
// ./example/wallet/app. With none, it runs every module that
// .github/scripts/go-modules.sh lists.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/flaky/discover"
	"github.com/z5labs/humus/tools/flaky/docker"
	"github.com/z5labs/humus/tools/flaky/internal/env"
	"github.com/z5labs/humus/tools/flaky/lanes"
	"github.com/z5labs/humus/tools/flaky/results"
	"github.com/z5labs/humus/tools/flaky/run"
	"github.com/z5labs/humus/tools/flaky/stress"
)

// grace is how long after its timeout a run still going is killed.
const grace = 30 * time.Second

// testcontainers is the level of the tests behind the testcontainers tag,
// whose containers are pinned to the kept-back cores.
const testcontainers = "testcontainers"

// levelDefaults are each level's -count, -confirm and -timeout, in the order
// -tags lists them by default.
var levelDefaults = []struct {
	name           string
	count, confirm int
	timeout        time.Duration
}{
	{discover.Unit, 500, 1000, 2 * time.Minute},
	{testcontainers, 25, 50, 10 * time.Minute},
}

func main() {
	bedrock.Main(bedrock.AppFunc(flaky))
}

const usage = `usage: flaky stress [flags] [packages]

flaky stress runs every package's tests at each level of -tags -count times
at each pair of -cpu and -load values, each run pinned to as many cores as its
CPU count and sharing each of them with up to its load of runs. The
testcontainers containers run on the -keep cores kept back from the runs.
Then it runs each test that failed or hung alone, -confirm times at each pair,
and reports each test that failed or hung. With -run, it runs the matching
tests -count times at each pair, and confirms none. With -shuffle, each run
shuffles its tests with a seed of its own.
`

func flaky(ctx context.Context, p bedrock.Process) bedrock.ExitStatus {
	if len(p.Args) < 2 {
		say(p.Stderr, "%s", usage)
		return bedrock.ExitWithCause(2, errors.New("no command"))
	}
	switch p.Args[1] {
	case "stress":
		return stressCommand(ctx, p, p.Args[2:])
	case "-h", "-help", "--help", "help":
		say(p.Stdout, "%s", usage)
		return bedrock.Exit(0)
	}
	say(p.Stderr, "flaky: unknown command %q\n%s", p.Args[1], usage)
	return bedrock.ExitWithCause(2, fmt.Errorf("unknown command %q", p.Args[1]))
}

type stressFlags struct {
	levels     []string
	cpus       []int
	loads      []int
	count      int
	confirm    int
	timeout    time.Duration
	keep       int
	containers int
	shuffle    shuffle
	run        string
	skip       string
	out        string
	json       bool
	patterns   []string
	// set are the flags given.
	set map[string]bool
}

// level returns how the level name runs: its defaults, with -count, -confirm
// and -timeout in their place when they were given.
func (f stressFlags) level(name string) results.Level {
	l := results.Level{Name: name}
	for _, d := range levelDefaults {
		if d.name == name {
			l.Count, l.Confirm, l.Timeout = d.count, d.confirm, d.timeout
		}
	}
	l.DefaultTimeout = l.Timeout
	if f.set["count"] {
		l.Count = f.count
	}
	if f.set["confirm"] {
		l.Confirm = f.confirm
	}
	if f.set["timeout"] {
		l.Timeout = f.timeout
	}
	return l
}

func parseStress(p bedrock.Process, args []string) (stressFlags, error) {
	f := stressFlags{cpus: []int{1, 2, 4}, loads: []int{1, 2}, set: map[string]bool{}}
	for _, d := range levelDefaults {
		f.levels = append(f.levels, d.name)
	}
	fs := flag.NewFlagSet("flaky stress", flag.ContinueOnError)
	fs.SetOutput(p.Stderr)
	fs.Usage = func() {
		say(fs.Output(), "%s\nflags:\n", usage)
		fs.PrintDefaults()
	}
	fs.Func("tags", "the levels to run, comma-separated: unit, the untagged tests, and testcontainers, the tests behind that tag (default unit,testcontainers)", func(s string) error {
		levels, err := parseLevels(s)
		f.levels = levels
		return err
	})
	fs.Func("cpu", "the CPU counts to run each package at, comma-separated (default 1,2,4)", func(s string) error {
		cpus, err := parseCounts(s, "CPU count")
		f.cpus = cpus
		return err
	})
	fs.Func("load", "the loads to run each package at, comma-separated: how many runs, at most, share each core (default 1,2)", func(s string) error {
		loads, err := parseCounts(s, "load")
		f.loads = loads
		return err
	})
	fs.IntVar(&f.count, "count", 0, "how many times to run each package at each pair of CPU count and load (default 500 at the unit level, 25 at the testcontainers level)")
	fs.IntVar(&f.confirm, "confirm", 0, "how many times to run each test that failed or hung alone, at each pair of CPU count and load, once the sweep has ended; with -run, none is (default 1000 at the unit level, 50 at the testcontainers level)")
	fs.DurationVar(&f.timeout, "timeout", 0, "each run's -test.timeout; a run still going 30s later is killed (default 2m at the unit level, 10m at the testcontainers level)")
	fs.IntVar(&f.keep, "keep", 8, "how many cores to keep back from the lanes, for flaky, dockerd and the testcontainers containers")
	fs.IntVar(&f.containers, "containers", 4, "how many testcontainers runs, at most, go at once")
	fs.Var(&f.shuffle, "shuffle", "shuffle each run's tests with a seed of its own, or with -shuffle=<seed>, every run's with that seed")
	fs.StringVar(&f.run, "run", "", "run only the tests that match, as go test -run")
	fs.StringVar(&f.skip, "skip", "", "skip the tests that match, as go test -skip")
	fs.StringVar(&f.out, "o", "", "the directory to save failures' output under (default $TMPDIR/flaky-<time>)")
	fs.BoolVar(&f.json, "json", false, "write the report as JSON")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	if f.set["count"] && f.count < 1 {
		return f, fmt.Errorf("-count must be at least 1, not %d", f.count)
	}
	if f.set["confirm"] && f.confirm < 1 {
		return f, fmt.Errorf("-confirm must be at least 1, not %d", f.confirm)
	}
	if f.set["timeout"] && f.timeout <= 0 {
		return f, fmt.Errorf("-timeout must be positive, not %s", f.timeout)
	}
	if f.containers < 1 {
		return f, fmt.Errorf("-containers must be at least 1, not %d", f.containers)
	}
	f.patterns = fs.Args()
	return f, nil
}

// shuffle is -shuffle's value. It is off, on, or a seed, as go test's
// -shuffle is, but on gives each run a seed of its own.
type shuffle struct {
	on   bool
	seed *int64
}

func (s *shuffle) String() string {
	switch {
	case s.seed != nil:
		return strconv.FormatInt(*s.seed, 10)
	case s.on:
		return "on"
	}
	return "off"
}

func (s *shuffle) Set(v string) error {
	switch v {
	case "off", "false":
		*s = shuffle{}
	case "on", "true":
		*s = shuffle{on: true}
	default:
		seed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not on, off or a seed", v)
		}
		*s = shuffle{on: true, seed: &seed}
	}
	return nil
}

// IsBoolFlag lets -shuffle be given alone, to mean on.
func (s *shuffle) IsBoolFlag() bool { return true }

// next returns the seed of a run, or nil when runs aren't shuffled.
func (s *shuffle) next() *int64 {
	switch {
	case s.seed != nil:
		seed := *s.seed
		return &seed
	case s.on:
		seed := rand.Int64()
		return &seed
	}
	return nil
}

// parseLevels parses -tags: a comma-separated list of levels, each once.
func parseLevels(s string) ([]string, error) {
	var levels []string
	for part := range strings.SplitSeq(s, ",") {
		level := strings.TrimSpace(part)
		known := false
		for _, d := range levelDefaults {
			known = known || d.name == level
		}
		if !known {
			return nil, fmt.Errorf("%q is not a level: the levels are unit and testcontainers", part)
		}
		if !slices.Contains(levels, level) {
			levels = append(levels, level)
		}
	}
	return levels, nil
}

// parseCounts parses a comma-separated list of positive numbers, each a what.
func parseCounts(s, what string) ([]int, error) {
	var ns []int
	for part := range strings.SplitSeq(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%q is not a %s", part, what)
		}
		ns = append(ns, n)
	}
	return ns, nil
}

func stressCommand(ctx context.Context, p bedrock.Process, args []string) bedrock.ExitStatus {
	f, err := parseStress(p, args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return bedrock.Exit(0)
	case err != nil:
		say(p.Stderr, "flaky stress: %v\n", err)
		return bedrock.ExitWithCause(2, err)
	}
	ctx, stop := p.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep, err := stressRun(ctx, p, f)
	if err != nil {
		say(p.Stderr, "flaky stress: %v\n", err)
		var failed *stress.BuildError
		if errors.As(err, &failed) {
			return bedrock.ExitWithCause(1, err)
		}
		return bedrock.ExitWithCause(2, err)
	}
	write := results.WriteText
	if f.json {
		write = results.WriteJSON
	}
	if err := write(p.Stdout, rep); err != nil {
		return bedrock.ExitWithCause(2, err)
	}
	if rep.Failed() {
		return bedrock.ExitWithCause(1, errors.New("a test failed or hung"))
	}
	return bedrock.Exit(0)
}

func stressRun(ctx context.Context, p bedrock.Process, f stressFlags) (results.Report, error) {
	childEnv := env.Child(p.Environment)
	// The containers' pinning warns from a goroutine of its own, beside
	// the sweep's progress.
	log := &syncWriter{w: p.Stderr}
	wd, err := os.Getwd()
	if err != nil {
		return results.Report{}, err
	}
	root, err := discover.Root(ctx, wd, childEnv)
	if err != nil {
		return results.Report{}, err
	}
	goVersion, err := discover.GoVersion(ctx, root, childEnv)
	if err != nil {
		return results.Report{}, err
	}
	if warning := discover.CheckGoVersion(root, goVersion); warning != "" {
		say(log, "%s\n", warning)
	}
	commit, err := discover.Commit(ctx, root, childEnv)
	if err != nil {
		return results.Report{}, err
	}

	cores, err := run.Affinity()
	if err != nil {
		return results.Report{}, err
	}
	laneCores, err := lanes.Split(cores, f.keep, maxOf(f.cpus))
	if err != nil {
		return results.Report{}, err
	}

	start := time.Now()
	modules, err := discover.Modules(ctx, root, childEnv)
	if err != nil {
		return results.Report{}, err
	}
	pkgs, err := discover.Packages(ctx, root, modules, f.patterns, f.levels, childEnv)
	if err != nil {
		return results.Report{}, err
	}
	discovery := time.Since(start)
	atLevel := make([]string, len(f.levels))
	for i, level := range f.levels {
		n := 0
		for _, pkg := range pkgs {
			if pkg.Level == level {
				n++
			}
		}
		atLevel[i] = fmt.Sprintf("%d %s", n, level)
	}
	say(log, "found %d packages with tests (%s) in %s\n", len(pkgs), strings.Join(atLevel, ", "), discovery.Round(time.Millisecond))

	if slices.ContainsFunc(pkgs, func(pkg discover.Package) bool { return pkg.Level == testcontainers }) {
		kept := lanes.Kept(cores, f.keep)
		if len(kept) == 0 {
			return results.Report{}, errors.New("-keep 0 keeps back no cores for the testcontainers containers")
		}
		if err := docker.Check(ctx, childEnv); err != nil {
			return results.Report{}, err
		}
		pinner, err := docker.Pin(ctx, kept, childEnv, log)
		if err != nil {
			return results.Report{}, err
		}
		// An error that ended the watch early has been warned of
		// already.
		defer func() { _ = pinner.Stop() }()
		say(log, "pinning the testcontainers containers to cores %s\n", run.CoreList(kept))
	}

	tmp := tempDir(p.Environment)
	if f.out == "" {
		f.out = filepath.Join(tmp, "flaky-"+time.Now().Format("20060102T150405"))
	}
	bins, err := os.MkdirTemp(tmp, "flaky-bin-")
	if err != nil {
		return results.Report{}, err
	}
	defer func() {
		if err := os.RemoveAll(bins); err != nil {
			say(log, "warning: %v\n", err)
		}
	}()
	test2json, err := run.Test2JSON(ctx, childEnv)
	if err != nil {
		return results.Report{}, err
	}

	levels := make([]results.Level, len(f.levels))
	stressLevels := make([]stress.Level, len(f.levels))
	for i, name := range f.levels {
		levels[i] = f.level(name)
		if f.run != "" {
			// -run asks for the matching tests at -count, so there is
			// no sweep to confirm.
			levels[i].Confirm = 0
		}
		stressLevels[i] = stress.Level{
			Name:    name,
			Count:   levels[i].Count,
			Confirm: levels[i].Confirm,
			Own:     name != discover.Unit,
		}
		if name == testcontainers {
			stressLevels[i].Limit = f.containers
		}
	}
	r, err := newExecRunner(root, pkgs, bins, levels, f, childEnv, test2json)
	if err != nil {
		return results.Report{}, err
	}
	res := results.New(results.Config{
		Packages: pkgs, CPUs: f.cpus, Loads: f.loads, Levels: levels, Out: f.out,
		Run: f.run, Skip: f.skip,
	})
	pkgLevels := make([]int, len(pkgs))
	for i, pkg := range pkgs {
		pkgLevels[i] = slices.Index(f.levels, pkg.Level)
	}
	phases, err := stress.Stress(ctx, stress.Config{
		Levels:   stressLevels,
		Packages: pkgLevels,
		CPUs:     f.cpus,
		Loads:    f.loads,
		Lanes:    laneCores,
		Builds:   max(len(cores)/4, 1),
		Log:      log,
		Progress: time.Minute,
	}, r, res)
	if err != nil {
		return results.Report{}, err
	}

	rep := res.Report()
	rep.Commit = commit
	rep.Go = goVersion
	rep.Cores = len(cores)
	rep.Phases = []results.Phase{
		results.NewPhase("discovery", discovery),
		results.NewPhase("build", phases.Build),
		results.NewPhase("sweep", phases.Sweep),
	}
	if f.run == "" {
		rep.Phases = append(rep.Phases, results.NewPhase("confirm", phases.Confirm))
	}
	return rep, nil
}

// syncWriter is a writer safe for concurrent use.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func tempDir(e bedrock.Environment) string {
	if dir, err := e.Value("TMPDIR"); err == nil && dir != "" {
		return dir
	}
	return "/tmp"
}

func maxOf(ns []int) int {
	m := 0
	for _, n := range ns {
		m = max(m, n)
	}
	return m
}

type execRunner struct {
	root      string
	pkgs      []discover.Package
	bins      []string
	env       []string
	test2json string
	timeouts  []time.Duration
	run, skip string
	shuffle   shuffle
	// untagged is, for each package at a tagged level that has test files
	// without the tag, the binary built without it that its tests are
	// told apart from, and buildUntagged reports that the package builds
	// it, since no package at the unit level does.
	untagged      []string
	buildUntagged []bool
	// own are the tests each package at a tagged level runs, once it is
	// listed.
	own [][]string
}

func newExecRunner(root string, pkgs []discover.Package, bins string, levels []results.Level, f stressFlags, childEnv []string, test2json string) (*execRunner, error) {
	r := &execRunner{
		root: root, pkgs: pkgs, env: childEnv, test2json: test2json,
		run: f.run, skip: f.skip, shuffle: f.shuffle,
		bins:          make([]string, len(pkgs)),
		timeouts:      make([]time.Duration, len(pkgs)),
		untagged:      make([]string, len(pkgs)),
		buildUntagged: make([]bool, len(pkgs)),
		own:           make([][]string, len(pkgs)),
	}
	unit := map[string]int{}
	for i, pkg := range pkgs {
		name := filepath.Base(pkg.Dir)
		if tag := discover.Tag(pkg.Level); tag != "" {
			name += "-" + tag
		}
		r.bins[i] = filepath.Join(bins, fmt.Sprintf("%d-%s.test", i, name))
		for _, l := range levels {
			if l.Name == pkg.Level {
				r.timeouts[i] = l.Timeout
			}
		}
		if pkg.Level == discover.Unit {
			unit[pkg.ImportPath] = i
		}
	}
	for i, pkg := range pkgs {
		if pkg.Level == discover.Unit || !pkg.Untagged {
			continue
		}
		if u, ok := unit[pkg.ImportPath]; ok {
			r.untagged[i] = r.bins[u]
			continue
		}
		r.untagged[i] = strings.TrimSuffix(r.bins[i], ".test") + "-untagged.test"
		r.buildUntagged[i] = true
	}
	if f.run != "" {
		if _, err := regexp.Compile(topLevel(f.run)); err != nil {
			return nil, fmt.Errorf("-run: %w", err)
		}
	}
	return r, nil
}

func (r *execRunner) Build(ctx context.Context, pkg int) error {
	p := r.pkgs[pkg]
	dir := filepath.Join(r.root, p.Module)
	if err := run.Build(ctx, dir, p.ImportPath, discover.Tag(p.Level), r.bins[pkg], r.env); err != nil {
		return err
	}
	if r.buildUntagged[pkg] {
		return run.Build(ctx, dir, p.ImportPath, "", r.untagged[pkg], r.env)
	}
	return nil
}

// List returns the tests the package pkg's binary runs. At the unit level,
// they are the ones it lists. At a tagged level, they are the ones it lists
// and the untagged binary doesn't, which -run matches: the level's own tests.
func (r *execRunner) List(ctx context.Context, pkg int) ([]string, error) {
	dir := filepath.Join(r.root, r.pkgs[pkg].Dir)
	tests, err := run.List(ctx, r.bins[pkg], dir, r.env)
	if err != nil || r.pkgs[pkg].Level == discover.Unit {
		return tests, err
	}
	var untagged []string
	if r.untagged[pkg] != "" {
		if untagged, err = run.List(ctx, r.untagged[pkg], dir, r.env); err != nil {
			return nil, err
		}
	}
	match := regexp.MustCompile(topLevel(r.run))
	var own []string
	for _, t := range tests {
		if !slices.Contains(untagged, t) && match.MatchString(t) {
			own = append(own, t)
		}
	}
	r.own[pkg] = own
	return own, nil
}

// Run runs the package pkg's binary once. A confirm run runs test alone, by
// its own -run pattern; a sweep run, with test "", runs what -run matches, of
// the level's own tests at a tagged level. With -shuffle, either shuffles,
// with the seed -shuffle gives it.
func (r *execRunner) Run(ctx context.Context, pkg int, test string, cpu int, cores []int) run.Result {
	pattern := r.run
	switch {
	case test != "":
		pattern = results.RunPattern(test)
	case r.pkgs[pkg].Level != discover.Unit:
		pattern = ownPattern(r.own[pkg], r.run)
	}
	return run.Exec(ctx, run.Spec{
		Binary:     r.bins[pkg],
		Dir:        filepath.Join(r.root, r.pkgs[pkg].Dir),
		ImportPath: r.pkgs[pkg].ImportPath,
		Cores:      cores,
		CPU:        cpu,
		Timeout:    r.timeouts[pkg],
		Grace:      grace,
		Run:        pattern,
		Skip:       r.skip,
		Seed:       r.shuffle.next(),
		Env:        r.env,
		Test2JSON:  r.test2json,
	})
}

// ownPattern returns the -run pattern that runs exactly the top-level tests
// own, and in each, what the rest of the pattern run matches, after its
// top-level part.
func ownPattern(own []string, run string) string {
	quoted := make([]string, len(own))
	for i, t := range own {
		quoted[i] = regexp.QuoteMeta(t)
	}
	pattern := "^(" + strings.Join(quoted, "|") + ")$"
	if _, rest, ok := splitRun(run); ok {
		pattern += "/" + rest
	}
	return pattern
}

// topLevel returns the part of the -run pattern run that a top-level test's
// name must match.
func topLevel(run string) string {
	top, _, _ := splitRun(run)
	return top
}

// splitRun splits the -run pattern run at its first slash outside brackets
// and parentheses, as the testing package does, into the part a top-level
// test's name must match and the rest. ok reports that there was a slash.
func splitRun(run string) (top, rest string, ok bool) {
	brackets, parens := 0, 0
	for i := 0; i < len(run); i++ {
		switch run[i] {
		case '[':
			brackets++
		case ']':
			brackets = max(brackets-1, 0)
		case '(':
			if brackets == 0 {
				parens++
			}
		case ')':
			if brackets == 0 {
				parens = max(parens-1, 0)
			}
		case '\\':
			i++
		case '/':
			if brackets == 0 && parens == 0 {
				return run[:i], run[i+1:], true
			}
		}
	}
	return run, "", false
}

// say writes to one of the process's streams. A failed write there has
// nowhere left to be reported, so it is dropped.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
