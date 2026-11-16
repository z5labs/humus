// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package affected decides which of a repository's Go modules a change
// affects, so that CI lints, tests and vets a module, and runs its container
// and kind jobs, only when the change touches something they test.
//
// A change affects a module when it touches any of these:
//
//   - a file of the module's own directory, outside every module nested in
//     it, such as its Go files, its Dockerfile, its deploy/ directory or its
//     WireMock mappings, other than a file of Lint
//   - a file a package the module builds from is compiled or embeds from, in
//     another module of the repository, such as humus's own packages through
//     the module's replace
//   - the go.mod or go.sum of a module of the repository in the module's
//     build list
//   - a file of Always, which affects every module
//
// A module's lint and unit tests also run when the change touches a file of
// Lint, which configures the lint of every module but affects nothing else,
// or a file Reads says the module's unit tests read.
// A module's tests behind a tag of Tags are vetted when the change affects
// the module and a Go file of its own packages is behind that tag.
package affected

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/build/constraint"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/z5labs/humus/tools/ci/modules"
)

// Always holds the files, and with a trailing slash the directories, a
// change to which affects every module: the workflow that runs the jobs, the
// actions its jobs use, and the Go tooling that makes the decision.
var Always = []string{
	".github/workflows/build.yaml",
	".github/actions/",
	"tools/ci/",
}

// Lint holds the files a change to which runs the lint and unit tests of
// every module, and affects no module, the one whose directory holds them
// included: golangci-lint's configuration.
var Lint = []string{".golangci.yaml"}

// Reads holds, for a module whose unit tests read files of the repository
// besides those it builds from, a test of each file they read: a change to
// one runs the module's lint and unit tests, and affects no module.
// example/http/inlineedit's tests read every Go file outside example/ and
// docs/ for a hypermedia library's name.
var Reads = map[string]func(file string) bool{
	"example/http/inlineedit": func(file string) bool {
		return path.Ext(file) == ".go" && !strings.HasPrefix(file, "example/") && !strings.HasPrefix(file, "docs/")
	},
}

// Tags are the build tags a module's packages are listed with, so that what
// its tests behind those tags build from counts too, and the tags whose tests
// CI vets.
var Tags = []string{"testcontainers", "functional"}

// Module is a module of the repository, and what of the repository it builds
// from.
type Module struct {
	// Dir is the module's directory from the repository's root: . for the
	// root module.
	Dir string
	// Packages are the packages of the repository's other modules the
	// module's packages and their tests build from.
	Packages []Package
	// Requires are the directories of the repository's other modules in the
	// module's build list, such as one its go.mod replaces a requirement
	// with.
	Requires []string
	// Tags are the tags of Tags that a Go file of the module's own packages
	// is behind: its //go:build line names the tag, other than under a !.
	Tags []string
}

// Package is a package of the repository a module builds from.
type Package struct {
	// Dir is the package's directory from the repository's root.
	Dir string
	// Embeds are the package's //go:embed patterns, relative to Dir.
	Embeds []string
}

// Affected is a module a change affects.
type Affected struct {
	// Dir is the module's directory from the repository's root.
	Dir string
	// Because is the first changed file that affects it, or "" when there
	// was no change to compare, so every module is affected.
	Because string
}

// Plan is what CI runs for a change, module by module.
type Plan struct {
	// Affected are the modules the change affects, as Select decides. CI
	// runs a module's container and kind jobs, and its conformance tests,
	// only when it is here.
	Affected []Affected
	// Linted are the modules whose lint and unit tests run: every module
	// the change affects, every module when it touches a file of Lint, and
	// every module whose unit tests read a file it touches, as Reads says.
	Linted []Affected
	// Vetted holds, for each tag of Tags, the directory of every module of
	// Affected that has a Go file behind the tag, as Module.Tags says, in
	// the order of Affected.
	Vetted map[string][]string
}

// Select returns every module of mods that a change to the files changed
// affects, in the order of mods. changed are slash-separated paths from the
// repository's root. owners are the directories of every module of the
// repository, those that hold no Go package included, which is what decides
// the module a file belongs to: the one whose directory is the longest that
// holds it.
func Select(mods []Module, owners []string, changed []string) []Affected {
	return pick(mods, changed, func(m Module, file string) bool { return affects(m, owners, file) })
}

// Decide returns the Plan for a change to the files changed, as Select takes
// them.
func Decide(mods []Module, owners []string, changed []string) Plan {
	return plan(mods, Select(mods, owners, changed), pick(mods, changed, func(m Module, file string) bool {
		return affects(m, owners, file) || slices.Contains(Lint, file) || Reads[m.Dir] != nil && Reads[m.Dir](file)
	}))
}

// Everything returns the Plan when there is no change to compare, so every
// module is affected.
func Everything(mods []Module) Plan {
	all := make([]Affected, len(mods))
	for i, m := range mods {
		all[i] = Affected{Dir: m.Dir}
	}
	return plan(mods, all, all)
}

func plan(mods []Module, affected, linted []Affected) Plan {
	p := Plan{Affected: affected, Linted: linted, Vetted: make(map[string][]string, len(Tags))}
	for _, tag := range Tags {
		p.Vetted[tag] = []string{}
	}
	for _, a := range affected {
		i := slices.IndexFunc(mods, func(m Module) bool { return m.Dir == a.Dir })
		for _, tag := range mods[i].Tags {
			p.Vetted[tag] = append(p.Vetted[tag], a.Dir)
		}
	}
	return p
}

// pick returns every module of mods that one of the files changed affects,
// as affects decides, with the first such file, in the order of mods.
func pick(mods []Module, changed []string, affects func(Module, string) bool) []Affected {
	var out []Affected
	for _, m := range mods {
		for _, file := range changed {
			if affects(m, file) {
				out = append(out, Affected{Dir: m.Dir, Because: file})
				break
			}
		}
	}
	return out
}

func affects(m Module, owners []string, file string) bool {
	if always(file) {
		return true
	}
	if slices.Contains(Lint, file) {
		return false
	}
	owner := owner(owners, file)
	if owner == m.Dir {
		return true
	}
	if name := path.Base(file); (name == "go.mod" || name == "go.sum") && path.Dir(file) == owner && slices.Contains(m.Requires, owner) {
		return true
	}
	for _, p := range m.Packages {
		if p.owns(owners, owner, file) {
			return true
		}
	}
	return false
}

func always(file string) bool {
	for _, a := range Always {
		if file == a || strings.HasSuffix(a, "/") && strings.HasPrefix(file, a) {
			return true
		}
	}
	return false
}

// owner returns the directory of the module of owners that file belongs to,
// or "" when it belongs to none.
func owner(owners []string, file string) string {
	best, longest := "", -1
	for _, o := range owners {
		n := 0
		if o != "." {
			if !strings.HasPrefix(file, o+"/") {
				continue
			}
			n = len(o)
		}
		if n > longest {
			best, longest = o, n
		}
	}
	return best
}

// compiled are the extensions of the files the go command compiles into a
// package, as go/build lists them, besides .go.
var compiled = []string{
	".c", ".cc", ".cpp", ".cxx", ".m", ".h", ".hh", ".hpp", ".hxx",
	".f", ".F", ".for", ".f90", ".s", ".S", ".sx", ".swig", ".swigcxx", ".syso",
}

// owns reports whether file, which belongs to the module owner, is one the
// package is compiled or embeds from: a source file of its directory other
// than a test's, or a file one of its //go:embed patterns names, itself or
// by a directory that holds it, as go:embed does: a directory leaves out the
// files of it whose names, or a directory's of theirs, begin with . or _,
// unless the pattern begins with all:. A file the change deletes matches the same
// way, so deleting one is a change to the package too.
func (p Package) owns(owners []string, owner, file string) bool {
	if owner != modulesOwner(owners, p.Dir) {
		return false
	}
	if path.Dir(file) == p.Dir {
		ext := path.Ext(file)
		if ext == ".go" && !strings.HasSuffix(file, "_test.go") || slices.Contains(compiled, ext) {
			return true
		}
	}
	rel, ok := strings.CutPrefix(file, p.Dir+"/")
	if p.Dir == "." {
		rel, ok = file, true
	}
	if !ok {
		return false
	}
	for _, pattern := range p.Embeds {
		pattern, all := strings.CutPrefix(pattern, "all:")
		for name := rel; name != "."; name = path.Dir(name) {
			if matched, _ := path.Match(pattern, name); matched && (name == rel || all || !hidden(strings.TrimPrefix(rel, name+"/"))) {
				return true
			}
		}
	}
	return false
}

// hidden reports whether a name of rel's begins with . or _, which a
// //go:embed pattern naming a directory that holds rel leaves out, unless
// the pattern begins with all:.
func hidden(rel string) bool {
	for name := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return true
		}
	}
	return false
}

// modulesOwner returns the module of owners that the directory dir belongs
// to.
func modulesOwner(owners []string, dir string) string {
	if dir == "." {
		return owner(owners, "")
	}
	return owner(owners, dir+"/")
}

// Load lists what of the repository at root the module in dir, a directory
// from root, builds from: the packages of the repository's other modules in
// go list -deps -test of its packages with Tags, and the repository's other
// modules in go list -m all. It reads the //go:build line of every Go file of
// the module's own packages and their tests, those go list ignores included,
// for the module's Tags.
//
// env is the environment of go, as in exec.Cmd: nil is this process's.
func Load(ctx context.Context, root, dir string, env []string) (Module, error) {
	m := Module{Dir: dir}
	moduleDir := filepath.Join(root, filepath.FromSlash(dir))

	out, err := run(ctx, moduleDir, env, "go", "list", "-m", "-json=Dir", "all")
	if err != nil {
		return Module{}, fmt.Errorf("go list -m all in %s: %w", dir, err)
	}
	err = decode(out, func(v struct{ Dir string }) {
		if d, ok := within(root, v.Dir); ok && d != dir {
			m.Requires = append(m.Requires, d)
		}
	})
	if err != nil {
		return Module{}, fmt.Errorf("go list -m all in %s: %w", dir, err)
	}

	out, err = run(ctx, moduleDir, env, "go", "list", "-deps", "-test", "-tags", strings.Join(Tags, ","),
		"-json=Dir,Module,EmbedPatterns,GoFiles,TestGoFiles,XTestGoFiles,IgnoredGoFiles", "./...")
	if err != nil {
		return Module{}, fmt.Errorf("go list -deps in %s: %w", dir, err)
	}
	var files []string
	err = decode(out, func(v struct {
		Dir           string
		Module        *struct{ Dir string }
		EmbedPatterns []string
		GoFiles       []string
		TestGoFiles   []string
		XTestGoFiles  []string
		// A file behind one tag of Tags and not another, such as
		// testcontainers && !functional, is ignored with both.
		IgnoredGoFiles []string
	}) {
		if v.Module == nil {
			return
		}
		mod, ok := within(root, v.Module.Dir)
		if !ok {
			return
		}
		if mod == dir {
			for _, f := range slices.Concat(v.GoFiles, v.TestGoFiles, v.XTestGoFiles, v.IgnoredGoFiles) {
				// A test binary's generated main is in the build cache,
				// named by its absolute path.
				if !filepath.IsAbs(f) {
					files = append(files, filepath.Join(v.Dir, f))
				}
			}
			return
		}
		d, ok := within(root, v.Dir)
		if !ok || slices.ContainsFunc(m.Packages, func(p Package) bool { return p.Dir == d }) {
			return
		}
		m.Packages = append(m.Packages, Package{Dir: d, Embeds: v.EmbedPatterns})
	})
	if err != nil {
		return Module{}, fmt.Errorf("go list -deps in %s: %w", dir, err)
	}
	slices.Sort(m.Requires)
	m.Requires = slices.Compact(m.Requires)
	slices.SortFunc(m.Packages, func(a, b Package) int { return strings.Compare(a.Dir, b.Dir) })

	slices.Sort(files)
	var tags []string
	for _, f := range slices.Compact(files) {
		t, err := buildTags(f)
		if err != nil {
			return Module{}, fmt.Errorf("reading the build constraint of %s: %w", f, err)
		}
		tags = append(tags, t...)
	}
	for _, tag := range Tags {
		if slices.Contains(tags, tag) {
			m.Tags = append(m.Tags, tag)
		}
	}
	return m, nil
}

// buildTags returns the tags the //go:build line of the Go file name names,
// other than under a !, or none when it has no //go:build line. The line is
// among the comments and blank lines before the file's first other line, as
// the go command reads it.
func buildTags(name string) ([]string, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "//") {
			return nil, nil
		}
		if constraint.IsGoBuild(line) {
			expr, err := constraint.Parse(line)
			if err != nil {
				return nil, err
			}
			var tags []string
			named(expr, &tags)
			return tags, nil
		}
	}
	return nil, nil
}

// named appends to tags every tag expr names, other than under a !.
func named(expr constraint.Expr, tags *[]string) {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		*tags = append(*tags, e.Tag)
	case *constraint.AndExpr:
		named(e.X, tags)
		named(e.Y, tags)
	case *constraint.OrExpr:
		named(e.X, tags)
		named(e.Y, tags)
	}
}

// within returns dir, an absolute path, as a slash-separated path from root,
// and whether it is under root at all.
func within(root, dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// decode decodes each JSON value in out, one after another, into a T and
// passes it to f.
func decode[T any](out []byte, f func(T)) error {
	d := json.NewDecoder(bytes.NewReader(out))
	for {
		var v T
		err := d.Decode(&v)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		f(v)
	}
}

// Changed returns the slash-separated path from the repository's root of
// every file that differs between the commit base and HEAD in the git
// repository root: added, deleted or modified, and a renamed file by both its
// names.
func Changed(ctx context.Context, root, base string, env []string) ([]string, error) {
	out, err := run(ctx, root, env, "git", "diff", "--name-only", "--no-renames", "-z", base, "HEAD", "--")
	if err != nil {
		return nil, err
	}
	var files []string
	for name := range strings.SplitSeq(string(out), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// Run decides the Plan for the change from the commit base to HEAD in the git
// repository that holds dir, its modules in the order of modules.List. With no
// base there is no change to compare, so every module is affected, as
// Everything decides. It lists what every module builds from whether or not
// it has a base, so a run with none, such as a push to main's, still
// downloads what a run with one needs.
//
// env is the environment of git and go, as in exec.Cmd: nil is this
// process's.
func Run(ctx context.Context, dir, base string, env []string) (Plan, error) {
	l, err := modules.List(ctx, dir, env)
	if err != nil {
		return Plan{}, err
	}
	mods := make([]Module, len(l.Dirs))
	errs := make([]error, len(l.Dirs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, d := range l.Dirs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			mods[i], errs[i] = Load(ctx, l.Root, d, env)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return Plan{}, err
	}
	if base == "" {
		return Everything(mods), nil
	}
	changed, err := Changed(ctx, l.Root, base, env)
	if err != nil {
		return Plan{}, err
	}
	return Decide(mods, slices.Concat(l.Dirs, l.Empty), changed), nil
}

func run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s in %s: %w: %s", name, strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
