# Testing a humus App

A humus App runs whole inside a test.
[humustest](https://pkg.go.dev/github.com/z5labs/humus/humustest) makes that
possible:

- **`humustest.Start`** hands the end user's `Init` to the App that
  `humus.Main` would run for it, with the same defaults, on a test Host in
  place of `humus.OSHost()` and on a new process from
  [bedrocktest](https://pkg.go.dev/github.com/z5labs/bedrock/bedrocktest):
  its arguments, environment and stdin, which the test sets, its output, which
  the test reads, and its signals, which the test sends. It returns the
  process and a channel for the App's exit status.
- a **`Host`**, whose in-memory network the App listens on and the test
  dials, and whose filesystem is confined to the test's own temp directory;
- a certificate authority, **`NewCA`**, generated for each test;
- **`Telemetry`**, a recorder of what an App emits, covered on the
  [Telemetry](/humus/pr-preview/pr-1308/fundamentals/telemetry/#testing) page;
- and **`TestService`**, the contract every Service must pass.

The network opens no real socket and starts no goroutines, so the App runs in a
[`testing/synctest`](https://pkg.go.dev/testing/synctest) bubble. It needs no
real environment, no real time and no file outside the test's temp directory.

The design behind everything on this page is in
[Proposal: humustest](https://github.com/z5labs/humus/discussions/633#discussioncomment-18964655),
and `Start` is in
[Proposal: humus.Main](https://github.com/z5labs/humus/discussions/687),
with its [revision 1](https://github.com/z5labs/humus/discussions/687#discussioncomment-18983867)
and [revision 2](https://github.com/z5labs/humus/discussions/687#discussioncomment-18986870).

## A whole-App test

`helloInit` is the end user's `Init`. Its Service serves hello over HTTP on
`:8080` of its `Setup`'s Host, and keeps serving until `Shutdown`:

```go
// helloService serves hello over HTTP on ":8080" of its Setup's Host, as
// the end user's HTTP Service would. It keeps serving until Shutdown.
type helloService struct {
	setup *humus.Setup
	srv   http.Server
}

func (h *helloService) Serve(context.Context) error {
	ln, err := h.setup.Host.Listen("tcp", ":8080")
	if err != nil {
		return err
	}
	h.srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	})
	if err := h.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (h *helloService) Shutdown(ctx context.Context) error { return h.srv.Shutdown(ctx) }

// helloInit is the end user's Init of the package doc's example.
func helloInit(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
	return []humus.Service{&helloService{setup: s}}, nil
}
```

This test hands `helloInit` to `Start`, sends the App a request and stops it
with SIGTERM:

```go
func TestInit_serves_hello(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		p, status := humustest.Start(t, helloInit,
			humustest.WithApp(humus.WithHost(h.Host())),
		)

		// Wait until every goroutine in the bubble is blocked, which
		// includes the App blocked in Accept on :8080.
		synctest.Wait()

		resp, err := h.HTTPClient().Get("http://localhost:8080/")
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || string(b) != "hello" {
			t.Errorf("body = %q, %v, want hello", b, err)
		}

		p.Signal(syscall.SIGTERM)
		st := <-status
		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
	})
}
```

The Service listens through `s.Host.Listen`, never through `net.Listen`, so it
listens on the test Host. `Start` builds a Host of its own, and
`humus.WithHost(h.Host())` replaces it with one the test holds, so that the
test can dial it: `h.HTTPClient()` is an `http.Client` that dials the Host,
with keep-alives off so that no idle connection outlives a request.
`h.Host()` is the `humus.Host` of the test Host, just as `p.Process()` is the
`bedrock.Process` of bedrocktest's process.

Build the Host, and a CA, inside the bubble that uses them, with the bubble's
`t`, and call `Start` there too, so that everything they make belongs to that
bubble.

## Start

`humustest.Start(t, init, opts...)` runs the App that `humus.Main` would run
for `init`. With no options, that App has:

- a new humustest Host, in place of `humus.OSHost()`;
- `Main`'s telemetry default, so its log records reach `p.Stdout()` as JSON
  lines;
- `Main`'s 20s grace period, and no drain.

Its options are of two kinds, in one list, so a whole-App test needs no
bedrocktest options:

- **`humustest.WithApp(opts...)`** carries humus's own options, the ones
  `main` passes to `humus.Main`, such as `humus.WithHost`,
  `humus.WithTelemetry`, `humus.WithShutdownGracePeriod` and
  `humus.WithDrain`. They are applied after `Start`'s defaults, in order, and
  `WithApp` given more than once applies every one's options in the order
  they were given, so a later option wins. An option sees the test Host and
  `Init`, and may wrap them.
- **`humustest.WithEnv`, `WithArgs` and `WithStdin`** set the process's
  environment, its arguments after the program name, and its standard input,
  as bedrocktest's options of the same names do.

This test sets all three for an `Init` that reads them:

```go
_, status := humustest.Start(t, init,
	humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
	humustest.WithEnv(map[string]string{"EXPORT_BUCKET": "nightly"}),
	humustest.WithArgs("-dry-run", "rows.csv"),
	humustest.WithStdin(strings.NewReader("row 1\nrow 2\n")),
)
```

`Init` then sees `s.Process.Args` as `bedrocktest.ProgramName` followed by
`-dry-run` and `rows.csv`, `EXPORT_BUCKET` as `nightly`, and the two rows on
`s.Process.Stdin`.

### Sharing main's options

A test runs the App `main` runs only if it passes the options `main` passes.
An end user whose `main` passes options that a test should keep, such as a
grace period, puts them in one function that returns them:

```go
// mainOptions returns the options main passes to humus.Main, so that a
// test passes them too and runs with production's grace period.
func mainOptions() []humus.Option {
	return []humus.Option{
		humus.WithTelemetry(buildTelemetry),
		humus.WithShutdownGracePeriod(30 * time.Second),
	}
}
```

`main` passes its result to `humus.Main`:

```go
humus.Main(Init, mainOptions()...)
```

And the test passes it with `WithApp`, before its own Host and telemetry, so
that those win:

```go
p, status := humustest.Start(t, Init,
	humustest.WithApp(mainOptions()...),
	humustest.WithApp(humus.WithTelemetry(tel.Builder())),
	withEnv(nil),
)
```

### A base environment

`WithEnv`, `WithArgs` and `WithStdin` work like bedrocktest's options: if one
is given more than once, the last one wins. A second `WithEnv` replaces the
first map rather than adding to it. So a helper that sets a base environment,
and lets a test add to it, merges the maps itself:

```go
// withEnv sets the environment Init needs, and adds more to it. Given more
// than once, humustest.WithEnv keeps only the last one, so a helper that
// sets a base environment merges the maps itself.
func withEnv(more map[string]string) humustest.Option {
	env := map[string]string{"DATABASE_URL": "postgres://db.internal:5432/greeter"}
	maps.Copy(env, more)
	return humustest.WithEnv(env)
}
```

### Runtime packages

A runtime package whose Services need a drain has a `Main` of its own, with
its defaults, such as a drain and a shorter grace period, as one
`humus.Option` ([Runtime packages](/humus/pr-preview/pr-1308/fundamentals/lifecycle/#runtime-packages)).
A test of a `main` that calls the package's `Main` passes the same defaults
with `WithApp`, before any options of its own, as
`humustest.Start(t, Init, humustest.WithApp(httpDefaults()))` does with the
stand-in on the lifecycle page. Otherwise the test runs on `humus.Main`'s
defaults, with no drain, and not on the ones production runs with.

A runtime package may also ship a test package of its own, named after it,
whose `Start` is `humustest.Start` with the package's defaults, along with
helpers such as a client wired to the test Host. A test that uses it can't
forget the defaults. `httpserver` ships `httpservertest`, with `Start`, which
is `humustest.Start` with `httpserver.Defaults()` applied before the test's
options, and `NewClient`, which returns an HTTP/2 client for the test Host
that trusts the test's CA. [Serving HTTP](/humus/pr-preview/pr-1308/services/http/serving/#testing)
shows both. `grpcserver` ships `grpcservertest`, with `Start`, which is
`humustest.Start` with `grpcserver.Defaults()` applied before the test's
options, and `NewClient`, which returns a gRPC client that dials the Service
through the test Host, over TLS, trusting the test's CA.
[Serving gRPC](/humus/pr-preview/pr-1308/services/grpc/serving/#testing) shows both.

A runtime package with no defaults of its own needs no test package.
`queue/kafka` ships none: a consumer runs on `humus.Main`'s defaults, so
`humustest.Start` already runs it on the ones production uses, and the fake
Kafka cluster a test runs it against listens on the test Host with one option.
[Consuming and producing Kafka](/humus/pr-preview/pr-1308/services/kafka/#testing) shows how.

`queue/rabbitmq` ships no test package and no fake broker. Its consumer runs
on `humus.Main`'s defaults too, so it needs no test package for them, and
humus is neither RabbitMQ's client nor its server, so it doesn't write or
keep a broker for a test to run against, and no in-memory RabbitMQ exists for
Go. So an App that uses RabbitMQ can't run whole inside `synctest` on a test
Host, as the charter's principle 6 asks of every App, and the charter's
[revision 10](https://github.com/z5labs/humus/discussions/629#discussioncomment-19009924)
records the exception. It is tested in two parts:

- **Its `Processor`, as a unit,** inside `synctest`, called with
  `rabbitmq.Message` values, with its other dependencies on the test Host
  and its publishing behind an interface of its own over `Publish`, which
  the test fakes.
- **The whole App, against RabbitMQ in a container,** with `humustest.Start`
  and `humus.WithHost(humus.OSHost())`, in real time. The test declares its
  queues itself, generates the broker's login rather than use `guest`, and
  sits behind a build tag, in a CI job of its own.

[Consuming and publishing with RabbitMQ](/humus/pr-preview/pr-1308/services/rabbitmq/#testing)
shows both.

The `kube` module ships no test package and no fake API server either, for
the same reason, and client-go's fakes can't apply an unstructured object or
run admission, validation, defaulting or garbage collection. A controller
runs on `humus.Main`'s defaults, and is tested in three parts:

- **Its Reconciler, as a function,** handed a plain `kube.Object`, with the
  resources it applies going through client-go's `kubefake.NewClientset()`.
- **Its CRD,** with a golden test of `Resource.CRD()`'s YAML.
- **The whole App, against k3s in a container,** with `humustest.Start` and
  `humus.WithHost(humus.OSHost())`, in real time, behind a build tag, in a
  CI job of its own.

[Writing a Kubernetes controller](/humus/pr-preview/pr-1308/services/kubernetes/#testing)
shows all three.

### Helpers

`Start` returns bedrocktest's `Process` and the App's status channel
unwrapped. A test calls their methods, such as `p.Signal` and `p.Stdout()`,
without importing bedrocktest. A helper that names the process or the status,
as `*bedrocktest.Process` or `bedrock.ExitStatus`, imports bedrocktest and
bedrock, since humustest builds on them:

```go
// stopApp sends SIGTERM once the App listens for it and every goroutine in
// the bubble is blocked, and returns the App's exit status. It names
// bedrocktest's Process and bedrock's ExitStatus, so it imports both.
func stopApp(p *bedrocktest.Process, status <-chan bedrock.ExitStatus) bedrock.ExitStatus {
	<-p.Notified(syscall.SIGTERM)
	synctest.Wait()
	p.Signal(syscall.SIGTERM)
	return <-status
}
```

A test of an App value it builds itself, rather than of the end user's
`Init`, runs it with `bedrocktest.Start`, as the lifecycle's own tests do.

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
```

### Resolvers inside synctest

The Host never resolves a name, so a client that dials it must not resolve one
either. A gRPC client's target must not use the `dns` resolver, grpc-go's
default for a target with no scheme, such as `carts:9090`. That resolver looks
the host up with `net.DefaultResolver`, whatever dialler the client has, on
the real network, where a name that exists only on the test Host is not
found. Inside a `synctest` bubble the lookup crashes the test binary with
`fatal error: send on synctest channel from outside bubble`.

`grpcservertest.NewClient` targets `passthrough:///` and the address, and
dials through the Host, so no lookup is made. A test that builds a gRPC client
of its own does the same, as
[Serving gRPC](/humus/pr-preview/pr-1308/services/grpc/serving/#the-dns-resolver-and-synctest)
describes.

### A client next to a server

An App that calls another service is tested with that service on the same
Host. The dependency runs as a second App, started with `humustest.Start` on
`h.Host()`, and the end user's App, on the same Host, dials it by the name
it listens on. Both Apps run in the test's `synctest` bubble, so the calls
between them take no real time, and stopping the dependency's App is an
outage of it.

Here the dependency is a `grpcserver` Service at `carts:9090`, with a
certificate for `carts` from the test's CA:

```go
// startCarts runs the dependency on h, at carts:9090, over TLS with a
// certificate for carts from ca. It returns a function that stops the
// dependency and requires it to exit 0.
func startCarts(t *testing.T, h *humustest.Host, ca *humustest.CA) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, "carts")
	p, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: "carts:9090",
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		})
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, carts{})
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening("carts:9090")
	return func() {
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}
```

The end user's App builds a [`grpcclient`](/humus/pr-preview/pr-1308/dependencies/grpc/)
Client of `carts:9090` in its `Init`, from `CARTS_ADDR` and `CARTS_CA_FILE`,
and serves its own gRPC service, whose handler calls `carts`. The test gives
it the dependency's CA and its own certificate through the Host's
filesystem, starts it next to the dependency, and calls it:

```go
ca := humustest.NewCA(t)
orders := ca.Issue(t, "orders")
h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
	"tls.pem":      pemFile(t, orders),
	"carts-ca.pem": caPEM(orders),
}))
stop := startCarts(t, h, ca)
defer stop()

p, exit := grpcservertest.Start(t, Init,
	humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
	humustest.WithEnv(map[string]string{
		"GRPC_ADDR":         ":8443",
		"GRPC_TLS_PEM_FILE": "tls.pem",
		"CARTS_ADDR":        "carts:9090",
		"CARTS_CA_FILE":     "carts-ca.pem",
	}),
)
<-h.Listening("orders:8443")

client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "orders:8443", ca.Pool()))
for range 2 {
	resp, err := client.UnaryCall(t.Context(), &testpb.SimpleRequest{})
	require.NoError(t, err)
	require.Equal(t, "orders, via carts", resp.GetUsername())
}
```

The `Client` dials through its Setup's Host, which is the test's, so it
reaches `carts:9090` on the Host's network, and the dependency's certificate
verifies against the CA the test wrote. Each App waits for its own
listener: the test waits for `carts:9090` before it starts the end user's
App, and for `orders:8443` before it calls it. An
[`httpclient`](/humus/pr-preview/pr-1308/dependencies/http/#testing) client is tested the
same way, next to an `httpserver` App.

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
```

Real files are what let libraries that need an `*os.File` or a path, such as
SQLite drivers, archive libraries and programs started with `os/exec`, keep
working in a test. They cost two things:

- **No injected I/O errors.** A test can't make a write fail with a full disk.
  Code that needs that test puts an `io.Writer` seam in its own code.
- **Real `ModTime`, even inside `synctest`.** A bubble's clock is fake, and a
  file's modification time is not, so code that compares `ModTime()` with
  `time.Now()` needs `os.Chtimes` in its tests.

[The App lifecycle](/humus/pr-preview/pr-1308/fundamentals/lifecycle/#why-fs-hands-out-osfile)
explains why `FS` hands out `*os.File`.

## TLS

A test that serves TLS generates what it needs with `NewCA`, and never uses a
fixed key, certificate or password. Each CA, and each certificate it issues
with `Issue`, has a new random key and serial number, and each `PKCS12` bundle
a new random password. So no secret is ever written into the repository, and
nothing is shared between tests.

This App reads its certificate from a PKCS#12 bundle, through its Host, and
serves HTTPS. The test writes the bundle with `WithFiles`, passes its path and
password in settings, set with `humustest.WithEnv`, and trusts the CA through
`ca.Pool()`:

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
		init := func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			cfg, err := tlsFromPKCS12(ctx, s)
			if err != nil {
				return nil, err
			}
			return []humus.Service{newHTTPSService(s, ":8443", cfg, hello)}, nil
		}
		p, status := humustest.Start(t, init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{
				"TLS_PKCS12_FILE":     h.Path("etc/tls/server.p12"),
				"TLS_PKCS12_PASSWORD": password,
			}),
		)
		synctest.Wait()

		client := h.HTTPClient()
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
		resp, err := client.Get("https://api.internal:8443/")
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "hello over TLS" {
			t.Errorf("body = %q, want hello over TLS", b)
		}

		p.Signal(syscall.SIGTERM)
		st := <-status
		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
	})
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
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	key, leaf, chain, err := pkcs12.DecodeChain(b, password)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	cert := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
	for _, c := range chain {
		cert.Certificate = append(cert.Certificate, c.Raw)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}
```

`h.HTTPClient()` returns a new client, with a new `*http.Transport`, on every
call, so setting its `TLSClientConfig` changes no other client.

A `Transport` with a dialer or a `TLSClientConfig` of its own negotiates only
HTTP/1.1 unless `ForceAttemptHTTP2` or `Protocols` says otherwise, so the
client above, which has both, speaks HTTP/1.1. For an HTTP/2 client, such as one for a Service from
`httpserver`, use `httpservertest.NewClient(h, ca.Pool())`: it trusts only the
CA's pool and speaks only HTTP/2, as
[Serving HTTP](/humus/pr-preview/pr-1308/services/http/serving/#testing) describes.

A server that takes a `tls.Certificate` rather than a file serves
`cert.TLS()` instead, and a client that dials the Host itself trusts the CA's
pool:

```go
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
```

A connection on the Host's network buffers up to 256 KiB in each direction, as
a real socket does, so a `Write` returns once its bytes are buffered, not once
the peer reads them, and waits only while the buffer is full. So a TLS
connection closes as it does on a real socket: its `close_notify` alert is
buffered like any other write, and `Close` doesn't wait for a peer that isn't
reading unless the buffer is full, whatever happens to the connection's write
deadline meanwhile.

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

1. **Its Service runs whole under `synctest`**, returned by an `Init` that
   the test hands to `humustest.Start`, with the package's defaults when it
   has any.
2. **No test calls `t.Setenv`, opens a real socket or touches a file outside
   its own temp directory**, except integration tests behind a build tag.
3. **Negative tests assert why something failed** — the cause, with
   `errors.Is` or `errors.AsType` — not only the exit code. A test that checks
   a logged failure reads it from humustest's telemetry recorder,
   [`humustest.Telemetry`](/humus/pr-preview/pr-1308/fundamentals/telemetry/#testing).
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

The [App lifecycle](/humus/pr-preview/pr-1308/fundamentals/lifecycle/#ambient-leaks) page lists the
dependencies that read the real process behind an App's back, which a whole-App
test can neither control nor see.

Next: [Service types](/humus/pr-preview/pr-1308/services/)
