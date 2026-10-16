// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"software.sslmate.com/src/go-pkcs12"

	"github.com/z5labs/humus/humustest"
)

// The docs site quotes the code in this file, and the root package's
// TestDocs_code_samples checks that every quote still matches it.

// exampleTB stands in for the *testing.T an Example does not have. In a
// test, pass t to humustest.NewHost and humustest.NewCA instead. It
// implements only the methods humustest calls: Helper, Errorf, Fatalf,
// Cleanup and TempDir. Any other method panics on the nil testing.TB.
type exampleTB struct {
	testing.TB
	cleanups []func()
}

func (*exampleTB) Helper() {}

func (*exampleTB) Errorf(format string, args ...any) {
	fmt.Printf("test failed: "+format+"\n", args...)
}

func (*exampleTB) Fatalf(format string, args ...any) {
	panic(fmt.Sprintf("test failed: "+format, args...))
}

func (tb *exampleTB) Cleanup(f func()) {
	tb.cleanups = append(tb.cleanups, f)
}

// TempDir makes a new directory, removed by runCleanups, as t.TempDir's is
// removed when the test ends.
func (tb *exampleTB) TempDir() string {
	dir, err := os.MkdirTemp("", "humustest-example-")
	if err != nil {
		tb.Fatalf("TempDir: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func (tb *exampleTB) runCleanups() {
	for i := len(tb.cleanups) - 1; i >= 0; i-- {
		tb.cleanups[i]()
	}
}

// A Host's network is in memory. Listen holds a name and port, and Dial
// reaches the listener for exactly that name: names are compared as
// written and never resolved, so 127.0.0.1 is not localhost. A Dial that
// finds no listener is refused at once; it never waits for one.
func ExampleNewHost() {
	t := new(exampleTB)
	defer t.runCleanups()

	h := humustest.NewHost(t)
	ln, err := h.Listen("tcp", "localhost:8080")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "hello from localhost:8080")
	}()

	conn, err := h.Dial(context.Background(), "tcp", "localhost:8080")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = conn.Close() }()
	b, err := io.ReadAll(conn)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(b))

	_, err = h.Dial(context.Background(), "tcp", "127.0.0.1:8080")
	fmt.Println("127.0.0.1:8080 refused:", errors.Is(err, syscall.ECONNREFUSED))
	// Output:
	// hello from localhost:8080
	// 127.0.0.1:8080 refused: true
}

// A test waits for a listener before it dials. Inside a synctest bubble,
// synctest.Wait does that for every listener at once; Listening waits for
// one, and works outside a bubble too. A listener on every name, such as
// ":8080", is reached by a Dial to any name on its port.
func ExampleHost_Listening() {
	t := new(exampleTB)
	defer t.runCleanups()

	h := humustest.NewHost(t)
	_, err := h.Dial(context.Background(), "tcp", "localhost:8080")
	fmt.Println("before Listen, refused:", errors.Is(err, syscall.ECONNREFUSED))

	// The goroutine stands in for an App, which listens when it gets to
	// it. Listening is called first, so the test can't miss the listener.
	listening := h.Listening("localhost:8080")
	served := make(chan struct{})
	go func() {
		defer close(served)
		ln, err := h.Listen("tcp", ":8080")
		if err != nil {
			fmt.Println(err)
			return
		}
		defer func() { _ = ln.Close() }()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "hello from every name")
	}()

	select {
	case <-listening:
	case <-served: // Listen failed, and the goroutine said why
		return
	}
	conn, err := h.Dial(context.Background(), "tcp", "localhost:8080")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = conn.Close() }()
	b, err := io.ReadAll(conn)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(b))
	<-served
	// Output:
	// before Listen, refused: true
	// hello from every name
}

// WithFiles writes the files a test sets up into the Host's directory
// before the App starts, and Path gives the absolute OS path of one, to
// put in a setting. The Host's FS opens it there, and refuses a path that
// leaves the directory.
func ExampleWithFiles() {
	t := new(exampleTB)
	defer t.runCleanups()

	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"etc/app/greeting.txt": []byte("hello from a real file"),
	}))
	path := h.Path("etc/app/greeting.txt")
	fmt.Println("absolute:", filepath.IsAbs(path))

	f, err := h.FS().Open(path)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(b))

	_, err = h.FS().Open("../outside.txt")
	fmt.Println("outside the directory refused:", err != nil)
	// Output:
	// absolute: true
	// hello from a real file
	// outside the directory refused: true
}

// NewCA generates a certificate authority for one test, and Issue a
// certificate signed by it, each with a new random key. The server serves
// the certificate's TLS form, and the client trusts the CA's Pool, so a
// TLS handshake over the Host's network verifies.
func ExampleNewCA() {
	t := new(exampleTB)
	defer t.runCleanups()

	ca := humustest.NewCA(t)
	cert := ca.Issue(t, "api.internal")

	h := humustest.NewHost(t)
	ln, err := h.Listen("tcp", "api.internal:8443")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		srv := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert.TLS()}})
		defer func() { _ = srv.Close() }()
		_, _ = io.WriteString(srv, "hello over TLS")
	}()

	conn, err := h.Dial(context.Background(), "tcp", "api.internal:8443")
	if err != nil {
		fmt.Println(err)
		return
	}
	client := tls.Client(conn, &tls.Config{RootCAs: ca.Pool(), ServerName: "api.internal"})
	defer func() { _ = client.Close() }()
	b, err := io.ReadAll(client)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(b))
	fmt.Println("verified for:", client.ConnectionState().PeerCertificates[0].DNSNames)
	// Output:
	// hello over TLS
	// verified for: [api.internal]
}

// PKCS12 encodes a certificate and its key as a bundle for a file, with a
// new random password on every call. A test writes the bundle with
// WithFiles and passes the password in a setting, so no key or password is
// ever written into a repository.
func ExampleCertificate_PKCS12() {
	t := new(exampleTB)
	defer t.runCleanups()

	ca := humustest.NewCA(t)
	bundle, password := ca.Issue(t, "api.internal").PKCS12(t)
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"etc/tls/server.p12": bundle,
	}))

	// The App would read the path and the password from its settings.
	b, err := os.ReadFile(h.Path("etc/tls/server.p12"))
	if err != nil {
		fmt.Println(err)
		return
	}
	_, leaf, _, err := pkcs12.DecodeChain(b, password)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("decoded:", leaf.DNSNames)

	_, other := ca.Issue(t, "api.internal").PKCS12(t)
	fmt.Println("same password twice:", other == password)
	// Output:
	// decoded: [api.internal]
	// same password twice: false
}
