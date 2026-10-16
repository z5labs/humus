// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/z5labs/humus"
)

// tempDirName is the directory inside a Host's directory that CreateTemp
// and MkdirTemp use when dir is "", as os.TempDir is for package os.
const tempDirName = "tmp"

// WithFiles writes files into the Host's directory, keyed by their path
// relative to it, such as "etc/tls/server.p12". Parent directories are
// created.
//
// Paths use forward slashes on every OS. A path that is absolute, or that
// leaves the directory, fails the test through t, naming the path. The map
// and its contents are copied when WithFiles is called, so changing them
// afterwards changes nothing.
func WithFiles(files map[string][]byte) HostOption {
	files = maps.Clone(files)
	for name, data := range files {
		files[name] = bytes.Clone(data)
	}
	return func(h *Host) {
		h.tb.Helper()
		for _, name := range slices.Sorted(maps.Keys(files)) {
			if err := writeFile(h.root, name, files[name]); err != nil {
				h.tb.Errorf("humustest: WithFiles: %q: %v", name, err)
			}
		}
	}
}

func writeFile(root *os.Root, name string, data []byte) error {
	rel, err := local(name)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	return root.WriteFile(rel, data, 0o644)
}

// errNotLocal is the cause of every error for a name outside a Host's
// directory.
var errNotLocal = errors.New("not a path inside the Host's directory")

// local converts name, written with forward slashes and relative to a
// Host's directory, to a relative OS path, or fails if it is absolute or
// leaves the directory.
func local(name string) (string, error) {
	p := filepath.FromSlash(name)
	if filepath.IsAbs(p) || !filepath.IsLocal(p) {
		return "", errNotLocal
	}
	return p, nil
}

// FS returns the Host's filesystem. It is confined to the Host's directory
// with os.Root, so ".." and symlinks can't leave it. It accepts absolute
// paths under that directory, so a file's Name() can be passed back in, as
// in production. CreateTemp and MkdirTemp with dir == "" use a directory
// inside it.
//
// Relative names are relative to the Host's directory. Files are opened
// under its absolute path, so a file's Name() is an absolute OS path, and
// MkdirTemp returns one. The directory CreateTemp and MkdirTemp use for
// dir == "" is "tmp", created when it is first needed.
func (h *Host) FS() humus.FS {
	return rootFS{dir: h.dir, root: h.root}
}

// Path returns the absolute OS path of name, which is relative to the
// Host's directory, to put in a setting.
//
// Like WithFiles, Path takes forward slashes on every OS, and fails the
// test through t, naming the path, if name is absolute or leaves the
// directory.
func (h *Host) Path(name string) string {
	rel, err := local(name)
	if err != nil {
		h.tb.Helper()
		h.tb.Errorf("humustest: Path(%q): %v", name, err)
		return filepath.Join(h.dir, filepath.FromSlash(name))
	}
	return filepath.Join(h.dir, rel)
}

// rootFS is the humus.FS of a Host: every method resolves its names
// against an os.Root opened on dir, the Host's directory.
type rootFS struct {
	dir  string
	root *os.Root
}

// rel returns the name to give the os.Root for name: name itself when it
// is relative, and its path relative to the directory when it is
// absolute. A name outside the directory comes back starting with "..",
// and the os.Root refuses it.
func (r rootFS) rel(name string) string {
	if !filepath.IsAbs(name) {
		return name
	}
	rel, err := filepath.Rel(r.dir, name)
	if err != nil {
		// Only on Windows, for a name on another volume. Any path that
		// escapes serves: the os.Root refuses it.
		return ".." + string(filepath.Separator) + name
	}
	return rel
}

// abs returns the absolute OS path of rel, a name relative to the
// directory.
func (r rootFS) abs(rel string) string {
	return filepath.Join(r.dir, rel)
}

// renamed puts name back in a *fs.PathError the os.Root returned for rel,
// so that an error names the path the caller passed.
func renamed(err error, rel, name string) error {
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Path == rel {
		pe.Path = name
	}
	return err
}

func (r rootFS) Open(name string) (*os.File, error) {
	rel := r.rel(name)
	f, err := r.root.Open(rel)
	return f, renamed(err, rel, name)
}

func (r rootFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	rel := r.rel(name)
	f, err := r.root.OpenFile(rel, flag, perm)
	return f, renamed(err, rel, name)
}

// tempDir returns the name, relative to the directory, of the directory
// that CreateTemp or MkdirTemp creates in: dir, or for "" the Host's
// temporary directory, which it creates.
func (r rootFS) tempDir(dir string) (string, error) {
	if dir != "" {
		return r.rel(dir), nil
	}
	if err := r.root.MkdirAll(tempDirName, 0o755); err != nil {
		return "", renamed(err, tempDirName, r.abs(tempDirName))
	}
	return tempDirName, nil
}

// tempName splits pattern at its last "*", as os.CreateTemp does, and
// returns a function making a new random name from it.
func tempName(op, pattern string) (func() string, error) {
	if strings.ContainsRune(pattern, os.PathSeparator) || strings.ContainsRune(pattern, '/') {
		return nil, &fs.PathError{Op: op, Path: pattern, Err: errors.New("pattern contains path separator")}
	}
	prefix, suffix := pattern, ""
	if i := strings.LastIndexByte(pattern, '*'); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	return func() string {
		return prefix + strconv.FormatUint(uint64(rand.Uint32()), 10) + suffix
	}, nil
}

// tempTries is how many names CreateTemp and MkdirTemp try before giving
// up, as package os does.
const tempTries = 10000

func (r rootFS) CreateTemp(dir, pattern string) (*os.File, error) {
	next, err := tempName("createtemp", pattern)
	if err != nil {
		return nil, err
	}
	d, err := r.tempDir(dir)
	if err != nil {
		return nil, err
	}
	for range tempTries {
		rel := filepath.Join(d, next())
		f, err := r.root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, renamed(err, rel, r.abs(rel))
	}
	return nil, &fs.PathError{Op: "createtemp", Path: r.abs(filepath.Join(d, pattern)), Err: fs.ErrExist}
}

func (r rootFS) MkdirTemp(dir, pattern string) (string, error) {
	next, err := tempName("mkdirtemp", pattern)
	if err != nil {
		return "", err
	}
	d, err := r.tempDir(dir)
	if err != nil {
		return "", err
	}
	for range tempTries {
		rel := filepath.Join(d, next())
		err := r.root.Mkdir(rel, 0o700)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", renamed(err, rel, r.abs(rel))
		}
		return r.abs(rel), nil
	}
	return "", &fs.PathError{Op: "mkdirtemp", Path: r.abs(filepath.Join(d, pattern)), Err: fs.ErrExist}
}

func (r rootFS) Mkdir(name string, perm os.FileMode) error {
	rel := r.rel(name)
	return renamed(r.root.Mkdir(rel, perm), rel, name)
}

func (r rootFS) MkdirAll(name string, perm os.FileMode) error {
	rel := r.rel(name)
	return renamed(r.root.MkdirAll(rel, perm), rel, name)
}

func (r rootFS) Remove(name string) error {
	rel := r.rel(name)
	return renamed(r.root.Remove(rel), rel, name)
}

func (r rootFS) RemoveAll(name string) error {
	rel := r.rel(name)
	return renamed(r.root.RemoveAll(rel), rel, name)
}

func (r rootFS) Rename(oldname, newname string) error {
	oldrel, newrel := r.rel(oldname), r.rel(newname)
	err := r.root.Rename(oldrel, newrel)
	var le *os.LinkError
	if errors.As(err, &le) {
		if le.Old == oldrel {
			le.Old = oldname
		}
		if le.New == newrel {
			le.New = newname
		}
	}
	return renamed(err, oldrel, oldname)
}

func (r rootFS) Stat(name string) (os.FileInfo, error) {
	rel := r.rel(name)
	fi, err := r.root.Stat(rel)
	return fi, renamed(err, rel, name)
}

func (r rootFS) ReadDir(name string) ([]os.DirEntry, error) {
	rel := r.rel(name)
	f, err := r.root.Open(rel)
	if err != nil {
		return nil, renamed(err, rel, name)
	}
	entries, err := f.ReadDir(-1)
	cerr := f.Close()
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return entries, errors.Join(err, cerr)
}

func (r rootFS) DirFS(dir string) fs.FS {
	sub, err := fs.Sub(r.root.FS(), filepath.ToSlash(r.rel(dir)))
	if err != nil {
		return errFS{err: &fs.PathError{Op: "dirfs", Path: dir, Err: errNotLocal}}
	}
	return sub
}

// errFS is the fs.FS DirFS returns for a dir outside the Host's
// directory: like os.DirFS, DirFS can't fail, so every Open does.
type errFS struct{ err error }

func (e errFS) Open(string) (fs.File, error) {
	return nil, e.err
}
