// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package release releases every Go module in the repository at one version.
// A nested module's versions are tags prefixed with its directory:
// grpc/v0.16.0 is version v0.16.0 of github.com/z5labs/humus/grpc. Every
// module is tagged with the same version on every release, whether or not it
// changed, after OpenTelemetry Go: one version identifies a consistent set of
// modules.
//
// The nested modules require the root module, and may require each other. A
// replace directive points each such requirement at the local directory, but
// a replace applies only in the main module, so an end user's build sees the
// required version itself. At every release, each requirement between humus
// modules therefore names the release's version. [Bump] sets them; [Tag]
// refuses a commit at which any of them names another version.
//
// The modules under example/ and tools/ are never tagged, but [Bump] sets
// their requirements on the released modules too, and [Tag] checks them with
// the rest. Every replace in such a module points at the local directories,
// so its build selects the highest version that it or any humus module it
// requires names. Once a nested module requires the root module at the
// release's version, an example still naming the last one has a go.mod that
// is out of date, and does not build.
//
// A release also sets every humus version the docs tell a reader to install
// or require, so the README and the docs site name the latest release in the
// tree its tags point at. The docs are every tracked Markdown file except any
// under testdata or a directory whose name begins with ., and a doc names a
// version as <path>@<ref>, the form go get takes, at a version, a branch such
// as main or a query such as latest, or as a go.mod line, <path> <version>,
// where <path> is in a released module. A placeholder that names no version,
// such as vX.Y.Z, and @none are left as they are. A pre-release changes none
// of them, so the docs keep naming the latest release. [Bump] sets them,
// [DryRun] prints each line it would change, and [Tag] refuses a commit at
// which one names another version.
//
// Before any tag is created, Run checks that the version is a Go module
// version of major version 0 or 1, that every nested module's path is its
// directory under the root module's path, that every requirement between
// humus modules, and every requirement an example or a tool has on a released
// module, names the version, that unless the version is a pre-release every
// humus version the docs name is the version, and that no tag already exists
// at a different commit. A tag that already exists at the release commit is
// skipped, so a re-run is safe. The tags it does create are pushed in one
// atomic push, so a failure leaves no partial set.
//
// The modules are discovered at the release commit, not from a list kept by
// hand: every tracked go.mod except any the Go tool ignores, as
// [modules.Find] finds them. A module that holds no Go files of its own, such
// as the Hugo module under docs/, has nothing to release and is skipped. So is
// every module under example/: an example is an application to copy, not a
// library to require. And so is every module under tools/: a tool is for the
// repository's maintainers, not a library to require. Neither is tagged or
// listed, but both have their requirements on the released modules set, as
// above.
package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/z5labs/humus/tools/ci/modules"
)

// BumpCommand is the command that runs a bump, which a refused release tells
// the maintainer to run.
const BumpCommand = "go -C tools/ci run . release --bump"

// Mode is what Run does.
type Mode int

const (
	// Tag checks the release commit and creates a tag for every module at
	// it: the version for the root module and <dir>/<version> for each
	// nested module.
	Tag Mode = iota
	// Bump sets every requirement one humus module has on another to the
	// version, in the go.mod files of the working tree, those of the modules
	// under example/ and tools/ included, and, unless the version is a
	// pre-release, every humus version the docs name.
	Bump
	// DryRun makes every check, prints the requirements Bump would set, in
	// the examples' and tools' go.mod files too, each doc line it would
	// change, and the tags that would be created, and changes nothing.
	DryRun
	// List prints the path of every module the release tags, one per line,
	// and touches no remote.
	List
)

// Options is what Run releases, and how.
type Options struct {
	// Mode is what Run does.
	Mode Mode
	// Version is the version to release: vMAJOR.MINOR.PATCH with an
	// optional pre-release suffix, such as v0.16.0 or v0.16.0-rc.1.
	Version string
	// Commit is the release commit, HEAD when it is "". Bump works on the
	// working tree, so it takes no Commit.
	Commit string
	// Remote is the remote tags are read from and pushed to, origin when it
	// is "".
	Remote string
	// Env is the environment of git and go, as in exec.Cmd: nil is this
	// process's.
	Env []string
}

// Run releases the repository that holds dir, as o says. It writes what it
// does to stdout, and each module it skips to stderr. Any error it returns
// holds one problem per line.
func Run(ctx context.Context, dir string, o Options, stdout, stderr io.Writer) error {
	if o.Mode == Bump && o.Commit != "" {
		return errors.New("a bump works on the working tree, so it takes no commit")
	}
	prerelease, err := checkVersion(o.Version)
	if err != nil {
		return err
	}
	remote := o.Remote
	if remote == "" {
		remote = "origin"
	}
	r := &runner{ctx: ctx, env: o.Env, stdout: stdout, stderr: stderr, version: o.Version}

	out, err := r.git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	r.root = strings.TrimSpace(out)

	// A bump reads and edits the working tree; every other mode reads the
	// release commit, so a module added after it is not released with it.
	var sha string
	if o.Mode == Bump {
		out, err = r.git(r.root, "ls-files", "-z")
		r.show = func(name string) (string, error) {
			b, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(name)))
			return string(b), err
		}
	} else {
		commit := o.Commit
		if commit == "" {
			commit = "HEAD"
		}
		out, err = r.git(r.root, "rev-parse", "--verify", "--quiet", commit+"^{commit}")
		if err != nil {
			return fmt.Errorf("%s does not name a commit", commit)
		}
		sha = strings.TrimSpace(out)
		out, err = r.git(r.root, "ls-tree", "-r", "-z", "--full-tree", "--name-only", sha)
		r.show = func(name string) (string, error) {
			return r.git(r.root, "show", sha+":"+name)
		}
	}
	if err != nil {
		return err
	}
	tracked := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")

	set, err := r.modules(tracked)
	if err != nil {
		return err
	}

	if o.Mode == List {
		for _, dir := range set.released {
			say(stdout, "%s\n", set.path(dir))
		}
		return nil
	}

	stale, err := r.stale(set)
	if err != nil {
		return err
	}

	// A pre-release changes no doc: the docs keep naming the latest release.
	var outdated []docLine
	if prerelease == "" {
		outdated, err = r.outdated(set, tracked)
		if err != nil {
			return err
		}
	}

	if o.Mode == Bump {
		return r.bump(stale, outdated, prerelease != "")
	}

	if o.Mode == DryRun && prerelease != "" {
		say(stdout, "leave the docs alone: %s is a pre-release, so they keep naming the latest release\n", o.Version)
	}

	target := sha
	if len(stale) > 0 || len(outdated) > 0 {
		if o.Mode == Tag {
			var errs []error
			for _, s := range stale {
				errs = append(errs, fmt.Errorf("%s requires %s %s, want %s", s.file, s.path, s.have, o.Version))
			}
			for _, l := range outdated {
				errs = append(errs, fmt.Errorf("%s:%d names a humus version other than %s: %s", l.file, l.n, o.Version, trim(l.old)))
			}
			errs = append(errs, fmt.Errorf("every requirement on a released humus module, and every humus version the docs name, must be %s at the release commit %s: set them with %s %s and merge the change first", o.Version, sha, BumpCommand, o.Version))
			return errors.Join(errs...)
		}
		for _, s := range stale {
			say(stdout, "would set %s: require %s %s (now %s)\n", s.file, s.path, o.Version, s.have)
		}
		for _, l := range outdated {
			say(stdout, "would set %s:%d: %s (now %s)\n", l.file, l.n, trim(l.new), trim(l.old))
		}
		// The tags would go on the commit that merges these changes, which
		// does not exist yet, so any tag that already exists is at a
		// different commit.
		say(stdout, "release %s at the commit that makes these changes on top of %s\n", o.Version, sha)
		target = ""
	} else {
		say(stdout, "release %s at %s\n", o.Version, sha)
	}

	return r.tag(set, remote, sha, target, o.Mode == DryRun)
}

// checkVersion returns the pre-release of version, a Go module version of
// major version 0 or 1: a canonical semantic version with a leading v, an
// optional pre-release and no build metadata.
func checkVersion(version string) (prerelease string, err error) {
	if strings.Contains(version, "+") {
		return "", fmt.Errorf("%s is not a Go module version: build metadata (+...) is not allowed", version)
	}
	m := versionRE.FindStringSubmatch(version)
	if m == nil {
		return "", fmt.Errorf("%s is not a Go module version: want vMAJOR.MINOR.PATCH with an optional -pre-release suffix", version)
	}
	prerelease = m[5]
	if prerelease != "" {
		for id := range strings.SplitSeq(prerelease, ".") {
			if id == "" {
				return "", fmt.Errorf("%s is not a Go module version: its pre-release has an empty identifier", version)
			}
			if leadingZero.MatchString(id) {
				return "", fmt.Errorf("%s is not a Go module version: numeric pre-release identifier %s has a leading zero", version, id)
			}
		}
	}
	if major, err := strconv.Atoi(m[1]); err != nil || major > 1 {
		return "", fmt.Errorf("%s has major version %s: from v2 on every module path needs a /v%s suffix, which this repository does not have", version, m[1], m[1])
	}
	return prerelease, nil
}

var (
	versionRE   = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z.-]+))?$`)
	leadingZero = regexp.MustCompile(`^0[0-9]+$`)
)

// runner holds what every step of a run reads.
type runner struct {
	ctx            context.Context
	env            []string
	stdout, stderr io.Writer
	version        string
	root           string
	// show returns the contents of a file, by its path from root, as the
	// run reads it: in the working tree or at the release commit.
	show func(name string) (string, error)
}

// moduleSet is every module of the repository, and what a release does with
// each.
type moduleSet struct {
	root string
	// dirs is the directory of every module, the root's first.
	dirs []string
	// released is the directory of every module the release tags, the
	// root's first when it is tagged.
	released []string
	// pinned is the directory of every module whose requirements on the
	// released modules name the version: the released modules' and the
	// examples' and tools'.
	pinned []string
	// paths is every released module's path.
	paths map[string]bool
}

// path returns the path of the module in dir.
func (s moduleSet) path(dir string) string {
	if dir == "." {
		return s.root
	}
	return s.root + "/" + dir
}

// modules finds the modules among tracked, and which of them the release
// tags.
func (r *runner) modules(tracked []string) (moduleSet, error) {
	found, ignored := modules.Find(tracked)
	for _, ig := range ignored {
		say(r.stderr, "skip %s: under %s, which the Go tool ignores\n", ig.GoMod, ig.Under)
	}
	var set moduleSet
	hasRoot := false
	for _, dir := range found {
		if dir == "." {
			hasRoot = true
			continue
		}
		set.dirs = append(set.dirs, dir)
	}
	if !hasRoot {
		return moduleSet{}, errors.New("there is no go.mod at the root of the repository")
	}
	set.dirs = append([]string{"."}, set.dirs...)

	owners := owners(set.dirs, tracked)
	root, err := r.modulePath(".")
	if err != nil {
		return moduleSet{}, err
	}
	if root == "" {
		return moduleSet{}, errors.New("go.mod declares no module path")
	}
	set.root = root

	// An example's or a tool's module stays among dirs, so its Go files are
	// its own and not its parent's, but it is never released. Its
	// requirements on the released modules are still pinned to the version,
	// with theirs.
	for _, dir := range set.dirs {
		switch {
		case dir == "example" || strings.HasPrefix(dir, "example/"):
			say(r.stderr, "skip %s: under example/, whose modules are never released\n", modules.GoMod(dir))
			set.pinned = append(set.pinned, dir)
			continue
		case dir == "tools" || strings.HasPrefix(dir, "tools/"):
			say(r.stderr, "skip %s: under tools/, whose modules are never released\n", modules.GoMod(dir))
			set.pinned = append(set.pinned, dir)
			continue
		case !owners[dir]:
			say(r.stderr, "skip %s: it holds no Go packages\n", modules.GoMod(dir))
			continue
		}
		set.released = append(set.released, dir)
		set.pinned = append(set.pinned, dir)
	}

	// Every nested module's tag resolves only if its path is its directory
	// under the root module's path. Every mismatch is reported before
	// anything happens.
	set.paths = map[string]bool{}
	var mismatched []error
	for _, dir := range set.released {
		set.paths[set.path(dir)] = true
		if dir == "." {
			continue
		}
		p, err := r.modulePath(dir)
		if err != nil {
			return moduleSet{}, err
		}
		if p != set.path(dir) {
			if p == "" {
				p = "<none>"
			}
			mismatched = append(mismatched, fmt.Errorf("%s declares module %s, want %s: its tag %s/%s would never resolve", modules.GoMod(dir), p, set.path(dir), dir, r.version))
		}
	}
	return set, errors.Join(mismatched...)
}

// owners returns the directory of every module among dirs that holds a Go
// file of its own among tracked. A Go file belongs to the module in the
// deepest directory that holds it, and is no package at all under a
// directory the Go tool ignores, or when its own name begins with . or _.
func owners(dirs, tracked []string) map[string]bool {
	out := map[string]bool{}
	for _, file := range tracked {
		if !strings.HasSuffix(file, ".go") || ignoredGoFile(file) {
			continue
		}
		// The root module's directory is the shortest of all, however short
		// a nested module's name.
		owner, depth := ".", 0
		for _, d := range dirs {
			if d != "." && strings.HasPrefix(file, d+"/") && len(d) > depth {
				owner, depth = d, len(d)
			}
		}
		out[owner] = true
	}
	return out
}

func ignoredGoFile(file string) bool {
	parts := strings.Split(file, "/")
	for i, part := range parts {
		if part == "testdata" && i < len(parts)-1 {
			return true
		}
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return true
		}
	}
	return false
}

// modulePath returns the module path the go.mod of the module in dir
// declares, or "" when it declares none.
func (r *runner) modulePath(dir string) (string, error) {
	gomod, err := r.show(modules.GoMod(dir))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(gomod, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "module" {
			if len(f) < 2 {
				return "", nil
			}
			return strings.ReplaceAll(f[1], `"`, ""), nil
		}
	}
	return "", nil
}

// requirement is a requirement in a go.mod.
type requirement struct {
	// file is the go.mod's path from the repository's root.
	file string
	// path is the required module's path, and have the version it names.
	path, have string
}

// requirements returns every requirement in the go.mod at file, in either
// the single-line or the block form, in its order.
func requirements(file, gomod string) []requirement {
	var out []requirement
	block := false
	for line := range strings.SplitSeq(gomod, "\n") {
		f := strings.Fields(line)
		var path, have string
		switch {
		case len(f) >= 2 && f[0] == "require" && f[1] == "(":
			block = true
			continue
		case block && len(f) > 0 && f[0] == ")":
			block = false
			continue
		case block && len(f) >= 2 && !strings.HasPrefix(f[0], "//"):
			path, have = f[0], f[1]
		case !block && len(f) >= 3 && f[0] == "require":
			path, have = f[1], f[2]
		}
		if path = strings.ReplaceAll(path, `"`, ""); path != "" {
			out = append(out, requirement{file: file, path: path, have: have})
		}
	}
	return out
}

// stale returns every requirement a released module, an example or a tool
// has on a released module that does not name the version.
func (r *runner) stale(set moduleSet) ([]requirement, error) {
	var out []requirement
	for _, dir := range set.pinned {
		file := modules.GoMod(dir)
		gomod, err := r.show(file)
		if err != nil {
			return nil, err
		}
		for _, req := range requirements(file, gomod) {
			if set.paths[req.path] && req.have != r.version {
				out = append(out, req)
			}
		}
	}
	return out, nil
}

// bump sets every stale requirement and, unless the version is a
// pre-release, every outdated doc line, in the working tree.
func (r *runner) bump(stale []requirement, outdated []docLine, prerelease bool) error {
	if len(stale) == 0 {
		say(r.stdout, "every requirement on a released humus module already names %s\n", r.version)
	}
	for _, s := range stale {
		if _, err := r.command(r.root, "go", "mod", "edit", "-require="+s.path+"@"+r.version, s.file); err != nil {
			return err
		}
		say(r.stdout, "set %s: require %s %s (was %s)\n", s.file, s.path, r.version, s.have)
	}
	if prerelease {
		say(r.stdout, "leave the docs alone: %s is a pre-release, so they keep naming the latest release\n", r.version)
		return nil
	}
	if len(outdated) == 0 {
		say(r.stdout, "every humus version the docs name is already %s\n", r.version)
		return nil
	}
	var edited []string
	newLines := map[string]map[int]string{}
	for _, l := range outdated {
		say(r.stdout, "set %s:%d: %s (was %s)\n", l.file, l.n, trim(l.new), trim(l.old))
		if newLines[l.file] == nil {
			newLines[l.file] = map[int]string{}
			edited = append(edited, l.file)
		}
		newLines[l.file][l.n] = l.new
	}
	for _, file := range edited {
		doc, err := r.show(file)
		if err != nil {
			return err
		}
		var b strings.Builder
		for i, line := range lines(doc) {
			if n, ok := newLines[file][i+1]; ok {
				line = n
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		if err := os.WriteFile(filepath.Join(r.root, filepath.FromSlash(file)), []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// tag creates the tag of every released module at sha, or with dryRun says
// it would. A tag that already exists at target is skipped, and one that
// exists anywhere else fails the run; target is "" when the tags would go on
// a commit that does not exist yet.
func (r *runner) tag(set moduleSet, remote, sha, target string, dryRun bool) error {
	out, err := r.git(r.root, "ls-remote", "--tags", remote)
	if err != nil {
		say(r.stderr, "%s\n", err)
		return fmt.Errorf("could not list the tags of %s", remote)
	}
	tags := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if hash, ref, ok := strings.Cut(line, "\t"); ok {
			tags[ref] = hash
		}
	}
	// The commit an existing tag points to, peeled through any annotated tag
	// object.
	tagged := func(tag string) string {
		if peeled, ok := tags["refs/tags/"+tag+"^{}"]; ok {
			return peeled
		}
		return tags["refs/tags/"+tag]
	}

	var refspecs, created []string
	var conflicts []error
	for _, dir := range set.released {
		tag := r.version
		if dir != "." {
			tag = dir + "/" + r.version
		}
		switch existing := tagged(tag); existing {
		case "":
			refspecs = append(refspecs, sha+":refs/tags/"+tag)
			created = append(created, tag)
			if dryRun {
				say(r.stdout, "would create %s\n", tag)
			}
		case target:
			say(r.stdout, "exists %s at the release commit, skipping\n", tag)
		default:
			conflicts = append(conflicts, fmt.Errorf("%s already exists at %s, not at the release commit", tag, existing))
		}
	}
	if len(conflicts) > 0 {
		return errors.Join(conflicts...)
	}
	if dryRun {
		say(r.stdout, "%s\n", "dry run: changed nothing")
		return nil
	}
	if len(refspecs) == 0 {
		say(r.stdout, "%s\n", "nothing to create")
		return nil
	}

	cmd := exec.CommandContext(r.ctx, "git", append([]string{"push", "--atomic", remote}, refspecs...)...)
	cmd.Dir = r.root
	cmd.Env = r.env
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push --atomic %s: %w", remote, err)
	}
	for _, tag := range created {
		say(r.stdout, "created %s\n", tag)
	}
	return nil
}

func (r *runner) git(dir string, args ...string) (string, error) {
	return r.command(dir, "git", args...)
}

func (r *runner) command(dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(r.ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = r.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// trim returns s without its leading white space.
func trim(s string) string {
	return strings.TrimLeft(s, " \t\n\v\f\r")
}

// say writes to w, which reports what the command does; a write that fails
// changes nothing the command does.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
