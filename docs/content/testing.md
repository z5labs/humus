---
title: Testing a humus App
linkTitle: Testing
description: >
  How to run a whole humus App inside a test with bedrocktest and humustest:
  under synctest, with no real socket, no file outside the test's temp
  directory and no secret in the repository.
type: docs
weight: 20
---

A humus App runs whole inside a test. Two packages make that possible, and a
whole-App test imports both:

- [bedrocktest](https://pkg.go.dev/github.com/z5labs/bedrock/bedrocktest)
  supplies the process the App runs on: its arguments and environment, which
  the test sets, its output, which the test reads, and its signals, which the
  test sends. `bedrocktest.Start` runs an App on a new process and hands back
  the process and a channel for the App's exit status.
- [humustest](https://pkg.go.dev/github.com/z5labs/humus/humustest) supplies
  what a humus App needs beyond the process: a `Host`, whose in-memory network
  the App listens on and the test dials, and whose filesystem is confined to
  the test's own temp directory; a certificate authority, `NewCA`, generated
  for each test; and `TestService`, the contract every Service must pass.

The network opens no real socket and starts no goroutines, so the App runs in a
[`testing/synctest`](https://pkg.go.dev/testing/synctest) bubble. It needs no
real environment, no real time and no file outside the test's temp directory.

The design behind everything on this page is in
[Proposal: humustest](https://github.com/z5labs/humus/discussions/633#discussioncomment-18964655).
Telemetry in tests is added to humustest by
[Proposal: OpenTelemetry without globals](https://github.com/z5labs/humus/discussions/632),
and documented here when it lands.

## A whole-App test

This test runs an App whose Service serves HTTP on `:8080`, sends it a request
and stops it with SIGTERM:

```go
func TestApp_Run_HTTP_a_whole_App_on_a_test_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello")
		})
		p, status := bedrocktest.Start(t, httpApp(h, hello))

		// Wait until every goroutine in the bubble is blocked, which
		// includes the App blocked in Accept on :8080.
		synctest.Wait()

		resp, err := h.HTTPClient().Get("http://localhost:8080/")
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "hello" {
			t.Errorf("body = %q, want hello", b)
		}

		p.Signal(syscall.SIGTERM)
		st := <-status
		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		requireSignalCause(t, st.Cause())
		requireRefused(t, h, "localhost:8080")
	})
}
```

`httpApp` builds the App on the Host. Its `Host` field is `h.Host()`, the
`humus.Host` of the test Host, just as a test passes `p.Process()` where a
`bedrock.Process` is wanted:

```go
func httpApp(h *humustest.Host, handler http.Handler, more ...humus.Service) humus.App {
	return humus.App{
		Host: h.Host(),
		Init: func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			return append([]humus.Service{newHTTPService(s, ":8080", handler)}, more...), nil
		},
	}
}
```

The Service listens through `s.Host.Listen`, never through `net.Listen`, so it
listens on the test Host. `h.HTTPClient()` is an `http.Client` that dials the
Host, with keep-alives off so that no idle connection outlives a request.

Build the Host, and a CA, inside the bubble that uses them, with the bubble's
`t`, so that everything they make belongs to that bubble.

## The network

The Host's network follows the rules of a real one.

- **A name and port can be held once.** A listener on every name, an empty
  host, `0.0.0.0` or `::`, conflicts with any other listener on its port.
  A second `Listen` on a held name and port fails with `EADDRINUSE`.
- **Names are compared as written** and never resolved. `localhost` and
  `127.0.0.1` are different names, so a client dials the name a Service
  listens on, or the Service listens on every name, as `:8080` does above.

```go
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
```

### Waiting for a listener

`Dial` refuses rather than waits. A `Dial` with no listener fails at once with
`ECONNREFUSED`, as a real dial does. A test that dials before the App listens
has dialled too early, and waiting for a listener would hide that, or turn it
into a `synctest` deadlock report that doesn't name the cause.

So a test waits for the listener itself, before it dials, in one of two ways:

- **Inside a `synctest` bubble, `synctest.Wait()`.** It returns once every
  goroutine in the bubble is blocked, which includes the App blocked in
  `Accept`. That is what the whole-App test above does.
- **Anywhere, `<-h.Listening(address)`.** It waits for one particular
  listener, and works outside a bubble too. It works like bedrocktest's
  `Notified`: once closed, the channel stays closed.

```go
h := humustest.NewHost(t)
_, err := h.Dial(context.Background(), "tcp", "localhost:8080")
fmt.Println("before Listen, refused:", errors.Is(err, syscall.ECONNREFUSED))

// The goroutine stands in for an App, which listens when it gets to
// it. Listening is called first, so the test can't miss the listener.
listening := h.Listening("localhost:8080")
```

## Files

The filesystem is real. `NewHost` takes a directory from `t.TempDir()`, which
is removed when the test ends, and `WithFiles` writes the files a test sets up
before the App starts. `Host.FS` opens files with an `os.Root` on that
directory, so `..` and symbolic links can't leave it, and `Host.Path` gives
the absolute path of a file in it to put in a setting:

```go
h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
	"etc/app/greeting.txt": []byte("hello from a real file"),
}))
path := h.Path("etc/app/greeting.txt")
fmt.Println("absolute:", filepath.IsAbs(path))

f, err := h.FS().Open(path)
```

Real files are what let libraries that need an `*os.File` or a path, such as
SQLite drivers, archive libraries and programs started with `os/exec`, keep
working in a test. They cost two things:

- **No injected I/O errors.** A test can't make a write fail with a full disk.
  Code that needs that test puts an `io.Writer` seam in its own code.
- **Real `ModTime`, even inside `synctest`.** A bubble's clock is fake, and a
  file's modification time is not, so code that compares `ModTime()` with
  `time.Now()` needs `os.Chtimes` in its tests.

[The App lifecycle]({{< relref "lifecycle#why-fs-hands-out-osfile" >}})
explains why `FS` hands out `*os.File`.

## TLS

A test that serves TLS generates what it needs with `NewCA`, and never uses a
fixed key, certificate or password. Each CA, and each certificate it issues
with `Issue`, has a new random key and serial number, and each `PKCS12` bundle
a new random password. So no secret is ever written into the repository, and
nothing is shared between tests.

This App reads its certificate from a PKCS#12 bundle, through its Host, and
serves HTTPS. The test writes the bundle with `WithFiles`, passes its path and
password in settings, and trusts the CA through `ca.Pool()`:

```go
func TestApp_Run_HTTPS_with_a_PKCS12_bundle_from_NewCA(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		bundle, password := ca.Issue(t, "api.internal").PKCS12(t)
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"etc/tls/server.p12": bundle,
		}))
		hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello over TLS")
		})
		app := humus.App{
			Host: h.Host(),
			Init: func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
				cfg, err := tlsFromPKCS12(ctx, s)
				if err != nil {
					return nil, err
				}
				return []humus.Service{newHTTPSService(s, ":8443", cfg, hello)}, nil
			},
		}
		p, status := bedrocktest.Start(t, app, bedrocktest.WithEnv(map[string]string{
			"TLS_PKCS12_FILE":     h.Path("etc/tls/server.p12"),
			"TLS_PKCS12_PASSWORD": password,
		}))
		synctest.Wait()

		client := h.HTTPClient()
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
		resp, err := client.Get("https://api.internal:8443/")
		if err != nil {
			t.Fatal(err)
		}
```

The setting names are only for illustration; each runtime package names its
own. `tlsFromPKCS12` opens the bundle through `s.Host.FS`, so the App reads it
from the test's directory here and from the real disk in production:

```go
func tlsFromPKCS12(ctx context.Context, s *humus.Setup) (*tls.Config, error) {
	path, err := config.Env(s.Process.Environment, "TLS_PKCS12_FILE").Read(ctx)
	if err != nil {
		return nil, err
	}
	password, err := config.Env(s.Process.Environment, "TLS_PKCS12_PASSWORD").Read(ctx)
	if err != nil {
		return nil, err
	}
	f, err := s.Host.FS.Open(path)
```

A server that takes a `tls.Certificate` rather than a file serves
`cert.TLS()` instead:

```go
ca := humustest.NewCA(t)
cert := ca.Issue(t, "api.internal")
```

```go
srv := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert.TLS()}})
```

A connection on the Host's network is a `net.Pipe`, which has no buffer. When
both ends of a TLS connection close at once, each waits to send its
`close_notify` alert until crypto/tls's five-second write deadline passes.
Inside a `synctest` bubble that wait takes no time; outside one, it is real.

## Checking a Service

A package that provides a `humus.Service` checks it with `TestService`, from a
test of its own:

```go
func TestHTTPService(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		return newHTTPService(s, ":8080", http.NotFoundHandler())
	})
}
```

`TestService` runs the Service contract against it inside `synctest`, each
rule in a subtest named after it:

- `Serve` returns `nil` after `Shutdown`.
- `Shutdown` with a context that is already done stops the Service at once,
  and returns an error that wraps the context's error.
- `Shutdown` is safe before `Serve`, after `Serve` has returned on its own, and
  twice.
- No goroutine the Service started outlives `Serve`.

The Service must reach the network and the filesystem only through the
`Setup`'s Host. A goroutine blocked on a real socket is one `synctest` can't
see, and the test then hangs until its timeout.

## For package authors

Every package proposal, and every package that lands from one, shows in its
tests that:

1. **Its App or Service runs whole under `synctest`** with bedrocktest and
   humustest.
2. **No test calls `t.Setenv`, opens a real socket or touches a file outside
   its own temp directory**, except integration tests behind a build tag.
3. **Negative tests assert why something failed** — the cause, with
   `errors.Is` or `errors.AsType` — not only the exit code. A test that checks
   a logged failure reads it from humustest's telemetry recorder, once
   [Proposal: OpenTelemetry without globals](https://github.com/z5labs/humus/discussions/632)
   adds it.
4. **Every Service passes `humustest.TestService`.**

And one rule for every test, fixture and `testdata` file:

- **No secrets.** None contains a private key, a certificate, a password or a
  token, even one made only for tests. A test generates the secrets it needs,
  such as with `humustest.NewCA`, so that no copy of a fixed key ends up in a
  repository or in the test suites copied from it.

A negative test that meets the third requirement asserts the cause:

```go
st := <-status
if st.Code() != humus.ExitFailure {
	t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
}
if !errors.Is(st.Cause(), pkcs12.ErrIncorrectPassword) {
	t.Errorf("cause = %v, want pkcs12.ErrIncorrectPassword", st.Cause())
}
```

The [App lifecycle]({{< relref "lifecycle#ambient-leaks" >}}) page lists the
dependencies that read the real process behind an App's back, which a whole-App
test can neither control nor see.
