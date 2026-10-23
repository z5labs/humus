// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package discover finds what flaky runs: the repository's modules, the
// packages in them that have tests at each level, and the walkthroughs. It
// keeps no list of any of them, so adding or removing a module, a package, a
// test or a walkthrough changes nothing in flaky.
//
// A level is the tests a build tag adds. The unit level is the untagged
// tests; any other level is named after its tag, such as testcontainers, and
// its tests are the ones a binary built with the tag has and the untagged
// binary doesn't. The levels are found too: one for each build tag the test
// files use.
package discover

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Unit is the level of the untagged tests.
const Unit = "unit"

// Tag returns the build tag that the binaries of level are built with: none
// for Unit, and the level's name for any other.
func Tag(level string) string {
	if level == Unit {
		return ""
	}
	return level
}

// Levels returns the levels of the repository at root: Unit, then one for
// each build tag in a //go:build line of a _test.go file that git tracks
// there, in order of their names. It keeps no list of tags, so a tag that a
// test file starts to use is a level of its own, with no change to flaky.
func Levels(ctx context.Context, root string, env []string) ([]string, error) {
	out, err := command(ctx, root, env, "git", "ls-files", "-z", "--", "*_test.go")
	if err != nil {
		return nil, err
	}
	tags := map[string]string{}
	for name := range strings.SplitSeq(out, "\x00") {
		if name == "" {
			continue
		}
		if err := fileTags(root, name, tags); err != nil {
			return nil, err
		}
	}
	if file, ok := tags[Unit]; ok {
		return nil, fmt.Errorf("%s uses the build tag %s, which is the name of the untagged level", file, Unit)
	}
	levels := []string{Unit}
	for tag := range tags {
		levels = append(levels, tag)
	}
	slices.Sort(levels[1:])
	return levels, nil
}

// fileTags adds the build tags in the //go:build line of the Go file name, a
// slash-separated path under root, to tags, each one mapped to the first file
// that uses it. A file that git tracks but that is gone from the working tree
// has none.
func fileTags(root, name string, tags map[string]string) error {
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		return err
	}
	for _, group := range f.Comments {
		if group.Pos() >= f.Package {
			break
		}
		for _, c := range group.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			for _, tag := range exprTags(expr) {
				if _, ok := tags[tag]; !ok {
					tags[tag] = name
				}
			}
		}
	}
	return nil
}

// exprTags returns every build tag that expr names, negated or not.
func exprTags(expr constraint.Expr) []string {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		return []string{e.Tag}
	case *constraint.NotExpr:
		return exprTags(e.X)
	case *constraint.AndExpr:
		return append(exprTags(e.X), exprTags(e.Y)...)
	case *constraint.OrExpr:
		return append(exprTags(e.X), exprTags(e.Y)...)
	}
	return nil
}

// Package is a package that has test files at a level.
type Package struct {
	// Module is the directory of the package's module, relative to the
	// repository's root, such as grpc, or . for the root module.
	Module string `json:"module"`
	// Dir is the package's directory, relative to the repository's root,
	// such as grpc/grpchealth.
	Dir string `json:"dir"`
	// ImportPath is the package's import path.
	ImportPath string `json:"importPath"`
	// Level is the level whose tests the package's binary runs.
	Level string `json:"level"`
	// Untagged reports, of a package at a tagged level, that it has test
	// files without the tag too, so its binary built without the tag may
	// have tests of its own, which its level's binary has too.
	Untagged bool `json:"-"`
}

// Root returns the root of the git repository that holds dir.
func Root(ctx context.Context, dir string, env []string) (string, error) {
	out, err := command(ctx, dir, env, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Commit returns the commit checked out at root.
func Commit(ctx context.Context, root string, env []string) (string, error) {
	out, err := command(ctx, root, env, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// GoVersion returns the version of the local go command, such as go1.27.2.
func GoVersion(ctx context.Context, root string, env []string) (string, error) {
	out, err := command(ctx, root, env, "go", "env", "GOVERSION")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Modules returns the directory of every module that the repository's
// .github/scripts/go-modules.sh lists, as CI's matrices do.
func Modules(ctx context.Context, root string, env []string) ([]string, error) {
	out, err := command(ctx, root, env, filepath.Join(root, ".github", "scripts", "go-modules.sh"))
	if err != nil {
		return nil, err
	}
	modules := strings.Fields(out)
	if len(modules) == 0 {
		return nil, fmt.Errorf("go-modules.sh listed no modules in %s", root)
	}
	return modules, nil
}

// Packages returns the packages with test files at each of levels, in the
// modules, that the patterns match, module by module and, in each, level by
// level.
//
// A package is at the unit level when it has test files. It is at any other
// level when its tag changes its test files, so that its binary built with
// the tag may have tests the untagged one doesn't; which tests those are is
// known only once both are built.
//
// A pattern is a directory from the repository's root, such as
// ./grpc/grpchealth, or ends in /... to match every directory under it too,
// such as ./grpc/... or ./... With no patterns, every package matches. Every
// pattern must match at least one package, at some level.
func Packages(ctx context.Context, root string, modules, patterns, levels []string, env []string) ([]Package, error) {
	matchers := make([]matcher, 0, len(patterns))
	for _, p := range patterns {
		m, err := parsePattern(p)
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, m)
	}
	if len(matchers) == 0 {
		matchers = append(matchers, matcher{all: true})
	}

	matched := make([]bool, len(matchers))
	var pkgs []Package
	for _, module := range modules {
		if !slices.ContainsFunc(matchers, func(m matcher) bool { return m.overlaps(module) }) {
			continue
		}
		found, err := levelPackages(ctx, root, module, levels, env)
		if err != nil {
			return nil, err
		}
		for _, pkg := range found {
			hit := false
			for i, m := range matchers {
				if m.matches(pkg.Dir) {
					matched[i] = true
					hit = true
				}
			}
			if hit {
				pkgs = append(pkgs, pkg)
			}
		}
	}
	for i, ok := range matched {
		if !ok {
			return nil, fmt.Errorf("%s matched no package with tests", patterns[i])
		}
	}
	return pkgs, nil
}

// levelPackages returns the packages with test files at each of levels in
// the module dir, level by level.
func levelPackages(ctx context.Context, root, module string, levels []string, env []string) ([]Package, error) {
	untagged, err := modulePackages(ctx, root, module, "", env)
	if err != nil {
		return nil, err
	}
	files := make(map[string]string, len(untagged))
	for _, l := range untagged {
		files[l.ImportPath] = l.files
	}
	var pkgs []Package
	for _, level := range levels {
		listed := untagged
		if tag := Tag(level); tag != "" {
			if listed, err = modulePackages(ctx, root, module, tag, env); err != nil {
				return nil, err
			}
		}
		for _, l := range listed {
			untagged, ok := files[l.ImportPath]
			if Tag(level) != "" && untagged == l.files {
				continue
			}
			l.Level = level
			l.Untagged = Tag(level) != "" && ok
			pkgs = append(pkgs, l.Package)
		}
	}
	return pkgs, nil
}

// listed is a package with test files, and those files.
type listed struct {
	Package
	files string
}

// modulePackages returns the packages with test files in the module dir,
// built with tag, or with none when tag is "".
func modulePackages(ctx context.Context, root, module, tag string, env []string) ([]listed, error) {
	args := []string{"list"}
	if tag != "" {
		args = append(args, "-tags", tag)
	}
	args = append(args, "-f", "{{.ImportPath}}\t{{.Dir}}\t{{join .TestGoFiles \",\"}}\t{{join .XTestGoFiles \",\"}}", "./...")
	out, err := command(ctx, filepath.Join(root, module), env, "go", args...)
	if err != nil {
		return nil, err
	}
	var pkgs []listed
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("go list in %s printed %q", module, sc.Text())
		}
		if fields[2] == "" && fields[3] == "" {
			continue
		}
		dir, err := filepath.Rel(root, fields[1])
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, listed{
			Package: Package{Module: module, Dir: filepath.ToSlash(dir), ImportPath: fields[0]},
			files:   fields[2] + "\t" + fields[3],
		})
	}
	return pkgs, nil
}

type matcher struct {
	all    bool
	dir    string
	subdir bool
}

func parsePattern(p string) (matcher, error) {
	if !strings.HasPrefix(p, "./") && p != "." {
		return matcher{}, fmt.Errorf("package pattern %q must start with ./, from the repository's root", p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return matcher{}, fmt.Errorf("package pattern %q leaves the repository", p)
	}
	if clean == "..." {
		return matcher{all: true}, nil
	}
	if dir, ok := strings.CutSuffix(clean, "/..."); ok {
		return matcher{dir: dir, subdir: true}, nil
	}
	return matcher{dir: clean}, nil
}

func (m matcher) matches(dir string) bool {
	if m.all || dir == m.dir {
		return true
	}
	return m.subdir && strings.HasPrefix(dir, m.dir+"/")
}

// overlaps reports whether the module in the directory module could hold a
// package m matches.
func (m matcher) overlaps(module string) bool {
	if m.all || module == "." || module == m.dir || strings.HasPrefix(m.dir, module+"/") {
		return true
	}
	return m.subdir && strings.HasPrefix(module, m.dir+"/")
}

// Walkthrough is a module that deploys onto a kind cluster: one with a
// Dockerfile and a deploy/kind/up.sh. Its functional tests are its packages at
// the functional level.
type Walkthrough struct {
	// Module is its module's directory, relative to the repository's
	// root, such as example/grpc/wallet.
	Module string `json:"module"`
	// Name is its directory's name, such as wallet, which its image is
	// tagged with.
	Name string `json:"name"`
}

// Dockerfile returns the path of the walkthrough's Dockerfile, under root.
func (w Walkthrough) Dockerfile(root string) string {
	return filepath.Join(root, filepath.FromSlash(w.Module), "Dockerfile")
}

// Up returns the path of the walkthrough's deploy/kind/up.sh, under root.
func (w Walkthrough) Up(root string) string {
	return filepath.Join(root, filepath.FromSlash(w.Module), "deploy", "kind", "up.sh")
}

// Walkthroughs returns the modules, of the directories modules under root,
// that are walkthroughs, in their order. Two walkthroughs can't share a name,
// since each one's image is tagged with it.
func Walkthroughs(root string, modules []string) ([]Walkthrough, error) {
	var found []Walkthrough
	names := map[string]string{}
	for _, module := range modules {
		w := Walkthrough{Module: module, Name: path.Base(module)}
		ok := true
		for _, file := range []string{w.Dockerfile(root), w.Up(root)} {
			info, err := os.Stat(file)
			switch {
			case os.IsNotExist(err):
				ok = false
			case err != nil:
				return nil, err
			case !info.Mode().IsRegular():
				ok = false
			}
		}
		if !ok {
			continue
		}
		if other, dup := names[w.Name]; dup {
			return nil, fmt.Errorf("the walkthroughs %s and %s share the name %s", other, module, w.Name)
		}
		names[w.Name] = module
		found = append(found, w)
	}
	return found, nil
}

var goVersionLine = regexp.MustCompile(`go-version:\s*['"]?([0-9][0-9.]*)['"]?`)

// CIGoVersion returns the Go version that the repository's
// .github/workflows/build.yaml sets, such as 1.27.2.
func CIGoVersion(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "build.yaml"))
	if err != nil {
		return "", err
	}
	m := goVersionLine.FindSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("%s sets no go-version", filepath.Join(".github", "workflows", "build.yaml"))
	}
	return string(m[1]), nil
}

// CheckGoVersion returns a warning when the local Go, such as go1.27.0,
// isn't the version build.yaml sets at root, or "" when it is.
func CheckGoVersion(root, local string) string {
	want, err := CIGoVersion(root)
	if err != nil {
		return fmt.Sprintf("warning: can't tell which Go CI uses: %v", err)
	}
	if "go"+want != local {
		return fmt.Sprintf("warning: the local Go is %s, but build.yaml sets %s", local, want)
	}
	return ""
}

func command(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s in %s: %w: %s", name, strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
