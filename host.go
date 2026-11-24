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

	"github.com/z5labs/bedrock/host"
)

// IPNetwork is the network of the default Host. host.IPNetwork implements
// it, and so does the test host.
type IPNetwork interface {
	// ListenTCP announces on the local network address, for "tcp", "tcp4"
	// or "tcp6", like net.ListenConfig.Listen.
	ListenTCP(ctx context.Context, network, address string) (net.Listener, error)

	// DialTCP connects to address over "tcp", "tcp4" or "tcp6", like
	// net.Dialer.DialContext.
	DialTCP(ctx context.Context, network, address string) (net.Conn, error)

	// ListenUDP binds a UDP socket to the local network address, for "udp",
	// "udp4" or "udp6", like net.ListenConfig.ListenPacket.
	ListenUDP(ctx context.Context, network, address string) (net.PacketConn, error)

	// DialUDP returns a UDP socket connected to address, for "udp", "udp4"
	// or "udp6", like net.Dialer.DialContext. It sends only to the address
	// it dialled and takes datagrams only from it.
	DialUDP(ctx context.Context, network, address string) (net.Conn, error)
}

// Host is the default host: the network and the filesystem humus hands an
// App beyond its process. It embeds its network, so a Host's ListenTCP,
// DialTCP, ListenUDP and DialUDP are its IPNetwork's. Both fields are
// required.
//
// In main, an App runs on OSHost. In a test, it runs on a Host built from
// the test host's parts, so a whole App can run without touching the real
// host.
type Host struct {
	IPNetwork

	// Filesystem is the Host's filesystem.
	Filesystem Filesystem
}

// OSHost returns Host{IPNetwork: host.IPNetwork{}, Filesystem: host.OSFilesystem{}}.
func OSHost() Host {
	return Host{IPNetwork: host.IPNetwork{}, Filesystem: host.OSFilesystem{}}
}

// Filesystem is the filesystem of a Host. Names are OS paths, as accepted
// by the functions of the same name in package os, and files are *os.File,
// so a file opened through a Filesystem can be handed to anything that
// takes an OS file or a path.
//
// host.OSFilesystem and hosttest's *Filesystem implement it, and so does
// any type with its twelve methods.
type Filesystem interface {
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
