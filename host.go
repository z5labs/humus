// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"io/fs"
	"os"

	"github.com/z5labs/bedrock/host"
)

// Host is what humus hands an App beyond its process: the network and the
// filesystem. Every field is required.
//
// In main, an App runs on OSHost. In a test, it runs on a Host whose
// listeners, dialler and filesystem the test controls, so a whole App can
// run without touching the real host.
type Host = host.Host[FS]

// FS is the filesystem of a Host. Names are OS paths, as accepted by the
// functions of the same name in package os, and files are *os.File, so a
// file opened through an FS can be handed to anything that takes an OS file
// or a path.
//
// FS is sealed by host.FS: host.OSFS and hosttest's *FS implement it, and
// a type elsewhere satisfies it by embedding one of them, or FS itself.
type FS interface {
	host.FS

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

// OSHost returns the Host of the real machine: the fields of host.OS, with
// its OSFS as the FS.
func OSHost() Host {
	h := host.OS()
	return Host{Listen: h.Listen, ListenPacket: h.ListenPacket, Dial: h.Dial, FS: h.FS}
}
