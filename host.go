// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"io/fs"
	"net"
	"os"
)

// Host is what humus hands an App beyond its process: the network and the
// filesystem. Every field is required.
//
// In main, an App runs on OSHost. In a test, it runs on a Host whose
// listeners, dialler and filesystem the test controls, so a whole App can
// run without touching the real host.
type Host struct {
	// Listen announces on the local network address, like net.Listen.
	Listen func(network, address string) (net.Listener, error)

	// Dial connects to the address on the named network, like
	// (*net.Dialer).DialContext.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)

	// FS is the Host's filesystem.
	FS FS
}

// FS is the filesystem of a Host. Names are OS paths, as accepted by the
// functions of the same name in package os, and files are *os.File, so a
// file opened through an FS can be handed to anything that takes an OS file
// or a path.
type FS interface {
	// Open opens the named file for reading, like os.Open.
	Open(name string) (*os.File, error)

	// OpenFile opens the named file with the given flag and perm, like
	// os.OpenFile.
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)

	// CreateTemp creates a new temporary file in dir, like os.CreateTemp.
	CreateTemp(dir, pattern string) (*os.File, error)

	// MkdirTemp creates a new temporary directory in dir and returns its
	// path, like os.MkdirTemp.
	MkdirTemp(dir, pattern string) (string, error)

	// Mkdir creates a directory, like os.Mkdir.
	Mkdir(name string, perm os.FileMode) error

	// MkdirAll creates a directory and any parents it needs, like
	// os.MkdirAll.
	MkdirAll(name string, perm os.FileMode) error

	// Remove removes the named file or empty directory, like os.Remove.
	Remove(name string) error

	// RemoveAll removes name and anything it contains, like os.RemoveAll.
	RemoveAll(name string) error

	// Rename renames oldname to newname, like os.Rename.
	Rename(oldname, newname string) error

	// Stat returns the FileInfo of the named file, following symbolic
	// links, like os.Stat.
	Stat(name string) (os.FileInfo, error)

	// ReadDir reads the named directory and returns its entries sorted by
	// filename, like os.ReadDir.
	ReadDir(name string) ([]os.DirEntry, error)

	// DirFS returns an fs.FS view of dir, for fs.WalkDir, fs.Glob,
	// template.ParseFS and the other std helpers.
	DirFS(dir string) fs.FS
}

// OSHost returns the Host of the current OS process: net.Listen, a
// net.Dialer's DialContext, and an FS that calls package os directly.
//
// The FS is not confined to a directory. In particular it is not
// os.OpenRoot("/"), because an os.Root refuses to follow absolute symbolic
// links even when it is rooted at "/", and paths on a real host routinely
// pass through them.
func OSHost() Host {
	var d net.Dialer
	return Host{
		Listen: net.Listen,
		Dial:   d.DialContext,
		FS:     osFS{},
	}
}

// osFS is the FS of OSHost. Each method is the os function of the same name.
type osFS struct{}

func (osFS) Open(name string) (*os.File, error) {
	return os.Open(name)
}

func (osFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

func (osFS) CreateTemp(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

func (osFS) MkdirTemp(dir, pattern string) (string, error) {
	return os.MkdirTemp(dir, pattern)
}

func (osFS) Mkdir(name string, perm os.FileMode) error {
	return os.Mkdir(name, perm)
}

func (osFS) MkdirAll(name string, perm os.FileMode) error {
	return os.MkdirAll(name, perm)
}

func (osFS) Remove(name string) error {
	return os.Remove(name)
}

func (osFS) RemoveAll(name string) error {
	return os.RemoveAll(name)
}

func (osFS) Rename(oldname, newname string) error {
	return os.Rename(oldname, newname)
}

func (osFS) Stat(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

func (osFS) ReadDir(name string) ([]os.DirEntry, error) {
	return os.ReadDir(name)
}

func (osFS) DirFS(dir string) fs.FS {
	return os.DirFS(dir)
}
