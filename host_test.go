// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/z5labs/humus"
)

// Every test in this file works inside a directory from t.TempDir and opens
// no socket. The loopback test of Listen and Dial is in
// host_loopback_test.go, behind the loopback build tag.

func TestOSHost_has_every_field(t *testing.T) {
	h := humus.OSHost()

	if h.Listen == nil {
		t.Error("Listen is nil")
	}
	if h.Dial == nil {
		t.Error("Dial is nil")
	}
	if h.FS == nil {
		t.Error("FS is nil")
	}
}

func TestOSHost_Listen_is_net_Listen(t *testing.T) {
	h := humus.OSHost()

	got := reflect.ValueOf(h.Listen).Pointer()
	want := reflect.ValueOf(net.Listen).Pointer()
	if got != want {
		t.Error("Listen is not net.Listen")
	}
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, f *os.File) string {
	t.Helper()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func closeFile(t *testing.T, f *os.File) {
	t.Helper()
	if err := f.Close(); err != nil {
		t.Error(err)
	}
}

func TestOSHost_FS(t *testing.T) {
	t.Run("Open reads an existing file", func(t *testing.T) {
		dir := t.TempDir()
		name := filepath.Join(dir, "a.txt")
		writeFile(t, name, "hello")

		f, err := humus.OSHost().FS.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		defer closeFile(t, f)

		if got := readAll(t, f); got != "hello" {
			t.Errorf("read %q, want %q", got, "hello")
		}
	})

	t.Run("Open fails with fs.ErrNotExist for a missing file", func(t *testing.T) {
		dir := t.TempDir()

		_, err := humus.OSHost().FS.Open(filepath.Join(dir, "missing"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("got %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("OpenFile creates and writes a file", func(t *testing.T) {
		dir := t.TempDir()
		name := filepath.Join(dir, "b.txt")

		f, err := humus.OSHost().FS.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, "written"); err != nil {
			t.Fatal(err)
		}
		closeFile(t, f)

		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "written" {
			t.Errorf("file holds %q, want %q", b, "written")
		}
	})

	t.Run("OpenFile with O_EXCL fails with fs.ErrExist for an existing file", func(t *testing.T) {
		dir := t.TempDir()
		name := filepath.Join(dir, "b.txt")
		writeFile(t, name, "")

		_, err := humus.OSHost().FS.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			t.Errorf("got %v, want fs.ErrExist", err)
		}
	})

	t.Run("CreateTemp creates a new file in dir", func(t *testing.T) {
		dir := t.TempDir()

		f, err := humus.OSHost().FS.CreateTemp(dir, "tmp-*.txt")
		if err != nil {
			t.Fatal(err)
		}
		defer closeFile(t, f)

		if got := filepath.Dir(f.Name()); got != dir {
			t.Errorf("created in %q, want %q", got, dir)
		}
		base := filepath.Base(f.Name())
		if !strings.HasPrefix(base, "tmp-") || !strings.HasSuffix(base, ".txt") {
			t.Errorf("name %q does not match the pattern tmp-*.txt", base)
		}
		if _, err := os.Stat(f.Name()); err != nil {
			t.Error(err)
		}
	})

	t.Run("MkdirTemp creates a new directory in dir", func(t *testing.T) {
		dir := t.TempDir()

		name, err := humus.OSHost().FS.MkdirTemp(dir, "tmp-")
		if err != nil {
			t.Fatal(err)
		}

		if got := filepath.Dir(name); got != dir {
			t.Errorf("created in %q, want %q", got, dir)
		}
		if !strings.HasPrefix(filepath.Base(name), "tmp-") {
			t.Errorf("name %q does not match the pattern tmp-*", name)
		}
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", name)
		}
	})

	t.Run("Mkdir creates a directory", func(t *testing.T) {
		dir := t.TempDir()
		name := filepath.Join(dir, "sub")

		if err := humus.OSHost().FS.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}

		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", name)
		}
	})

	t.Run("Mkdir fails with fs.ErrExist for an existing directory", func(t *testing.T) {
		dir := t.TempDir()

		err := humus.OSHost().FS.Mkdir(dir, 0o700)
		if !errors.Is(err, fs.ErrExist) {
			t.Errorf("got %v, want fs.ErrExist", err)
		}
	})

	t.Run("MkdirAll creates every missing parent", func(t *testing.T) {
		dir := t.TempDir()
		name := filepath.Join(dir, "a", "b", "c")

		if err := humus.OSHost().FS.MkdirAll(name, 0o700); err != nil {
			t.Fatal(err)
		}

		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", name)
		}
	})

	t.Run("Remove removes a file", func(t *testing.T) {
		dir := t.TempDir()
		name := filepath.Join(dir, "a.txt")
		writeFile(t, name, "")

		if err := humus.OSHost().FS.Remove(name); err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("after Remove, Stat returned %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("Remove fails on a directory that is not empty", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(sub, "a.txt"), "")

		if err := humus.OSHost().FS.Remove(sub); err == nil {
			t.Error("Remove of a non-empty directory succeeded")
		}
	})

	t.Run("RemoveAll removes a tree", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub")
		if err := os.MkdirAll(filepath.Join(sub, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(sub, "nested", "a.txt"), "")

		if err := humus.OSHost().FS.RemoveAll(sub); err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(sub); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("after RemoveAll, Stat returned %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("Rename moves a file", func(t *testing.T) {
		dir := t.TempDir()
		oldname := filepath.Join(dir, "old.txt")
		newname := filepath.Join(dir, "new.txt")
		writeFile(t, oldname, "moved")

		if err := humus.OSHost().FS.Rename(oldname, newname); err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(oldname); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("after Rename, Stat of the old name returned %v, want fs.ErrNotExist", err)
		}
		b, err := os.ReadFile(newname)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "moved" {
			t.Errorf("new name holds %q, want %q", b, "moved")
		}
	})

	t.Run("Stat describes a file", func(t *testing.T) {
		dir := t.TempDir()
		name := filepath.Join(dir, "a.txt")
		writeFile(t, name, "12345")

		info, err := humus.OSHost().FS.Stat(name)
		if err != nil {
			t.Fatal(err)
		}

		if info.Name() != "a.txt" {
			t.Errorf("Name is %q, want %q", info.Name(), "a.txt")
		}
		if info.Size() != 5 {
			t.Errorf("Size is %d, want 5", info.Size())
		}
		if !info.Mode().IsRegular() {
			t.Errorf("Mode is %v, want a regular file", info.Mode())
		}
	})

	t.Run("Stat fails with fs.ErrNotExist for a missing file", func(t *testing.T) {
		dir := t.TempDir()

		_, err := humus.OSHost().FS.Stat(filepath.Join(dir, "missing"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("got %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("ReadDir lists a directory sorted by name", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "b.txt"), "")
		writeFile(t, filepath.Join(dir, "a.txt"), "")
		if err := os.Mkdir(filepath.Join(dir, "c"), 0o700); err != nil {
			t.Fatal(err)
		}

		entries, err := humus.OSHost().FS.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}

		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if want := []string{"a.txt", "b.txt", "c"}; !slices.Equal(names, want) {
			t.Errorf("entries are %v, want %v", names, want)
		}
		if !entries[2].IsDir() {
			t.Errorf("entry %q is not a directory", entries[2].Name())
		}
	})

	t.Run("DirFS can be walked with fs.WalkDir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "top.txt"), "")
		writeFile(t, filepath.Join(dir, "a", "b", "deep.txt"), "deep")

		fsys := humus.OSHost().FS.DirFS(dir)

		var walked []string
		err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			walked = append(walked, path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		want := []string{".", "a", "a/b", "a/b/deep.txt", "top.txt"}
		if !slices.Equal(walked, want) {
			t.Errorf("walked %v, want %v", walked, want)
		}

		b, err := fs.ReadFile(fsys, "a/b/deep.txt")
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "deep" {
			t.Errorf("read %q, want %q", b, "deep")
		}
	})
}

// An os.Root refuses to follow an absolute symbolic link wherever it is
// rooted, "/" included, which is why OSHost's FS is built on plain os calls.
// The link here points at an absolute path inside the test's own directory,
// so the test touches no other file.
func TestOSHost_FS_follows_an_absolute_symlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	writeFile(t, target, "through the link")
	if !filepath.IsAbs(target) {
		t.Fatalf("target %q is not absolute", target)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	t.Run("os.Root refuses it", func(t *testing.T) {
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := root.Close(); err != nil {
				t.Error(err)
			}
		}()

		f, err := root.Open("link.txt")
		if err == nil {
			closeFile(t, f)
			t.Fatal("os.Root followed an absolute symlink")
		}
	})

	t.Run("Open follows it", func(t *testing.T) {
		f, err := humus.OSHost().FS.Open(link)
		if err != nil {
			t.Fatal(err)
		}
		defer closeFile(t, f)

		if got := readAll(t, f); got != "through the link" {
			t.Errorf("read %q, want %q", got, "through the link")
		}
	})

	t.Run("Stat follows it", func(t *testing.T) {
		info, err := humus.OSHost().FS.Stat(link)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Errorf("Mode is %v, want the regular file the link points at", info.Mode())
		}
	})
}
