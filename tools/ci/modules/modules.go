// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package modules finds the Go modules of a git repository. `./...` does not
// descend into a nested module, so CI lints and tests each module List finds
// on its own, and the release tags each module Find finds at the release
// commit.
package modules

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Ignored is a go.mod that is a test's fixture rather than a module of the
// repository, because a directory it is under is one the Go tool ignores.
type Ignored struct {
	// GoMod is the go.mod's path from the repository's root, such as
	// grpc/testdata/x/go.mod.
	GoMod string
	// Under is the name of the directory it is under that the Go tool
	// ignores, such as testdata or _tools.
	Under string
}

// Find returns the directory of every module among files, the
// slash-separated paths of a repository's files from its root, in their
// order: . for the root module's and the go.mod's directory for each other.
// It also returns every go.mod among files that is under a testdata
// directory, or under a directory whose name begins with . or _, as the Go
// tool ignores those directories too, and leaves them out of dirs.
func Find(files []string) (dirs []string, ignored []Ignored) {
	for _, file := range files {
		if file != "go.mod" && !strings.HasSuffix(file, "/go.mod") {
			continue
		}
		dir := path.Dir(file)
		if under := ignoredUnder(dir); under != "" {
			ignored = append(ignored, Ignored{GoMod: file, Under: under})
			continue
		}
		dirs = append(dirs, dir)
	}
	return dirs, ignored
}

// ignoredUnder returns the first of dir's directories the Go tool ignores,
// or "" when it ignores none of them.
func ignoredUnder(dir string) string {
	if dir == "." {
		return ""
	}
	for part := range strings.SplitSeq(dir, "/") {
		if part == "testdata" || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return part
		}
	}
	return ""
}

// GoMod returns the path from the repository's root of the go.mod of the
// module in dir: go.mod for the root module's, ., and dir/go.mod for every
// other.
func GoMod(dir string) string {
	if dir == "." {
		return "go.mod"
	}
	return dir + "/go.mod"
}

// Listing is what List found.
type Listing struct {
	// Root is the repository's root.
	Root string
	// Dirs is the directory of every module that holds at least one Go
	// package, relative to Root, sorted: . for the root module.
	Dirs []string
	// Empty is the directory of every module that holds no Go package, such
	// as the Hugo module under docs/, which has nothing to lint or test.
	Empty []string
}

// List finds the modules of the git repository that holds dir. Every go.mod
// git tracks is a module, except any Find ignores. A module whose go list
// ./... lists a package is in Dirs, and one whose go list lists none is in
// Empty. A go list that fails is a broken module, not an empty one, so it
// fails List.
//
// env is the environment of git and go, as in exec.Cmd: nil is this
// process's.
func List(ctx context.Context, dir string, env []string) (Listing, error) {
	out, err := command(ctx, dir, env, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return Listing{}, err
	}
	root := strings.TrimSpace(out)
	out, err = command(ctx, root, env, "git", "ls-files", "--", "go.mod", "**/go.mod")
	if err != nil {
		return Listing{}, err
	}
	dirs, _ := Find(strings.Split(strings.TrimSuffix(out, "\n"), "\n"))
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)

	l := Listing{Root: root}
	for _, d := range dirs {
		out, err := command(ctx, filepath.Join(root, filepath.FromSlash(d)), env, "go", "list", "./...")
		if err != nil {
			return Listing{}, fmt.Errorf("go list failed in %s: %w", d, err)
		}
		if strings.TrimSpace(out) == "" {
			l.Empty = append(l.Empty, d)
			continue
		}
		l.Dirs = append(l.Dirs, d)
	}
	return l, nil
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
