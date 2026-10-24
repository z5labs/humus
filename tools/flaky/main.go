// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command flaky runs the repository's tests until they fail, so a flaky test
// is found before it turns CI red. A maintainer runs it by hand, on Linux.
//
//	flaky stress [-tags unit,testcontainers,...] [-cpu 1,2,4] [-load 1,2]
//	             [-count n] [-confirm n] [-timeout d] [-keep n] [-containers n]
//	             [-clusters n] [-shuffle] [-run regexp] [-skip regexp] [-o dir]
//	             [-json] [packages]
//
// The levels are found on each run: the unit level, plus one level for each
// build tag in a //go:build line of a _test.go file that git tracks. -tags
// picks among them, all of them by default. The unit level runs the untagged
// tests, as CI's Test jobs do. Any other level builds with its tag, and runs
// only the tests its binary lists and the untagged binary doesn't, by
// -test.run, so no unit test runs at two levels. The testcontainers level does
// so as CI's testcontainers jobs do, and the functional level runs each
// walkthrough's functional tests against a kind cluster, as CI's kind jobs do.
// Any other tag, such as conformance, runs as the unit tests do, with its tag.
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
// A walkthrough is a module with a Dockerfile and a deploy/kind/up.sh, and its
// functional tests are its packages at the functional level. Its image is
// built with the test binaries, from the repository's root, and tagged with
// its directory's name. Its functional runs go on a kind cluster of its own,
// flaky-<walkthrough>, whose node is pinned to one lane's 4 cores. up.sh
// deploys onto it, with KIND_CLUSTER_NAME set, and once its Deployments have
// rolled out, the runs follow one at a time, each with KIND_CLUSTER_NAME set
// and pinned to the same 4 cores, at load 1. Then the cluster is deleted. Its
// CPU count and load aren't varied. -clusters caps how many clusters are up
// at once, and clusters start before every other run. A walkthrough whose
// cluster doesn't come up is reported by name.
//
// -count, -confirm and -timeout are 500, 1000 and 2m at the unit level, and
// at any level whose tag flaky doesn't know, 25, 50 and 10m at the
// testcontainers level, and 10, 50 and 15m at the functional level. Given,
// each applies to every level.
//
// Once every one of those runs has ended, it confirms each test that failed
// or hung: it runs the test alone, by its own -run pattern, -confirm times at
// each pair, including the pairs where it never failed. A functional test gets
// a fresh cluster for its confirm runs. With -run, there are no confirm runs:
// the matching tests run -count times at each pair.
//
// With -shuffle, each run shuffles its tests with a seed of its own, as
// go test -shuffle does, so a test that fails only after another is found.
// -shuffle=<seed> shuffles every run with that seed. A failure's seed is kept
// with its result, and the commands that reproduce it name it.
//
// It reports each test that failed or hung, at each pair, and exits 1 if any
// did, and 0 if none did. It exits 2 when it can't run the sweep.
//
// On SIGINT or SIGTERM, it starts no more runs, waits for the runs going, and
// deletes the clusters that are up. Then it reports what the runs that ended
// found, as it would have at the end, and exits 3, whatever they found. A run
// cut short counts as neither a pass nor a failure. The report says the sweep
// was interrupted, how many runs it planned and how many ended: with -json,
// in interrupted, plannedRuns and endedRuns. No confirm run starts after the
// interrupt. Interrupted during the sweep, a test that failed or hung has no
// confirm runs, and its counts are its sweep runs'. Interrupted during the
// confirm runs, a test's counts are of its confirm runs that ended, and a test
// whose confirm runs didn't all end is marked not fully confirmed, with -json
// in confirmIncomplete, confirmPlanned and confirmEnded.
//
// Packages are patterns from the repository's root, such as ./grpc/... or
// ./example/grpc/wallet/app. With none, it runs every module that
// .github/scripts/go-modules.sh lists.
//
//	flaky report -results file -package path -test name -description file
//	             [-notes file] [-dry-run]
//
// flaky report posts a flaky test's Bugs discussion, rendered from a fixed
// template. -results is what flaky stress -json wrote, -package the test's
// package, as its directory from the repository's root or its import path,
// and -test its full name. -description and -notes are files of Markdown with
// no headings: what the test checks and how it fails, and anything else worth
// saying. Everything else, the title, the commit, the Go version, the core
// count, the table, the first failure, both commands and a link to the test's
// function at the commit, is rendered from the results.
//
// It posts nothing, and says why, when the description or the notes is empty
// or has a heading, when the results don't have the test failing or hanging,
// or when the test failed every run at some pair, since that test is failing
// rather than flaky. When an open Bugs discussion or an open story already
// names the test, it prints that report's link and exits 2. A closed report
// that names the test is linked under References.
//
// It finds the Bugs category by name, creates the discussion with gh api
// graphql, and prints its URL. With -dry-run, it prints the title and body
// instead. It exits 1 when it posts nothing for any other reason.
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
	"github.com/z5labs/humus/tools/flaky/report"
	"github.com/z5labs/humus/tools/flaky/results"
	"github.com/z5labs/humus/tools/flaky/run"
	"github.com/z5labs/humus/tools/flaky/stress"
)

// grace is how long after its timeout a run still going is killed.
const grace = 30 * time.Second

// interruptedCode is the exit code of a sweep stopped by SIGINT or SIGTERM,
// which still reports what the runs that ended found.
const interruptedCode = 3

// testcontainers is the level of the tests behind the testcontainers tag,
// whose containers are pinned to the kept-back cores.
const testcontainers = "testcontainers"

// functional is the level of the tests behind the functional tag, which run
// on their walkthrough's kind cluster.
const functional = "functional"

// upTimeout bounds how long a cluster takes to come up, and downTimeout how
// long it takes to be deleted.
const (
	upTimeout   = 20 * time.Minute
	downTimeout = 5 * time.Minute
)

// levelDefaults are the -count, -confirm and -timeout of the levels flaky
// knows, in the order -tags lists them by default, before the levels it
// doesn't know. A level it doesn't know has the unit level's.
var levelDefaults = []levelDefault{
	{discover.Unit, 500, 1000, 2 * time.Minute},
	{testcontainers, 25, 50, 10 * time.Minute},
	{functional, 10, 50, 15 * time.Minute},
}

type levelDefault struct {
	name           string
	count, confirm int
	timeout        time.Duration
}

func main() {
	bedrock.Main(bedrock.AppFunc(flaky))
}

const usage = `usage: flaky stress [flags] [packages]
       flaky report -results file -package path -test name -description file [-notes file] [-dry-run]

flaky stress runs every package's tests at each level of -tags, the unit level
and one for each build tag the test files use by default, -count times
at each pair of -cpu and -load values, each run pinned to as many cores as its
CPU count and sharing each of them with up to its load of runs. The
testcontainers containers run on the -keep cores kept back from the runs, and
each walkthrough's functional tests on a kind cluster of its own, pinned to a
lane's 4 cores. Then it runs each test that failed or hung alone, -confirm times at each pair,
and reports each test that failed or hung. With -run, it runs the matching
tests -count times at each pair, and confirms none. With -shuffle, each run
shuffles its tests with a seed of its own. On SIGINT or SIGTERM, it reports
what the runs that ended found, and exits 3.

flaky report posts a flaky test's Bugs discussion, rendered from the results
of flaky stress -json and a description, unless an open report already names
the test. Then it prints that report's link and exits 2.
`

func flaky(ctx context.Context, p bedrock.Process) bedrock.ExitStatus {
	if len(p.Args) < 2 {
		say(p.Stderr, "%s", usage)
		return bedrock.ExitWithCause(2, errors.New("no command"))
	}
	switch p.Args[1] {
	case "stress":
		return stressCommand(ctx, p, p.Args[2:])
	case "report":
		return reportCommand(ctx, p, p.Args[2:], func(root string) report.GitHub {
			return report.GH{Dir: root, Env: env.GH(p.Environment)}
		})
	case "-h", "-help", "--help", "help":
		say(p.Stdout, "%s", usage)
		return bedrock.Exit(0)
	}
	say(p.Stderr, "flaky: unknown command %q\n%s", p.Args[1], usage)
	return bedrock.ExitWithCause(2, fmt.Errorf("unknown command %q", p.Args[1]))
}

type stressFlags struct {
	// tags is -tags, the levels asked for, or nil for every level found.
	tags       []string
	cpus       []int
	loads      []int
	count      int
	confirm    int
	timeout    time.Duration
	keep       int
	containers int
	clusters   int
	shuffle    shuffle
	run        string
	skip       string
	out        string
	json       bool
	patterns   []string
	// set are the flags given.
	set map[string]bool
}

// level returns how the level name runs: its defaults, or the unit level's
// when flaky doesn't know it, with -count, -confirm and -timeout in their
// place when they were given.
func (f stressFlags) level(name string) results.Level {
	l := results.Level{Name: name}
	d := levelDefaults[0]
	if i := slices.IndexFunc(levelDefaults, func(d levelDefault) bool { return d.name == name }); i >= 0 {
		d = levelDefaults[i]
	}
	l.Count, l.Confirm, l.Timeout = d.count, d.confirm, d.timeout
	l.DefaultTimeout = l.Timeout
	if name == functional {
		l.CPU, l.Load = stress.ClusterCPU, 1
	}
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
	fs := flag.NewFlagSet("flaky stress", flag.ContinueOnError)
	fs.SetOutput(p.Stderr)
	fs.Usage = func() {
		say(fs.Output(), "%s\nflags:\n", usage)
		fs.PrintDefaults()
	}
	fs.Func("tags", "the levels to run, comma-separated: unit, the untagged tests, and any build tag the test files use, the tests behind it (default every level found)", func(s string) error {
		f.tags = splitLevels(s)
		return nil
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
	fs.IntVar(&f.count, "count", 0, "how many times to run each package at each pair of CPU count and load (default 500 at the unit level and any level whose tag flaky doesn't know, 25 at the testcontainers level, 10 at the functional level)")
	fs.IntVar(&f.confirm, "confirm", 0, "how many times to run each test that failed or hung alone, at each pair of CPU count and load, once the sweep has ended; with -run, none is (default 1000 at the unit level and any level whose tag flaky doesn't know, 50 at the testcontainers and functional levels)")
	fs.DurationVar(&f.timeout, "timeout", 0, "each run's -test.timeout; a run still going 30s later is killed (default 2m at the unit level and any level whose tag flaky doesn't know, 10m at the testcontainers level, 15m at the functional level)")
	fs.IntVar(&f.keep, "keep", 8, "how many cores to keep back from the lanes, for flaky, dockerd and the testcontainers containers")
	fs.IntVar(&f.containers, "containers", 4, "how many testcontainers runs, at most, go at once")
	fs.IntVar(&f.clusters, "clusters", 2, "how many walkthroughs' kind clusters, at most, are up at once")
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
	if f.clusters < 1 {
		return f, fmt.Errorf("-clusters must be at least 1, not %d", f.clusters)
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

// splitLevels splits -tags, a comma-separated list of levels, into each one
// once.
func splitLevels(s string) []string {
	var levels []string
	for part := range strings.SplitSeq(s, ",") {
		if level := strings.TrimSpace(part); !slices.Contains(levels, level) {
			levels = append(levels, level)
		}
	}
	return levels
}

// pickLevels returns the levels to run, of found: the ones tags asks for, in
// its order, or with no tags, every one, those flaky knows first, in the
// order of levelDefaults, and then the rest in found's order.
func pickLevels(found, tags []string) ([]string, error) {
	if tags == nil {
		known := func(level string) int {
			i := slices.IndexFunc(levelDefaults, func(d levelDefault) bool { return d.name == level })
			if i < 0 {
				return len(levelDefaults)
			}
			return i
		}
		levels := slices.Clone(found)
		slices.SortStableFunc(levels, func(a, b string) int { return known(a) - known(b) })
		return levels, nil
	}
	for _, level := range tags {
		if !slices.Contains(found, level) {
			names := found
			if len(names) == 1 {
				return nil, fmt.Errorf("%q is not a level: the only level is %s", level, names[0])
			}
			last := len(names) - 1
			return nil, fmt.Errorf("%q is not a level: the levels are %s and %s", level, strings.Join(names[:last], ", "), names[last])
		}
	}
	return tags, nil
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
	return writeStress(p, f, rep)
}

// writeStress writes the report rep of a sweep, and returns its exit status:
// interruptedCode when the sweep was interrupted, 1 when a test failed or
// hung, or a cluster didn't come up, and 0 otherwise.
func writeStress(p bedrock.Process, f stressFlags, rep results.Report) bedrock.ExitStatus {
	write := results.WriteText
	if f.json {
		write = results.WriteJSON
	}
	if err := write(p.Stdout, rep); err != nil {
		return bedrock.ExitWithCause(2, err)
	}
	if rep.Interrupted {
		return bedrock.ExitWithCause(interruptedCode, fmt.Errorf("interrupted with %d of %d planned runs ended", rep.EndedRuns, rep.PlannedRuns))
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

	start := time.Now()
	found, err := discover.Levels(ctx, root, childEnv)
	if err != nil {
		return results.Report{}, err
	}
	levelNames, err := pickLevels(found, f.tags)
	if err != nil {
		return results.Report{}, fmt.Errorf("-tags: %w", err)
	}
	modules, err := discover.Modules(ctx, root, childEnv)
	if err != nil {
		return results.Report{}, err
	}
	pkgs, err := discover.Packages(ctx, root, modules, f.patterns, levelNames, childEnv)
	if err != nil {
		return results.Report{}, err
	}
	discovery := time.Since(start)
	atLevel := make([]string, len(levelNames))
	for i, level := range levelNames {
		n := 0
		for _, pkg := range pkgs {
			if pkg.Level == level {
				n++
			}
		}
		atLevel[i] = fmt.Sprintf("%d %s", n, level)
	}
	walkthroughs, walkOf, err := pkgWalkthroughs(root, modules, pkgs)
	if err != nil {
		return results.Report{}, err
	}
	say(log, "found %d packages with tests (%s) in %s\n", len(pkgs), strings.Join(atLevel, ", "), discovery.Round(time.Millisecond))

	// A lane is as wide as the largest CPU count, and as a cluster when
	// there is one.
	width := maxOf(f.cpus)
	if len(walkthroughs) > 0 {
		width = max(width, stress.ClusterCPU)
	}
	laneCores, err := lanes.Split(cores, f.keep, width)
	if err != nil {
		return results.Report{}, err
	}
	if len(walkthroughs) > 0 {
		if err := docker.Check(ctx, functional, childEnv); err != nil {
			return results.Report{}, err
		}
		if err := docker.CheckKind(); err != nil {
			return results.Report{}, err
		}
	}

	if slices.ContainsFunc(pkgs, func(pkg discover.Package) bool { return pkg.Level == testcontainers }) {
		kept := lanes.Kept(cores, f.keep)
		if len(kept) == 0 {
			return results.Report{}, errors.New("-keep 0 keeps back no cores for the testcontainers containers")
		}
		if err := docker.Check(ctx, testcontainers, childEnv); err != nil {
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

	levels := make([]results.Level, len(levelNames))
	stressLevels := make([]stress.Level, len(levelNames))
	for i, name := range levelNames {
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
		switch name {
		case testcontainers:
			stressLevels[i].Limit = f.containers
		case functional:
			stressLevels[i].Limit = f.clusters
		}
	}
	r, err := newExecRunner(root, pkgs, bins, levels, f, childEnv, test2json)
	if err != nil {
		return results.Report{}, err
	}
	r.walkthroughs, r.walkOf, r.log = walkthroughs, walkOf, log
	res := results.New(results.Config{
		Packages: pkgs, CPUs: f.cpus, Loads: f.loads, Levels: levels, Out: f.out,
		Walkthroughs: walkthroughs,
		Run:          f.run, Skip: f.skip,
	})
	pkgLevels := make([]int, len(pkgs))
	for i, pkg := range pkgs {
		pkgLevels[i] = slices.Index(levelNames, pkg.Level)
	}
	phases, err := stress.Stress(ctx, stress.Config{
		Levels:       stressLevels,
		Packages:     pkgLevels,
		Walkthroughs: walkOf,
		CPUs:         f.cpus,
		Loads:        f.loads,
		Lanes:        laneCores,
		Builds:       max(len(cores)/4, 1),
		Log:          log,
		Progress:     time.Minute,
	}, r, res)
	// A sweep stopped by a signal still reports the runs that ended,
	// whatever error stopping it gave, such as a build it killed.
	interrupted := err != nil && ctx.Err() != nil
	if err != nil && !interrupted {
		return results.Report{}, err
	}

	rep := res.Report()
	rep.Interrupted = interrupted
	if interrupted {
		say(log, "interrupted: %d of %d planned runs ended\n", rep.EndedRuns, rep.PlannedRuns)
	}
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

const reportUsage = `usage: flaky report -results file -package path -test name -description file [-notes file] [-dry-run]

flaky report renders a flaky test's Bugs discussion from the results of flaky
stress -json, the description and any notes, and creates it, then prints its
URL. It posts nothing, and says why, when the description or the notes is
empty or has a heading, when the results don't have the test failing or
hanging, or when it failed every run at some pair. When an open Bugs
discussion or an open story already names the test, it prints that report's
link and exits 2. It exits 1 when it posts nothing for any other reason.
`

// reportCommand runs flaky report, reaching GitHub through what github
// returns for the repository's root.
func reportCommand(ctx context.Context, p bedrock.Process, args []string, github func(root string) report.GitHub) bedrock.ExitStatus {
	var req report.Request
	fs := flag.NewFlagSet("flaky report", flag.ContinueOnError)
	fs.SetOutput(p.Stderr)
	fs.Usage = func() {
		say(fs.Output(), "%s\nflags:\n", reportUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&req.Results, "results", "", "the file flaky stress -json wrote (required)")
	fs.StringVar(&req.Package, "package", "", "the test's package: its directory from the repository's root, or its import path (required)")
	fs.StringVar(&req.Test, "test", "", "the test's full name, such as TestX or TestX/a_case (required)")
	fs.StringVar(&req.Description, "description", "", "a file of Markdown with no headings: what the test checks and how it fails (required)")
	fs.StringVar(&req.Notes, "notes", "", "a file of Markdown with no headings: anything else worth saying")
	fs.BoolVar(&req.DryRun, "dry-run", false, "print the title and body rather than creating the discussion")
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return bedrock.Exit(0)
	case err != nil:
		say(p.Stderr, "flaky report: %v\n", err)
		return bedrock.ExitWithCause(1, err)
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("flaky report takes no arguments, but was given %q", fs.Args())
		say(p.Stderr, "flaky report: %v\n", err)
		return bedrock.ExitWithCause(1, err)
	}
	req.Env = env.Child(p.Environment)
	err := func() error {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		if req.Root, err = discover.Root(ctx, wd, req.Env); err != nil {
			return err
		}
		return report.File(ctx, github(req.Root), req, p.Stdout)
	}()
	var open *report.OpenError
	switch {
	case errors.As(err, &open):
		say(p.Stderr, "flaky report: %v\n", err)
		say(p.Stdout, "%s\n", open.Report.URL)
		return bedrock.ExitWithCause(2, err)
	case err != nil:
		say(p.Stderr, "flaky report: %v\n", err)
		return bedrock.ExitWithCause(1, err)
	}
	return bedrock.Exit(0)
}

// pkgWalkthroughs returns the walkthroughs that the packages at the
// functional level, of pkgs, are the functional tests of, and for each
// package, the index of its walkthrough among them, or -1 when it isn't at the
// functional level. A package at the functional level outside a walkthrough
// has no cluster to run on, and is an error.
func pkgWalkthroughs(root string, modules []string, pkgs []discover.Package) ([]discover.Walkthrough, []int, error) {
	walkOf := make([]int, len(pkgs))
	if !slices.ContainsFunc(pkgs, func(pkg discover.Package) bool { return pkg.Level == functional }) {
		for i := range walkOf {
			walkOf[i] = -1
		}
		return nil, walkOf, nil
	}
	all, err := discover.Walkthroughs(root, modules)
	if err != nil {
		return nil, nil, err
	}
	var used []discover.Walkthrough
	for i, pkg := range pkgs {
		walkOf[i] = -1
		if pkg.Level != functional {
			continue
		}
		j := slices.IndexFunc(all, func(w discover.Walkthrough) bool { return w.Module == pkg.Module })
		if j < 0 {
			return nil, nil, fmt.Errorf("%s has functional tests, but its module %s isn't a walkthrough, with a Dockerfile and a deploy/kind/up.sh, so it has no cluster to run on", pkg.Dir, pkg.Module)
		}
		k := slices.Index(used, all[j])
		if k < 0 {
			k = len(used)
			used = append(used, all[j])
		}
		walkOf[i] = k
	}
	return used, walkOf, nil
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
	// walkthroughs are the walkthroughs whose clusters the packages at the
	// functional level run on, and walkOf the index of each package's, or
	// -1. log is where a cluster's coming up and going down is written.
	walkthroughs []discover.Walkthrough
	walkOf       []int
	log          io.Writer
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
	if err := compileRun(f.run); err != nil {
		return nil, err
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
		Env:        r.runEnv(pkg),
		Test2JSON:  r.test2json,
	})
}

// runEnv returns the environment of a run of the package pkg: a run on a
// walkthrough's cluster has KIND_CLUSTER_NAME set to the cluster's name, as
// up.sh did.
func (r *execRunner) runEnv(pkg int) []string {
	if r.walkOf == nil || r.walkOf[pkg] < 0 {
		return r.env
	}
	return env.With(r.env, "KIND_CLUSTER_NAME", r.clusterName(r.walkOf[pkg]))
}

func (r *execRunner) clusterName(w int) string {
	return docker.ClusterName(r.walkthroughs[w].Name)
}

// Image builds the image of the walkthrough w from its Dockerfile, with the
// repository's root as the context, tagged with its directory's name.
func (r *execRunner) Image(ctx context.Context, w int) error {
	wt := r.walkthroughs[w]
	if err := docker.Build(ctx, r.root, wt.Dockerfile(r.root), wt.Name, r.env); err != nil {
		return fmt.Errorf("building the image of the walkthrough %s: %w", wt.Module, err)
	}
	return nil
}

// Up brings up the cluster flaky-<walkthrough> for the walkthrough w, pinned
// to cores, with its up.sh.
func (r *execRunner) Up(ctx context.Context, w int, cores []int) error {
	wt := r.walkthroughs[w]
	name := r.clusterName(w)
	say(r.log, "bringing %s up for %s on cores %s\n", name, wt.Module, run.CoreList(cores))
	ctx, cancel := context.WithTimeout(ctx, upTimeout)
	defer cancel()
	err := docker.Up(ctx, docker.Cluster{
		Name:  name,
		Up:    wt.Up(r.root),
		Dir:   filepath.Join(r.root, filepath.FromSlash(wt.Module)),
		Cores: cores,
	}, r.env)
	if err != nil {
		return fmt.Errorf("bringing %s up for the walkthrough %s: %w", name, wt.Module, err)
	}
	return nil
}

// Down deletes the cluster of the walkthrough w, though ctx is done, so an
// interrupted sweep leaves no cluster behind.
func (r *execRunner) Down(ctx context.Context, w int) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), downTimeout)
	defer cancel()
	if err := docker.Down(ctx, r.clusterName(w), r.env); err != nil {
		return fmt.Errorf("deleting %s: %w", r.clusterName(w), err)
	}
	return nil
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

// compileRun returns an error when a part of the -run pattern run, at any
// level of the tests' names, doesn't compile, as the test binary would fail
// on.
func compileRun(run string) error {
	for rest, more := run, run != ""; more; {
		var part string
		part, rest, more = splitRun(rest)
		if _, err := regexp.Compile(part); err != nil {
			return fmt.Errorf("-run: %w", err)
		}
	}
	return nil
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
