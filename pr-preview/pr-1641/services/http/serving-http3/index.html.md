# Serving HTTP/3

The [`http3server`](https://pkg.go.dev/github.com/z5labs/humus/http3/http3server)
package serves an `http.Handler` over HTTP/3 as a `humus.Service`. It is in
the nested module `github.com/z5labs/humus/http3`, which keeps
[quic-go](https://github.com/quic-go/quic-go) out of the core module. No
quic-go type is in its API, and none of httpserver's: it has its own
`Settings`, `DefaultSettings`, a check per field, `Defaults` and `Main`. Its
test package,
[`http3servertest`](https://pkg.go.dev/github.com/z5labs/humus/http3/http3server/http3servertest),
runs the same App in a test, on the same defaults, with an HTTP/3 client.

HTTP/3 is served beside [Serving HTTP](/humus/pr-preview/pr-1641/services/http/serving/)'s
Service, not in place of it, so it is off unless the end user's `Init`
returns it. The design is in
[Proposal: HTTP/3](https://github.com/z5labs/humus/discussions/1438), with
its [revision 1](https://github.com/z5labs/humus/discussions/1438#discussioncomment-19867936)
and [revision 2](https://github.com/z5labs/humus/discussions/1438#discussioncomment-19871416).

## A service

The end user's `Init` returns an `httpserver` Service and an `http3server`
Service that serve one handler, on one `host:port`, with one TLS config:

```go
func Init(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	type servers struct {
		tcp httpserver.Settings
		udp http3server.Settings
	}
	settings, err := config.Struct(func(r *config.Resolver) servers {
		addr := r.Get(env("ADDR").Validate(http3server.CheckAddr))
		tlsConfig := r.Get(env("TLS_PEM_FILE").
			Parse(readFile(h.Filesystem)).
			Parse(serverTLS).
			Validate(httpserver.CheckTLS).
			Validate(http3server.CheckTLS))

		tcp := httpserver.DefaultSettings()
		tcp.Addr, tcp.TLS = addr, tlsConfig
		udp := http3server.DefaultSettings()
		udp.Addr, udp.TLS = addr, tlsConfig
		return servers{tcp: tcp, udp: udp}
	}).Read(ctx)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello/{name}", greet)

	tcp, err := httpserver.NewService(ctx, s, h, settings.tcp, mux)
	if err != nil {
		return nil, err
	}
	udp, err := http3server.NewService(ctx, s, h, settings.udp, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{tcp, udp}, nil
}
```

`readFile` and `serverTLS` are the end user's helpers, as on
[Serving HTTP](/humus/pr-preview/pr-1641/services/http/serving/#the-certificate).

- **One config serves both transports.** A config that allows TLS 1.2 and
  TLS 1.3 passes both packages' `CheckTLS`, so it is sourced once. quic-go
  clones it and offers `h3` in ALPN itself.
- **One address binds both.** TCP and UDP have separate ports, so the two
  Services bind one port number without conflict. The two packages'
  `CheckAddr` have the same rules, so one check is enough.
- **`NewService(ctx, s, n, settings, handler)` binds the UDP socket in `Init`,**
  through `n`, an `http3server.Network`, which lists one method, `ListenUDP`.
  `Init` passes its host. A UDP port that is taken fails `Init` with
  `EADDRINUSE`. The socket's close is registered with `s.OnStop`, and
  nothing else closes it: quic-go never closes a socket it is handed.
- **The handler chain is httpserver's,** the same code: `otelhttp`'s spans
  and metrics, the span named after the route, a panic recorded on the span
  and as one `handler panicked` record, and the failure scenario stripped
  unless `TrustScenario` trusts the caller. A panic resets the request's
  stream with `H3_INTERNAL_ERROR`, as HTTP/2 resets its stream.

The service's `main` is `http3server.Main(Init)`, or `httpserver.Main(Init)`:
`http3server.Defaults()` are httpserver's, a 5s drain and a 10s grace period,
so either package's `Main` runs both Services on the same defaults, and
[The defaults](/humus/pr-preview/pr-1641/services/http/serving/#the-defaults) holds for both.

## Settings

`http3server.Settings` follows the
[settings convention](/humus/pr-preview/pr-1641/fundamentals/configuration/#what-a-package-with-settings-exports),
as httpserver's does. Each field has the default, the range and the check of
httpserver's field of the same name, except where QUIC differs:

| Field | Default | Valid | Check | Over HTTP/3 |
|---|---|---|---|---|
| `Addr` | none: required | `host:port`, with a port from 0 to 65535 | `CheckAddr` | the UDP address, with the same rules |
| `TLS` | none: required | a config that has a certificate and allows TLS 1.3 | `CheckTLS` | QUIC requires TLS 1.3, so a `MaxVersion` below it is rejected |
| `ReadHeaderTimeout` | `2s` | not negative | `CheckReadHeaderTimeout` | bounds each request's headers, or `ReadTimeout` does when it is zero: a stream whose headers miss it is reset with `H3_REQUEST_INCOMPLETE`, and no handler runs |
| `ReadTimeout` | `5s` | not negative | `CheckReadTimeout` | bounds each request's stream from the end of its headers: once it has passed, a read of the body fails |
| `WriteTimeout` | `10s` | not negative | `CheckWriteTimeout` | bounds each request's stream from the end of its headers: when it passes, the request's context is cancelled, the stream's reads and writes fail, and the stream is reset with `H3_INTERNAL_ERROR` once the handler returns |
| `IdleTimeout` | `120s` | not negative | `CheckIdleTimeout` | closes a connection with no request in flight, or `ReadTimeout` does when it is zero. QUIC also closes a connection that is silent for 30s |
| `MaxHeaderBytes` | `1 << 20`, 1 MiB | positive | `CheckMaxHeaderBytes` | bounds a request's headers |
| `TrustScenario` | nil: trusts no caller | any | none | unchanged: the handler chain is httpserver's |

There is no `HTTP2Only`: HTTP/3 has no older protocol on its transport to
turn off.

- **The reset waits for the handler.** quic-go gives a handler no other way
  to reset its stream, so a handler that ignores its context and never
  writes holds its stream until it returns, where HTTP/2 resets it at the
  timer. A handler that streams moves either deadline with
  `http.ResponseController`'s `SetReadDeadline` and `SetWriteDeadline`, and
  one that takes its stream over with `http3.HTTPStreamer` lifts both.
- **The timeouts also bound the QUIC handshake,** at half the smallest of
  `ReadHeaderTimeout`, `ReadTimeout` and `WriteTimeout` that is set, or 5s
  when none is. quic-go aborts a handshake silent that long, or twice that
  long in all: 1s and 2s on the defaults.
- **QUIC's limits are code, not settings,** as HTTP/2's stream limit is in
  httpserver: a 30s idle timeout, 100 incoming streams and 100 incoming
  unidirectional streams, a 6 MiB stream and a 15 MiB connection receive
  window, no keep-alives and no datagrams. 0-RTT is never accepted. Each
  value is passed explicitly, so a quic-go bump changes none.
- **The 30s idle timeout closes a silent connection before `IdleTimeout`'s
  120s,** unless its client sends keep-alives. `http3client` sends one every
  10s.

## The drain

`Service.Shutdown` drains HTTP/3 itself, the way `net/http` drains HTTP/2:

- It sends each connection a GOAWAY, and resets with `H3_REQUEST_REJECTED`
  each stream its client opens after it. quic-go's client, `http3client`'s
  included, sends such a request again, once.
- It waits for the requests in flight, and closes each connection with
  `H3_NO_ERROR` one second after its last request finished, or one second
  after `Shutdown` began for a connection with none in flight. So an idle
  client that keeps its connection open doesn't hold `Shutdown` to the end of
  the grace period, and the second lets the last response finish.
- When the grace period runs out, it closes every connection at once, which
  cancels the context of each request still in flight, and returns without
  waiting for a handler that ignores its context, as httpserver's does.
- A connection opened after `Shutdown` began gets no answer, not a refusal:
  its client waits out its handshake timeout.

Until `Shutdown`, the Service keeps serving, through the App's
[drain](/humus/pr-preview/pr-1641/fundamentals/lifecycle/#the-drain), as httpserver's does. Both
Services drain in the same grace period.

## Alt-Svc

**humus has no API for Alt-Svc.** A client that speaks HTTP/3 by prior
knowledge, such as an [`http3client`](/humus/pr-preview/pr-1641/dependencies/http/#calling-a-dependency-over-http3)
client, needs none. A browser learns of HTTP/3 from an `Alt-Svc` header on
a response it got over TCP, or from a DNS HTTPS record with `alpn=h3`, which
is the operator's. An end user who serves browsers sets the header on the
responses `Init`'s handler writes, such as with a handler that wraps the mux
both Services serve. On an HTTP/3 response it is harmless, and refreshes
`ma`:

```
Alt-Svc: h3=":443"; ma=86400
```

- **The port is the one the browser reaches,** not the container's. Behind
  a load balancer it is the load balancer's UDP port, which is often 443
  while the pod binds 8443. An empty host before the port means the same
  host the browser already reached.
- **`ma` is how long, in seconds, the browser remembers it.** RFC 7838's
  default is 24 hours, 86400. A browser that remembers an entry tries
  HTTP/3, and falls back to TCP when it fails, so an entry that outlives the
  UDP path in front of the pod costs failed attempts until it expires. Keep
  `ma` no longer than such a change needs. quic-go's `SetQUICHeaders` writes
  30 days, and humus never calls it.

The right port and `ma` depend on what sits in front of the pod, which humus
can't see, so they are the end user's.

## On Kubernetes

**A pod serves both transports on one port number,** with one TCP and one
UDP entry. A Service's and a NetworkPolicy's ports default to TCP, so the
UDP entries are written out:

```yaml
# The Deployment's container.
ports:
  - name: https
    containerPort: 8443
    protocol: TCP
  - name: h3
    containerPort: 8443
    protocol: UDP
---
# The Service.
ports:
  - name: https
    port: 443
    targetPort: https
    protocol: TCP
  - name: h3
    port: 443
    targetPort: h3
    protocol: UDP
```

A `LoadBalancer` Service with both protocols on one port needs a cloud
provider that supports mixed protocols.

**Probes stay on TCP.** The kubelet probes `httpGet` over TCP, so a process
that serves both transports answers its probes on the `httpserver` Service's
port, as [Health probes](/humus/pr-preview/pr-1641/services/http/serving/#health-probes)
describes. A process that serves HTTP/3 alone returns
[`httphealth.NewService`](/humus/pr-preview/pr-1641/fundamentals/health/#exposing-the-probes)
for them, on a TCP port of its own.

The drain and the grace period are httpserver's, so
[`terminationGracePeriodSeconds`](/humus/pr-preview/pr-1641/services/http/serving/#terminationgraceperiodseconds)
holds as it is.

## The UDP-buffer warning

quic-go asks for a 7 MiB receive and send buffer on each UDP socket it
serves. When it can't get them, it writes a warning to the real standard
error, with the `log` package, once per process, such as:

```
2026/11/24 10:00:00 failed to sufficiently increase receive buffer size (was: 208 kiB, wanted: 7168 kiB, got: 416 kiB). See https://github.com/quic-go/quic-go/wiki/UDP-Buffer-Sizes for details.
```

**A pod prints it when the node's `net.core.rmem_max` or
`net.core.wmem_max` is below 3.5 MiB,** which Linux doubles to quic-go's 7
MiB, and the process lacks `CAP_NET_ADMIN`, with which quic-go forces the
size. Most nodes' defaults are below it, and a pod can't raise them: they
are the node's. The Service still serves, with the buffers it got, and a
busy server may drop packets its buffer has no room for. The fix is the
node's sysctls, or `CAP_NET_ADMIN` for the container.

The warning reaches neither the App's telemetry nor the `bedrock.Process`'s
standard error, so `p.Stderr()` in a test never shows it.
`QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING=true` turns it off, read from the
real environment, so an operator sets it on the process itself. Both are on
the lifecycle's
[ambient leaks](/humus/pr-preview/pr-1641/fundamentals/lifecycle/#ambient-leaks). humustest's
sockets set their buffers, so a test on a humustest Host writes nothing.

## Telemetry

The Service takes everything from the Setup's telemetry, as httpserver's
does:

- **Each request is one server span,** from `otelhttp`, with
  `network.protocol.version` `3.0`, named after the route, and
  OpenTelemetry's HTTP server metrics, such as
  `http.server.request.duration`. A query over both transports reads both
  versions.
- **The log records' scope is `github.com/z5labs/humus/http3/http3server`:**
  `listening`, at INFO, with `server.address`, when `Serve` starts;
  `handler panicked`, at ERROR; and each of quic-go's Error records as one
  ERROR record, such as `Malformed Content-Length` for a response whose
  handler set a `Content-Length` that isn't a number. quic-go's records
  below Error are dropped, and none reaches `slog.Default()`.

### What isn't visible

- **A QUIC handshake that fails is logged nowhere.** quic-go reports it only
  to a qlog tracer, which the Service has none of. Over TCP, httpserver logs
  every TLS handshake error, so a client with an untrusted certificate, or
  one that doesn't offer `h3`, shows over HTTP/2 and not over HTTP/3.
- **A connection dropped after `Shutdown` began** leaves no record: its
  client waits out its handshake timeout.
- **QUIC's own state,** such as loss, congestion and the buffers' sizes, is
  in no instrument and no record.

## Testing

An HTTP/3 service is tested whole, inside `testing/synctest`, as
[Testing a humus App](/humus/pr-preview/pr-1641/fundamentals/testing/) describes for any App, with
two functions from `http3servertest`:

- **`http3servertest.Start`** is `humustest.Start` with `http3server.Defaults()`
  applied before the test's options, and does nothing else.
- **`http3servertest.NewClient(t, h, roots)`** returns a client whose
  `Transport` is quic-go's, which dials through `h.DialUDP` and trusts only
  `roots`. Every request it makes is HTTP/3: there is no TCP to fall back
  to. It keeps its connections between requests, as an HTTP/3 client does,
  and closes them when the test ends.

`h.Listening` waits for TCP listeners only, so a test waits for the UDP
socket with `synctest.Wait()`. This test runs the `Init` above and asks once
over each transport:

```go
func TestInit_serves_HTTP3_beside_HTTP2(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t, hosttest.WithFiles(map[string][]byte{
			"tls.pem": pemFile(t, ca.Issue(t, "api.internal")),
		}))
		tel := humustest.NewTelemetry()
		p, status := http3servertest.Start(t, h.Host(), Init,
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"ADDR":         "api.internal:8443",
				"TLS_PEM_FILE": "tls.pem",
			}),
		)
		synctest.Wait()

		hello := func(c *http.Client) string {
			resp, err := c.Get("https://api.internal:8443/hello/humus")
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return resp.Proto + " " + string(body)
		}
		require.Equal(t, "HTTP/2.0 hello, humus", hello(httpservertest.NewClient(h, ca.Pool())))
		require.Equal(t, "HTTP/3.0 hello, humus", hello(http3servertest.NewClient(t, h, ca.Pool())))

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "%v", st.Cause())
		require.Empty(t, p.Stderr())

		var versions []string
		for _, span := range tel.Spans() {
			for _, a := range span.Attributes() {
				if a.Key == "network.protocol.version" {
					versions = append(versions, a.Value.AsString())
				}
			}
		}
		require.ElementsMatch(t, []string{"2.0", "3.0"}, versions)
	})
}
```

`pemFile` is a helper of the test file that writes the certificate's chain
and key as one PEM file. `NewClient` works without `Start`, so it works next
to an App started with `httpservertest.Start` or `humustest.Start` too.

Next: [Serving an API with OpenAPI](/humus/pr-preview/pr-1641/services/http/openapi/)
