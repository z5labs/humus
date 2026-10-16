// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humustest runs a whole humus App inside a test.
//
// bedrocktest supplies the process an App runs on. humustest supplies what
// a humus App needs beyond it: a [Host], whose in-memory network the App
// listens on and the test dials, and whose filesystem is confined to the
// test's own temp directory. The network opens no real socket and starts
// no goroutines, so an App on it runs inside a testing/synctest bubble.
//
// The rules of the network follow a real one. A name and port can be held
// once, and a listener on every name (an empty host, 0.0.0.0 or ::)
// conflicts with any other listener on its port. Names are compared as
// written and never resolved, so localhost and 127.0.0.1 are different
// names. A Dial with no listener fails at once with ECONNREFUSED; a test
// waits for the App to listen before it dials, with synctest.Wait inside a
// bubble or with [Host.Listening] anywhere.
//
// The filesystem is real. [NewHost] takes a directory from t.TempDir(),
// which is removed when the test ends, and [WithFiles] writes the files a
// test sets up before the App starts. [Host.FS] opens files with an
// os.Root on that directory, so ".." and symlinks can't leave it, and
// [Host.Path] gives the absolute path of a file in it to put in a setting.
//
// A test that serves TLS generates what it needs with [NewCA]. Each CA and
// each certificate it issues has a new random key and serial number, and
// each [Certificate.PKCS12] bundle a new random password, so no key,
// certificate or password is ever written into a repository. A client
// trusts the CA through [CA.Pool].
//
// A connection on the network is a net.Pipe, which has no buffer. When
// both ends of a TLS connection close at once, each waits to send its
// close_notify alert until crypto/tls's five-second write deadline passes.
// Inside a synctest bubble that wait takes no time; outside one, it is
// real.
//
// The design is Proposal: humustest:
// https://github.com/z5labs/humus/discussions/633
package humustest
