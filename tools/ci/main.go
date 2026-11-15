// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command ci is the repository's CI tooling. CI runs it from the repository,
// with go -C tools/ci run . and a command:
//
//	ci modules
//
// ci modules prints, one per line and relative to the repository's root, the
// directory of every Go module in the repository that holds at least one Go
// package. `./...` does not descend into a nested module, so CI lints and
// tests each module listed here on its own. A module with no Go packages,
// such as the Hugo module under docs/, has nothing to lint or test; it is
// reported on stderr and left out. A go.mod under a testdata directory, or
// under a directory whose name begins with . or _, is a test's fixture
// rather than a module of the repository, as the Go tool ignores those
// directories too. The modules under tools/ are listed like any other, so CI
// lints and tests them.
//
//	ci affected [--base <rev>] [--json]
//
// ci affected prints, one per line and in the order ci modules prints them,
// the directory of every module ci modules lists that the change from the
// commit <rev> to HEAD affects, as package affected decides, and on stderr
// the changed file that affects each. With no --base, or an empty one, there
// is no change to compare, and every module is affected. With --json it
// prints instead one JSON object, of arrays of those directories in the same
// order: modules, the modules the change affects; lint, the modules whose
// lint and unit tests run; and, for each tag of affected.Tags, vet_<tag>, the
// modules whose tests behind the tag are vetted. On stderr it then says too
// why each module of lint that the change doesn't affect is there. It exits 0
// when it decided, whatever it decided, 1 when it could not, such as when git
// cannot list the change's files or go list fails in a module, and 2 on a
// usage error. CI runs a module's jobs only when the module is printed, and
// every one of them when ci affected exits non-zero.
//
//	ci coverage-gate MIN PACKAGES PROFILE
//
// ci coverage-gate fails when any package listed in the file PACKAGES, one
// import path per line, has less than MIN percent statement coverage in the
// file PROFILE, a Go cover profile that may merge the profiles of several
// modules. An empty PACKAGES gates nothing and passes, saying nothing. A
// package with no statements passes. A package with statements but no tests
// is in the profile with every block unexecuted, so it counts as uncovered
// rather than being skipped. Statements are compared exactly, so 79.96% does
// not pass by rounding to 80.0%. It exits 0 when every package passes, 1 when
// one does not, and 2 on a usage error or a file it cannot read. go -C runs
// it in tools/ci, so a relative PACKAGES or PROFILE is relative to tools/ci.
//
//	ci release [--bump | --dry-run | --list] [--commit <rev>] [--remote <name>] <version>
//
// ci release releases every Go module in the repository at one version, as
// package release describes. <version> is vMAJOR.MINOR.PATCH with an
// optional pre-release suffix, such as v0.16.0 or v0.16.0-rc.1. With no mode
// it checks the release commit and creates a tag for every module at it:
// <version> for the root module and <dir>/<version> for each nested module.
// --bump sets every requirement one humus module has on another to
// <version>, in the go.mod files of the working tree, those of the modules
// under example/ and tools/ included, and, unless <version> is a
// pre-release, every humus version the docs name. --dry-run makes every
// check, prints the requirements --bump would set, in the examples' and
// tools' go.mod files too, each doc line it would change, and the tags that
// would be created, and changes nothing. --list prints the path of every
// module the release tags, one per line, and touches no remote. --commit
// names the release commit, HEAD by default; --bump works on the working
// tree, so it takes no --commit. --remote names the remote tags are read
// from and pushed to, origin by default. It exits 1 when a check fails, and
// 2 on a usage error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/ci/affected"
	"github.com/z5labs/humus/tools/ci/coverage"
	"github.com/z5labs/humus/tools/ci/modules"
	"github.com/z5labs/humus/tools/ci/release"
)

func main() {
	bedrock.Main(bedrock.AppFunc(ci))
}

const usage = `usage: ci modules
       ci affected [--base <rev>] [--json]
       ci coverage-gate MIN PACKAGES PROFILE
       ci release [--bump | --dry-run | --list] [--commit <rev>] [--remote <name>] <version>
`

func ci(ctx context.Context, p bedrock.Process) bedrock.ExitStatus {
	if len(p.Args) < 2 {
		say(p.Stderr, "%s", usage)
		return bedrock.ExitWithCause(2, errors.New("no command"))
	}
	switch p.Args[1] {
	case "modules":
		return modulesCommand(ctx, p, p.Args[2:])
	case "affected":
		return affectedCommand(ctx, p, p.Args[2:])
	case "coverage-gate":
		return coverageGateCommand(p, p.Args[2:])
	case "release":
		return releaseCommand(ctx, p, p.Args[2:])
	case "-h", "-help", "--help", "help":
		say(p.Stdout, "%s", usage)
		return bedrock.Exit(0)
	}
	say(p.Stderr, "ci: unknown command %q\n%s", p.Args[1], usage)
	return bedrock.ExitWithCause(2, fmt.Errorf("unknown command %q", p.Args[1]))
}

func modulesCommand(ctx context.Context, p bedrock.Process, args []string) bedrock.ExitStatus {
	if len(args) != 0 {
		say(p.Stderr, "%s", usage)
		return bedrock.ExitWithCause(2, errors.New("ci modules takes no arguments"))
	}
	l, err := modules.List(ctx, ".", nil)
	if err != nil {
		say(p.Stderr, "ci modules: %v\n", err)
		return bedrock.ExitWithCause(1, err)
	}
	for _, dir := range l.Empty {
		say(p.Stderr, "ci modules: %s holds no Go packages; skipping it\n", dir)
	}
	for _, dir := range l.Dirs {
		say(p.Stdout, "%s\n", dir)
	}
	return bedrock.Exit(0)
}

func affectedCommand(ctx context.Context, p bedrock.Process, args []string) bedrock.ExitStatus {
	var base string
	baseSet, asJSON := false, false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--base" && !baseSet && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
			i++
			base, baseSet = args[i], true
		case args[i] == "--json" && !asJSON:
			asJSON = true
		default:
			say(p.Stderr, "%s\n", "usage: ci affected [--base <rev>] [--json]")
			return bedrock.ExitWithCause(2, errors.New("ci affected takes only --base <rev> and --json"))
		}
	}
	plan, err := affected.Run(ctx, ".", base, nil)
	if err != nil {
		printErr(p.Stderr, "ci affected", err)
		return bedrock.ExitWithCause(1, err)
	}
	if base == "" {
		say(p.Stderr, "ci affected: no base to compare HEAD with, so every module is affected\n")
	}
	for _, a := range plan.Affected {
		if a.Because != "" {
			say(p.Stderr, "ci affected: %s: %s\n", a.Dir, a.Because)
		}
	}
	if !asJSON {
		for _, a := range plan.Affected {
			say(p.Stdout, "%s\n", a.Dir)
		}
		return bedrock.Exit(0)
	}
	out := map[string][]string{"modules": dirs(plan.Affected), "lint": dirs(plan.Linted)}
	for _, a := range plan.Linted {
		if !slices.ContainsFunc(plan.Affected, func(b affected.Affected) bool { return b.Dir == a.Dir }) {
			say(p.Stderr, "ci affected: %s: lint and unit tests only: %s\n", a.Dir, a.Because)
		}
	}
	for tag, ds := range plan.Vetted {
		out["vet_"+tag] = ds
	}
	b, err := json.Marshal(out)
	if err != nil {
		printErr(p.Stderr, "ci affected", err)
		return bedrock.ExitWithCause(1, err)
	}
	say(p.Stdout, "%s\n", b)
	return bedrock.Exit(0)
}

// dirs returns the directory of each of l, as an empty array rather than
// none when l is empty.
func dirs(l []affected.Affected) []string {
	out := make([]string, len(l))
	for i, a := range l {
		out[i] = a.Dir
	}
	return out
}

func coverageGateCommand(p bedrock.Process, args []string) bedrock.ExitStatus {
	fail := func(format string, a ...any) bedrock.ExitStatus {
		err := fmt.Errorf(format, a...)
		say(p.Stderr, "ci coverage-gate: %v\n", err)
		return bedrock.ExitWithCause(2, err)
	}
	if len(args) != 3 {
		say(p.Stderr, "%s\n", "usage: ci coverage-gate MIN PACKAGES PROFILE")
		return bedrock.ExitWithCause(2, errors.New("ci coverage-gate takes three arguments"))
	}
	min, err := percent(args[0])
	if err != nil {
		return fail("MIN must be an integer from 0 to 100, got: %s", args[0])
	}
	packages, err := os.ReadFile(args[1])
	if err != nil {
		return fail("cannot read %s: %w", args[1], err)
	}
	profile, err := os.Open(args[2])
	if err != nil {
		return fail("cannot read %s: %w", args[2], err)
	}
	defer func() { _ = profile.Close() }()
	passed, err := coverage.Gate(p.Stdout, min, strings.Split(string(packages), "\n"), profile)
	if err != nil {
		return fail("%s: %w", args[2], err)
	}
	if !passed {
		return bedrock.ExitWithCause(1, errors.New("a package is below the minimum coverage"))
	}
	return bedrock.Exit(0)
}

// percent parses s, an integer from 0 to 100 written in decimal digits alone.
func percent(s string) (int, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, errors.New("not a percentage")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > 100 {
		return 0, errors.New("not a percentage")
	}
	return n, nil
}

func releaseCommand(ctx context.Context, p bedrock.Process, args []string) bedrock.ExitStatus {
	o, err := releaseOptions(args)
	if err != nil {
		say(p.Stderr, "%s\n", "usage: ci release [--bump | --dry-run | --list] [--commit <rev>] [--remote <name>] <version>")
		return bedrock.ExitWithCause(2, err)
	}
	if err := release.Run(ctx, ".", o, p.Stdout, p.Stderr); err != nil {
		printErr(p.Stderr, "ci release", err)
		return bedrock.ExitWithCause(1, err)
	}
	return bedrock.Exit(0)
}

// releaseOptions parses the arguments of ci release, in any order: at most
// one mode, the version once, and --commit and --remote each followed by its
// value.
func releaseOptions(args []string) (release.Options, error) {
	var o release.Options
	modeSet := false
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--bump", "--dry-run", "--list":
			if modeSet {
				return o, errors.New("more than one mode")
			}
			modeSet = true
			o.Mode = map[string]release.Mode{"--bump": release.Bump, "--dry-run": release.DryRun, "--list": release.List}[arg]
		case "--commit", "--remote":
			if i+1 == len(args) {
				return o, fmt.Errorf("%s needs a value", arg)
			}
			i++
			if arg == "--commit" {
				o.Commit = args[i]
			} else {
				o.Remote = args[i]
			}
		default:
			if strings.HasPrefix(arg, "-") {
				return o, fmt.Errorf("unknown flag %s", arg)
			}
			if o.Version != "" {
				return o, errors.New("more than one version")
			}
			o.Version = arg
		}
	}
	if o.Version == "" {
		return o, errors.New("no version")
	}
	if o.Mode == release.Bump && o.Commit != "" {
		return o, errors.New("--bump takes no --commit")
	}
	return o, nil
}

// printErr writes each line of err to w, after prefix.
func printErr(w io.Writer, prefix string, err error) {
	for line := range strings.SplitSeq(err.Error(), "\n") {
		say(w, "%s: %s\n", prefix, line)
	}
}

// say writes to w, which reports what the command does; a write that fails
// changes nothing the command does.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
