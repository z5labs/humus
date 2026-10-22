// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command flaky runs the repository's tests until they fail, so a flaky test
// is found before it turns CI red. A maintainer runs it by hand, on Linux.
//
//	flaky stress [-cpu 1,2,4] [-count n] [-timeout d] [-keep n]
//	             [-run regexp] [-skip regexp] [-o dir] [-json] [packages]
//
// flaky stress builds every package's test binary with go test -c -race
// -cover, then runs each one -count times at each -cpu value. Each run is
// pinned with taskset to as many cores as its CPU count, so both GOMAXPROCS
// and runtime.NumCPU() are that count, as on a GitHub runner with that many
// vCPUs. It reports each test that failed or hung, and exits 1 if any did.
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

flaky stress runs every package's tests -count times at each -cpu value, each
run pinned to as many cores as its CPU count, and reports each test that failed
or hung.
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
	count    int
	timeout  time.Duration
	keep     int
	run      string
	skip     string
	out      string
	json     bool
	patterns []string
}

func parseStress(p bedrock.Process, args []string) (stressFlags, error) {
	f := stressFlags{cpus: []int{1, 2, 4}}
	fs := flag.NewFlagSet("flaky stress", flag.ContinueOnError)
	fs.SetOutput(p.Stderr)
	fs.Usage = func() {
		say(fs.Output(), "%s\nflags:\n", usage)
		fs.PrintDefaults()
	}
	fs.Func("cpu", "the CPU counts to run each package at, comma-separated (default 1,2,4)", func(s string) error {
		cpus, err := parseCPUs(s)
		f.cpus = cpus
		return err
	})
	fs.IntVar(&f.count, "count", 500, "how many times to run each package at each CPU count")
	fs.DurationVar(&f.timeout, "timeout", defaultTimeout, "each run's -test.timeout; a run still going 30s later is killed")
	fs.IntVar(&f.keep, "keep", 8, "how many cores to keep back from the lanes")
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
	if f.timeout <= 0 {
		return f, fmt.Errorf("-timeout must be positive, not %s", f.timeout)
	}
	f.patterns = fs.Args()
	return f, nil
}

func parseCPUs(s string) ([]int, error) {
	var cpus []int
	for part := range strings.SplitSeq(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%q is not a CPU count", part)
		}
		cpus = append(cpus, n)
	}
	return cpus, nil
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
		timeout: f.timeout, run: f.run, skip: f.skip,
		bins: make([]string, len(pkgs)),
	}
	for i, pkg := range pkgs {
		r.bins[i] = filepath.Join(bins, fmt.Sprintf("%d-%s.test", i, filepath.Base(pkg.Dir)))
	}
	res := results.New(results.Config{
		Packages: pkgs, CPUs: f.cpus, Count: f.count,
		Timeout: f.timeout, DefaultTimeout: defaultTimeout, Out: f.out,
	})
	phases, err := stress.Stress(ctx, stress.Config{
		Packages: len(pkgs),
		CPUs:     f.cpus,
		Count:    f.count,
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
}

func (r *execRunner) Build(ctx context.Context, pkg int) error {
	return run.Build(ctx, filepath.Join(r.root, r.pkgs[pkg].Module), r.pkgs[pkg].ImportPath, r.bins[pkg], r.env)
}

func (r *execRunner) List(ctx context.Context, pkg int) ([]string, error) {
	return run.List(ctx, r.bins[pkg], filepath.Join(r.root, r.pkgs[pkg].Dir), r.env)
}

func (r *execRunner) Run(ctx context.Context, pkg, cpu int, cores []int) run.Result {
	return run.Exec(ctx, run.Spec{
		Binary:     r.bins[pkg],
		Dir:        filepath.Join(r.root, r.pkgs[pkg].Dir),
		ImportPath: r.pkgs[pkg].ImportPath,
		Cores:      cores,
		CPU:        cpu,
		Timeout:    r.timeout,
		Grace:      grace,
		Run:        r.run,
		Skip:       r.skip,
		Env:        r.env,
		Test2JSON:  r.test2json,
	})
}

// say writes to one of the process's streams. A failed write there has
// nowhere left to be reported, so it is dropped.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
