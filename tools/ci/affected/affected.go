// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package affected decides which of a repository's Go modules a change
// affects, so that CI runs a module's container and kind jobs only when the
// change touches something they test.
//
// A change affects a module when it touches any of these:
//
//   - a file of the module's own directory, outside every module nested in
//     it, such as its Go files, its Dockerfile, its deploy/ directory or its
//     WireMock mappings
//   - a file a package the module builds from is compiled or embeds from, in
//     another module of the repository, such as humus's own packages through
//     the module's replace
//   - the go.mod or go.sum of a module of the repository in the module's
//     build list
//   - a file of Always, which affects every module
package affected

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Tags are the build tags a module's packages are listed with, so that what
// its tests behind those tags build from counts too.
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

// Select returns every module of mods that a change to the files changed
// affects, in the order of mods. changed are slash-separated paths from the
// repository's root. owners are the directories of every module of the
// repository, those that hold no Go package included, which is what decides
// the module a file belongs to: the one whose directory is the longest that
// holds it.
func Select(mods []Module, owners []string, changed []string) []Affected {
	var out []Affected
	for _, m := range mods {
		for _, file := range changed {
			if affects(m, owners, file) {
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
// by a directory that holds it. A file the change deletes matches the same
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
		pattern = strings.TrimPrefix(pattern, "all:")
		for name := rel; name != "."; name = path.Dir(name) {
			if matched, _ := path.Match(pattern, name); matched {
				return true
			}
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
// modules in go list -m all.
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
		"-json=Dir,Module,EmbedPatterns", "./...")
	if err != nil {
		return Module{}, fmt.Errorf("go list -deps in %s: %w", dir, err)
	}
	err = decode(out, func(v struct {
		Dir           string
		Module        *struct{ Dir string }
		EmbedPatterns []string
	}) {
		if v.Module == nil {
			return
		}
		mod, ok := within(root, v.Module.Dir)
		if !ok || mod == dir {
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
	return m, nil
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

// Run decides which modules of the git repository that holds dir the change
// from the commit base to HEAD affects, in the order of modules.List. With no
// base there is no change to compare, so every module is affected. It lists
// what every module builds from whether or not it has a base, so a run with
// none, such as a push to main's, still downloads what a run with one needs.
//
// env is the environment of git and go, as in exec.Cmd: nil is this
// process's.
func Run(ctx context.Context, dir, base string, env []string) ([]Affected, error) {
	l, err := modules.List(ctx, dir, env)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if base == "" {
		out := make([]Affected, len(mods))
		for i, m := range mods {
			out[i] = Affected{Dir: m.Dir}
		}
		return out, nil
	}
	changed, err := Changed(ctx, l.Root, base, env)
	if err != nil {
		return nil, err
	}
	return Select(mods, slices.Concat(l.Dirs, l.Empty), changed), nil
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
