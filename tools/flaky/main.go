// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command flaky runs the repository's tests until they fail, so a flaky test
// is found before it turns CI red. A maintainer runs it by hand, on Linux.
//
//	flaky stress [-cpu 1,2,4] [-load 1,2] [-count n] [-confirm n] [-timeout d]
//	             [-keep n] [-shuffle] [-run regexp] [-skip regexp] [-o dir]
//	             [-json] [packages]
//
// flaky stress builds every package's test binary with go test -c -race
// -cover, then runs each one -count times at each pair of -cpu and -load
// values. Each run is pinned with taskset to as many cores as its CPU count,
// so both GOMAXPROCS and runtime.NumCPU() are that count, as on a GitHub
// runner with that many vCPUs. At load 1 a run has its cores to itself; at
// load L, up to L runs are pinned to each of its cores, so they compete for
// them.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/flaky/discover"
	"github.com/z5labs/humus/tools/flaky/internal/env"
	"github.com/z5labs/humus/tools/flaky/lanes"
	"github.com/z5labs/humus/tools/flaky/results"
	"github.com/z5labs/humus/tools/flaky/run"
	"github.com/z5labs/humus/tools/flaky/stress"
)

const (
	defaultTimeout = 2 * time.Minute
	// grace is how long after its timeout a run still going is killed.
	grace = 30 * time.Second
)

func main() {
	bedrock.Main(bedrock.AppFunc(flaky))
}

const usage = `usage: flaky stress [flags] [packages]

flaky stress runs every package's tests -count times at each pair of -cpu and
-load values, each run pinned to as many cores as its CPU count and sharing
each of them with up to its load of runs. Then it runs each test that failed
or hung alone, -confirm times at each pair, and reports each test that failed
or hung. With -run, it runs the matching tests -count times at each pair, and
confirms none. With -shuffle, each run shuffles its tests with a seed of its
own.
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
	cpus     []int
	loads    []int
	count    int
	confirm  int
	timeout  time.Duration
	keep     int
	shuffle  shuffle
	run      string
	skip     string
	out      string
	json     bool
	patterns []string
}

func parseStress(p bedrock.Process, args []string) (stressFlags, error) {
	f := stressFlags{cpus: []int{1, 2, 4}, loads: []int{1, 2}}
	fs := flag.NewFlagSet("flaky stress", flag.ContinueOnError)
	fs.SetOutput(p.Stderr)
	fs.Usage = func() {
		say(fs.Output(), "%s\nflags:\n", usage)
		fs.PrintDefaults()
	}
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
	fs.IntVar(&f.count, "count", 500, "how many times to run each package at each pair of CPU count and load")
	fs.IntVar(&f.confirm, "confirm", 1000, "how many times to run each test that failed or hung alone, at each pair of CPU count and load, once the sweep has ended; with -run, none is")
	fs.DurationVar(&f.timeout, "timeout", defaultTimeout, "each run's -test.timeout; a run still going 30s later is killed")
	fs.IntVar(&f.keep, "keep", 8, "how many cores to keep back from the lanes")
	fs.Var(&f.shuffle, "shuffle", "shuffle each run's tests with a seed of its own, or with -shuffle=<seed>, every run's with that seed")
	fs.StringVar(&f.run, "run", "", "run only the tests that match, as go test -run")
	fs.StringVar(&f.skip, "skip", "", "skip the tests that match, as go test -skip")
	fs.StringVar(&f.out, "o", "", "the directory to save failures' output under (default $TMPDIR/flaky-<time>)")
	fs.BoolVar(&f.json, "json", false, "write the report as JSON")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if f.count < 1 {
		return f, fmt.Errorf("-count must be at least 1, not %d", f.count)
	}
	if f.confirm < 1 {
		return f, fmt.Errorf("-confirm must be at least 1, not %d", f.confirm)
	}
	if f.timeout <= 0 {
		return f, fmt.Errorf("-timeout must be positive, not %s", f.timeout)
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
		say(p.Stderr, "%s\n", warning)
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
	pkgs, err := discover.Packages(ctx, root, modules, f.patterns, childEnv)
	if err != nil {
		return results.Report{}, err
	}
	discovery := time.Since(start)
	say(p.Stderr, "found %d packages with tests in %s\n", len(pkgs), discovery.Round(time.Millisecond))

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
			say(p.Stderr, "warning: %v\n", err)
		}
	}()
	test2json, err := run.Test2JSON(ctx, childEnv)
	if err != nil {
		return results.Report{}, err
	}

	r := &execRunner{
		root: root, pkgs: pkgs, env: childEnv, test2json: test2json,
		timeout: f.timeout, run: f.run, skip: f.skip, shuffle: f.shuffle,
		bins: make([]string, len(pkgs)),
	}
	for i, pkg := range pkgs {
		r.bins[i] = filepath.Join(bins, fmt.Sprintf("%d-%s.test", i, filepath.Base(pkg.Dir)))
	}
	confirm := f.confirm
	if f.run != "" {
		// -run asks for the matching tests at -count, so there is no
		// sweep to confirm.
		confirm = 0
	}
	res := results.New(results.Config{
		Packages: pkgs, CPUs: f.cpus, Loads: f.loads, Count: f.count, Confirm: confirm,
		Timeout: f.timeout, DefaultTimeout: defaultTimeout, Out: f.out,
		Run: f.run, Skip: f.skip,
	})
	phases, err := stress.Stress(ctx, stress.Config{
		Packages: len(pkgs),
		CPUs:     f.cpus,
		Loads:    f.loads,
		Count:    f.count,
		Confirm:  confirm,
		Lanes:    laneCores,
		Builds:   max(len(cores)/4, 1),
		Log:      p.Stderr,
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
	if confirm > 0 {
		rep.Phases = append(rep.Phases, results.NewPhase("confirm", phases.Confirm))
	}
	return rep, nil
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
	timeout   time.Duration
	run, skip string
	shuffle   shuffle
}

func (r *execRunner) Build(ctx context.Context, pkg int) error {
	return run.Build(ctx, filepath.Join(r.root, r.pkgs[pkg].Module), r.pkgs[pkg].ImportPath, r.bins[pkg], r.env)
}

func (r *execRunner) List(ctx context.Context, pkg int) ([]string, error) {
	return run.List(ctx, r.bins[pkg], filepath.Join(r.root, r.pkgs[pkg].Dir), r.env)
}

// Run runs the package pkg's binary once. A confirm run runs test alone, by
// its own -run pattern; a sweep run, with test "", runs what -run matches.
// With -shuffle, either shuffles, with the seed -shuffle gives it.
func (r *execRunner) Run(ctx context.Context, pkg int, test string, cpu int, cores []int) run.Result {
	pattern := r.run
	if test != "" {
		pattern = results.RunPattern(test)
	}
	return run.Exec(ctx, run.Spec{
		Binary:     r.bins[pkg],
		Dir:        filepath.Join(r.root, r.pkgs[pkg].Dir),
		ImportPath: r.pkgs[pkg].ImportPath,
		Cores:      cores,
		CPU:        cpu,
		Timeout:    r.timeout,
		Grace:      grace,
		Run:        pattern,
		Skip:       r.skip,
		Seed:       r.shuffle.next(),
		Env:        r.env,
		Test2JSON:  r.test2json,
	})
}

// say writes to one of the process's streams. A failed write there has
// nowhere left to be reported, so it is dropped.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
