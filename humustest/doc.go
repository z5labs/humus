// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humustest runs a whole humus App inside a test.
//
// bedrocktest supplies the process an App runs on: its arguments,
// environment, output and signals, and Start, which runs an App on it.
// humustest supplies what a humus App needs beyond the process:
//
//   - a [Host], whose in-memory network the App listens on and the test
//     dials, and whose filesystem is confined to the test's own temp
//     directory;
//   - a certificate authority, [NewCA], generated for each test;
//   - [TestService], the contract every humus.Service must pass.
//
// A whole-App test imports both. The network opens no real socket and
// starts no goroutines, so the App runs inside a testing/synctest bubble,
// with no real environment, no real time and no file outside the test's
// temp directory. Here httpApp returns a humus.App on h whose Service
// serves hello over HTTP on ":8080":
//
//	func TestApp_Run_HTTP_a_whole_App_on_a_test_Host(t *testing.T) {
//		synctest.Test(t, func(t *testing.T) {
//			h := humustest.NewHost(t)
//			hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
//				_, _ = io.WriteString(w, "hello")
//			})
//			p, status := bedrocktest.Start(t, httpApp(h, hello))
//
//			// Wait until every goroutine in the bubble is blocked, which
//			// includes the App blocked in Accept on :8080.
//			synctest.Wait()
//
//			resp, err := h.HTTPClient().Get("http://localhost:8080/")
//			if err != nil {
//				t.Fatal(err)
//			}
//			// ... read and close the body, and check it ...
//
//			p.Signal(syscall.SIGTERM)
//			st := <-status
//			if st.Code() != 0 {
//				t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
//			}
//		})
//	}
//
// Build the Host, and a CA, inside the bubble that uses them, with the
// bubble's t.
//
// # Names
//
// The rules of the network follow a real one. A name and port can be held
// once, and a listener on every name (an empty host, 0.0.0.0 or ::)
// conflicts with any other listener on its port. Names are compared as
// written and never resolved, so localhost and 127.0.0.1 are different
// names: a client dials the name a Service listens on, or the Service
// listens on every name, as ":8080" does above.
//
// # Waiting for a listener
//
// [Host.Dial] refuses rather than waits. A Dial with no listener fails at
// once with ECONNREFUSED, as a real dial does, because a test that dials
// before the App listens has dialled too early, and waiting would hide
// that, or turn it into a synctest deadlock report that doesn't name the
// cause. A test waits for the listener itself, before it dials, in one of
// two ways:
//
//   - Inside a synctest bubble, synctest.Wait, which returns once every
//     goroutine in the bubble is blocked, including the App blocked in
//     Accept.
//   - Anywhere, <-h.Listening(address), which waits for one particular
//     listener, like bedrocktest's Process.Notified. [Host.Listening] also
//     works outside a bubble.
//
// # Files
//
// The filesystem is real. [NewHost] takes a directory from t.TempDir(),
// which is removed when the test ends, and [WithFiles] writes the files a
// test sets up before the App starts. [Host.FS] opens files with an
// os.Root on that directory, so ".." and symlinks can't leave it, and
// [Host.Path] gives the absolute path of a file in it to put in a setting.
//
// Real files are what let libraries that need an *os.File or a path keep
// working in a test, and they cost two things. A test can't inject an I/O
// error, such as a full disk, so code that needs that test puts an
// io.Writer seam in its own code. And a file's ModTime is real even inside
// a synctest bubble, whose clock is fake, so code that compares ModTime
// with time.Now needs os.Chtimes in its tests.
//
// # TLS
//
// A test that serves TLS generates what it needs with [NewCA], and never
// uses a fixed key, certificate or password. Each CA and each certificate
// it issues with [CA.Issue] has a new random key and serial number, and
// each [Certificate.PKCS12] bundle a new random password, so no secret is
// ever written into a repository. A server serves [Certificate.TLS], or
// reads the PKCS#12 bundle from a file written with [WithFiles], and a
// client trusts the CA through [CA.Pool].
//
// A connection on the network is a net.Pipe, which has no buffer. When
// both ends of a TLS connection close at once, each waits to send its
// close_notify alert until crypto/tls's five-second write deadline passes.
// Inside a synctest bubble that wait takes no time; outside one, it is
// real.
//
// # Services
//
// A package that provides a humus.Service checks it with [TestService],
// which runs the Service contract against it inside synctest: Serve returns
// nil after Shutdown, Shutdown with a done context stops the Service at
// once, Shutdown is safe before Serve, after Serve has returned and twice,
// and no goroutine the Service started outlives Serve.
//
// The docs site's page "Testing a humus App",
// https://z5labs.github.io/humus/testing/, covers the same ground, with
// the requirements every package's tests meet. The design is Proposal:
// humustest: https://github.com/z5labs/humus/discussions/633
package humustest
