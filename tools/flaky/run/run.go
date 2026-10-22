// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package run builds a package's test binary, and runs it pinned to its cores
// and reads each test's result from its output.
package run

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Outcome is how a test ended in one run.
type Outcome int

const (
	// Pass means the test passed.
	Pass Outcome = iota
	// Skip means the test was skipped.
	Skip
	// Fail means the test failed, or the run ended while it was running.
	Fail
	// Hang means the test was still running at the run's timeout.
	Hang
)

func (o Outcome) String() string {
	switch o {
	case Pass:
		return "pass"
	case Skip:
		return "skip"
	case Fail:
		return "fail"
	case Hang:
		return "hang"
	}
	return "outcome(" + strconv.Itoa(int(o)) + ")"
}

// Test is one test's result in one run.
type Test struct {
	// Name is the test's full name, such as TestX/a_case.
	Name    string
	Outcome Outcome
	// Output is the test's own lines of output.
	Output []string
}

// Result is what one run of a test binary did.
type Result struct {
	// Tests are the tests the run started, in the order they started.
	Tests []Test
	// PackageFailed reports that the run failed outside any test, such as
	// in TestMain.
	PackageFailed bool
	// PackageOutput is the output that belongs to no test.
	PackageOutput []string
	// Output is the run's whole output.
	Output []byte
	// TimedOut reports that the binary panicked at its timeout.
	TimedOut bool
	// Killed reports that the run was still going after its timeout and
	// grace, and was killed.
	Killed bool
	// Cancelled reports that the run was stopped because its context was
	// done, so its result says nothing about the tests.
	Cancelled bool
	// Took is how long the run took.
	Took time.Duration
	// Seed is the run's -test.shuffle seed, or nil when it ran its tests in
	// their order.
	Seed *int64
}

// Failed reports whether anything in the run failed or hung.
func (r Result) Failed() bool {
	if r.PackageFailed {
		return true
	}
	for _, t := range r.Tests {
		if t.Outcome == Fail || t.Outcome == Hang {
			return true
		}
	}
	return false
}

// Spec is one run of a test binary.
type Spec struct {
	// Binary is the test binary's path.
	Binary string
	// Dir is the package's directory, which the binary runs in.
	Dir string
	// ImportPath is the package's import path.
	ImportPath string
	// Cores are the cores the run is pinned to.
	Cores []int
	// CPU is the run's -test.cpu.
	CPU int
	// Timeout is the run's -test.timeout.
	Timeout time.Duration
	// Grace is how long after Timeout the run is killed.
	Grace time.Duration
	// Run and Skip are the run's -test.run and -test.skip, when set.
	Run, Skip string
	// Seed is the run's -test.shuffle seed, or nil to run the tests in
	// their order.
	Seed *int64
	// Env is the run's environment.
	Env []string
	// Test2JSON is the path of the test2json command.
	Test2JSON string
}

// Command returns the run's command line: taskset, then the binary and its
// flags.
func (s Spec) Command() []string {
	args := []string{
		"taskset", "-c", CoreList(s.Cores), s.Binary,
		"-test.count=1",
		"-test.cpu=" + strconv.Itoa(s.CPU),
		"-test.timeout=" + s.Timeout.String(),
		"-test.v=test2json",
	}
	if s.Seed != nil {
		args = append(args, "-test.shuffle="+strconv.FormatInt(*s.Seed, 10))
	}
	if s.Run != "" {
		args = append(args, "-test.run="+s.Run)
	}
	if s.Skip != "" {
		args = append(args, "-test.skip="+s.Skip)
	}
	return args
}

// CoreList formats cores as taskset's -c list, such as 4,5,6,7.
func CoreList(cores []int) string {
	parts := make([]string, len(cores))
	for i, c := range cores {
		parts[i] = strconv.Itoa(c)
	}
	return strings.Join(parts, ",")
}

// Exec runs s and returns its result, which keeps s's seed. A run that can't
// be started, or whose output test2json can't read, fails outside any test,
// with the error as its output.
func Exec(ctx context.Context, s Spec) Result {
	res := execute(ctx, s)
	res.Seed = s.Seed
	return res
}

func execute(ctx context.Context, s Spec) Result {
	start := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, s.Timeout+s.Grace)
	defer cancel()

	args := s.Command()
	cmd := exec.CommandContext(runCtx, args[0], args[1:]...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// The binary may start processes of its own, so the whole group is
	// killed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	took := time.Since(start)
	exited0 := err == nil

	if ctx.Err() != nil {
		return Result{Cancelled: true, Output: out.Bytes(), Took: took}
	}
	killed := runCtx.Err() != nil
	var exitErr *exec.ExitError
	if err != nil && !killed && !errors.As(err, &exitErr) {
		return failed(out.Bytes(), took, fmt.Errorf("running %s: %w", strings.Join(args, " "), err))
	}

	events, err := convert(ctx, s, out.Bytes())
	if err != nil {
		return failed(out.Bytes(), took, err)
	}
	res := Parse(events, exited0, killed)
	res.Output = out.Bytes()
	res.Took = took
	return res
}

func failed(out []byte, took time.Duration, err error) Result {
	out = append(slices.Clip(out), []byte(err.Error()+"\n")...)
	return Result{
		PackageFailed: true,
		PackageOutput: lines(string(out)),
		Output:        out,
		Took:          took,
	}
}

// Event is one event of test2json's output.
type Event struct {
	Action string
	Test   string
	Output string
}

// convert reads the run's output through test2json.
func convert(ctx context.Context, s Spec, out []byte) ([]Event, error) {
	cmd := exec.CommandContext(ctx, s.Test2JSON, "-p", s.ImportPath)
	cmd.Env = s.Env
	cmd.Stdin = bytes.NewReader(out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	converted, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("test2json: %w: %s", err, stderr.String())
	}
	return Decode(bytes.NewReader(converted))
}

// Decode reads test2json's events from r.
func Decode(r io.Reader) ([]Event, error) {
	var events []Event
	dec := json.NewDecoder(r)
	for {
		var e Event
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading test2json's output: %w", err)
		}
		events = append(events, e)
	}
}

// Parse turns a run's events into its result. exited0 reports that the
// binary exited with status 0, and killed that it was killed after its
// timeout and grace.
//
// A test still running at the timeout's panic, or when the run was killed,
// hangs. A test the run started and never ended otherwise fails, since the
// binary exited under it. A test fails or hangs only for itself: a test whose
// subtest failed or hung in the same run passes, so a failure is counted once,
// on the test that failed. The run fails outside any test when the binary
// didn't exit 0 and no test failed or hung.
func Parse(events []Event, exited0, killed bool) Result {
	res := Result{Killed: killed}
	index := map[string]int{}
	ended := map[string]bool{}
	var text strings.Builder
	for _, e := range events {
		switch e.Action {
		case "run":
			if _, ok := index[e.Test]; !ok {
				index[e.Test] = len(res.Tests)
				res.Tests = append(res.Tests, Test{Name: e.Test})
			}
		case "pass", "skip", "fail":
			i, ok := index[e.Test]
			if !ok {
				continue
			}
			ended[e.Test] = true
			res.Tests[i].Outcome = map[string]Outcome{"pass": Pass, "skip": Skip, "fail": Fail}[e.Action]
		case "output":
			text.WriteString(e.Output)
			line := strings.TrimSuffix(e.Output, "\n")
			if i, ok := index[e.Test]; ok {
				res.Tests[i].Output = append(res.Tests[i].Output, line)
			} else {
				res.PackageOutput = append(res.PackageOutput, line)
			}
		}
	}

	running := TimedOut(text.String())
	res.TimedOut = running != nil
	for i, t := range res.Tests {
		switch {
		case slices.Contains(running, t.Name):
			res.Tests[i].Outcome = Hang
		case !ended[t.Name] && (killed || res.TimedOut):
			res.Tests[i].Outcome = Hang
		case !ended[t.Name]:
			res.Tests[i].Outcome = Fail
		}
	}
	for i, t := range res.Tests {
		if t.Outcome != Fail && t.Outcome != Hang {
			continue
		}
		for _, sub := range res.Tests {
			if strings.HasPrefix(sub.Name, t.Name+"/") && (sub.Outcome == Fail || sub.Outcome == Hang) {
				res.Tests[i].Outcome = Pass
				break
			}
		}
	}
	res.PackageFailed = !exited0 && !res.Failed()
	return res
}

// TimedOut returns the tests a binary's timeout panic names as still
// running, or nil when out holds no such panic.
func TimedOut(out string) []string {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(nil, 1<<20)
	state := 0
	running := []string{}
	for sc.Scan() {
		line := sc.Text()
		switch state {
		case 0:
			if strings.HasPrefix(line, "panic: test timed out after ") {
				state = 1
			}
		case 1:
			if strings.TrimSpace(line) != "running tests:" {
				return running
			}
			state = 2
		case 2:
			name, ok := strings.CutPrefix(line, "\t\t")
			if !ok {
				return running
			}
			if i := strings.LastIndex(name, " ("); i >= 0 {
				name = name[:i]
			}
			running = append(running, name)
		}
	}
	if state == 0 {
		return nil
	}
	return running
}

func lines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// Build builds the test binary of the package importPath, in the module
// directory dir, with CI's flags, to out.
func Build(ctx context.Context, dir, importPath, out string, env []string) error {
	cmd := exec.CommandContext(ctx, "go", "test", "-c", "-race", "-cover", "-o", out, importPath)
	cmd.Dir = dir
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("building %s: %w\n%s", importPath, err, output)
	}
	return nil
}

// List returns the top-level tests that a test binary lists, run in the
// package's directory dir. Benchmarks are left out, since they don't run.
func List(ctx context.Context, binary, dir string, env []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, binary, "-test.list", ".")
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing the tests of %s: %w\n%s%s", binary, err, out, stderr.Bytes())
	}
	var tests []string
	for _, name := range strings.Fields(string(out)) {
		if !strings.HasPrefix(name, "Benchmark") {
			tests = append(tests, name)
		}
	}
	return tests, nil
}

// Test2JSON returns the path of the go command's test2json, building it if
// it isn't yet.
func Test2JSON(ctx context.Context, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "tool", "-n", "test2json")
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("finding test2json: %w: %s", err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// Affinity returns the cores this process may run on.
func Affinity() ([]int, error) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return nil, fmt.Errorf("reading the cores this process may run on: %w", err)
	}
	var cores []int
	for i := range len(set) * 64 {
		if set.IsSet(i) {
			cores = append(cores, i)
		}
	}
	return cores, nil
}
