---
title: Telemetry
linkTitle: Telemetry
description: >
  How a humus service gets its OpenTelemetry providers: the end user's
  Builder sources telemetry.Settings and calls telemetry.New, the App hands
  the SDK to Init, and humus never reads or sets OpenTelemetry's globals.
type: docs
weight: 17
---

A humus App builds its OpenTelemetry providers before `Init`, hands them to
`Init` as one value, logs through them, and shuts them down last. The
[`telemetry`](https://pkg.go.dev/github.com/z5labs/humus/telemetry) package
builds them from a plain `Settings` value. `humus.Main` builds them from
`telemetry.DefaultSettings()` unless the service passes a `Builder` of its
own, which sources that value the same way `Init` sources any other
package's [settings]({{< relref "configuration" >}}).

The design behind everything on this page is in
[Proposal: OpenTelemetry without globals, Revision 2](https://github.com/z5labs/humus/discussions/632#discussioncomment-18966249).
[The lifecycle]({{< relref "lifecycle#telemetry" >}}) describes when the App
builds its telemetry and shuts it down.

## Where the providers come from

A Service gets its providers from `s.Telemetry`, the
[`telemetry.SDK`](https://pkg.go.dev/github.com/z5labs/humus/telemetry#SDK) in
the `Setup` that `Init` is given. An SDK is a plain struct: a
`TracerProvider`, a `MeterProvider`, a `LoggerProvider`, a `Propagator`, and
the `Shutdown` that the App calls once it has stopped. Neither `Init` nor a
Service calls `Shutdown`.

humus never reads or sets OpenTelemetry's global providers or its global
propagator, and it never replaces `slog.Default`. Every humus package takes
an SDK, and third-party instrumentation is handed its providers explicitly:
otelhttp's options, grpc's `stats/opentelemetry` options, and
`otelslog.WithLoggerProvider`. This `Init` takes its Service's tracer and
logger from `s.Telemetry`:

```go
func CartsInit(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	tracer := s.Telemetry.TracerProvider.Tracer("carts")
	logger := otelslog.NewLogger("carts", otelslog.WithLoggerProvider(s.Telemetry.LoggerProvider))
	return []humus.Service{&cartJob{tracer: tracer, logger: logger}}, nil
}
```

A library that reads only the global providers can be given them: set them
in `Init`, from `s.Telemetry`. That is your decision and your global state,
not humus's. The one piece of global state humus does set is OpenTelemetry's
[error handler](#the-error-handler).

## Main's default

`App.Telemetry` is a
[`telemetry.Builder`](https://pkg.go.dev/github.com/z5labs/humus/telemetry#Builder).
`humus.Main` defaults it to one that calls `telemetry.New` with
`telemetry.DefaultSettings()`, the Host's `Dial` and the Process's stdout. So
a service that passes no `Builder`:

- writes its log records to standard output, as JSON lines in
  [the console log format](#the-console-log-format);
- gives its spans real trace and span IDs, which its log records carry, and
  exports none;
- builds no exporter that dials out, so it needs no collector, and a
  service that forgets its `Builder` doesn't fail.

[The defaults](#the-defaults) below describe what `DefaultSettings()` does in
full. `humustest.Start` keeps the same default, so a whole-App test that
passes no `Builder` sees on `p.Stdout()` the JSON lines production would
write. This example runs the App of this page that way:

```go
p, status := humustest.Start(t, CartsInit)
st := <-status

printConsoleLogs(p.Stdout(), "carts")
fmt.Println("exit code:", st.Code())
// Output:
// INFO "cart created" map[cart.id:c-123], trace context: true
// INFO "cart created, untraced" map[], trace context: false
// exit code: 0
```

**A service that exports anything passes its own `Builder`**, with
`humus.WithTelemetry`, and so does one that names itself, since
`DefaultSettings()` names every service `unknown_service`:

```go
humus.Main(CartsInit, humus.WithTelemetry(buildCartsTelemetry))
```

## Writing the Builder

The end user's `Builder` is given the context a signal cancels, the
`bedrock.Process` and the Host's `Dial`, and it does three things:

1. sources `telemetry.Settings` from `p.Environment` with
   [`config.Struct`](https://pkg.go.dev/github.com/z5labs/bedrock/config#Struct),
   starting from `telemetry.DefaultSettings()`;
2. chains each field's check after its default, so a bad default fails as a
   bad variable does, and the error names the variable;
3. calls `telemetry.New` with the `Settings`, `dial` and `p.Stdout`.

[Configuring a humus service]({{< relref "configuration#sourcing-settings-in-init" >}})
explains the pattern. This `Builder` keeps the OpenTelemetry specification's
variable names, but the names are yours to choose:

```go
func buildCartsTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	d := telemetry.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(p.Environment, name) }
	exporter := func(name string, def telemetry.Exporter) config.Value[telemetry.Exporter] {
		return env(name).
			Map(func(s string) telemetry.Exporter { return telemetry.Exporter(s) }).
			Default(def).
			Validate(telemetry.CheckExporter)
	}

	s, err := config.Struct(func(r *config.Resolver) telemetry.Settings {
		s := d
		s.Disabled = r.Get(env("OTEL_SDK_DISABLED").Parse(strconv.ParseBool).Default(d.Disabled))
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("carts").Validate(telemetry.CheckServiceName))
		s.Traces = r.Get(exporter("OTEL_TRACES_EXPORTER", d.Traces))
		s.Metrics = r.Get(exporter("OTEL_METRICS_EXPORTER", d.Metrics))
		s.Logs = r.Get(exporter("OTEL_LOGS_EXPORTER", d.Logs))
		return s
	}).Read(ctx)
	if err != nil {
		return telemetry.SDK{}, err // exit 78: every bad variable, one line each
	}
	return telemetry.New(ctx, s, dial, p.Stdout)
}
```

`s := d` starts from every default, so a field the `Builder` does not source,
such as `LogLevel` here, keeps its default. `DefaultSettings` cannot see the
process, so it names the service `unknown_service`. The `Builder` names it.

With `OTEL_TRACES_EXPORTER=zipkin`, this App never calls `Init`. It writes
the error to standard error, because it has no telemetry yet to log it
through, and exits 78:

```
build telemetry: config OTEL_TRACES_EXPORTER: "zipkin" is not one of none, console or otlp
```

`telemetry.New` runs every check too, so a `Settings` built in code is checked
as well. Its error names each bad field, such as
`telemetry: Settings.Traces: "zipkin" is not one of none, console or otlp`,
and an App whose `Builder` returns it exits 1, not 78: the bad value came from
code, not from a variable.

A `Builder` that needs something `Settings` does not cover, such as a metric
view or a Prometheus reader, builds the `SDK` value itself, `Shutdown`
included. `humustest`'s recorder is built that way.

## Settings

[`telemetry.Settings`](https://pkg.go.dev/github.com/z5labs/humus/telemetry#Settings)
follows the [shape every humus package's settings take]({{< relref "configuration#what-a-package-with-settings-exports" >}}):
plain values, `DefaultSettings()`, and a check for each field with a valid
range. A zero field means zero, never "use the default".

| Field | Default | Valid | Check | Spec variable |
|---|---|---|---|---|
| `Disabled` | `false` | any | | `OTEL_SDK_DISABLED` |
| `ServiceName` | `"unknown_service"` | not empty | `CheckServiceName` | `OTEL_SERVICE_NAME` |
| `ResourceOptions` | none, so no detector runs | any | | `OTEL_RESOURCE_ATTRIBUTES`, as `resource.WithAttributes` |
| `Traces` | `ExporterNone` | `none`, `console`, `otlp` | `CheckExporter` | `OTEL_TRACES_EXPORTER` |
| `Metrics` | `ExporterNone` | `none`, `console`, `otlp` | `CheckExporter` | `OTEL_METRICS_EXPORTER` |
| `Logs` | `ExporterConsole` | `none`, `console`, `otlp` | `CheckExporter` | `OTEL_LOGS_EXPORTER` |
| `OTLP.Endpoint` | `"localhost:4317"` | `host:port`, with a port from 1 to 65535 | `CheckOTLPEndpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT`, which is a URL in the spec |
| `OTLP.Protocol` | `ProtocolGRPC` | `grpc`, `http/protobuf` | `CheckOTLPProtocol` | `OTEL_EXPORTER_OTLP_PROTOCOL` |
| `OTLP.Headers` | none | any | | `OTEL_EXPORTER_OTLP_HEADERS` |
| `OTLP.TLS` | `nil`, which means plaintext | any | | `OTEL_EXPORTER_OTLP_CERTIFICATE` and the related variables |
| `OTLP.Timeout` | `10s` | positive | `CheckOTLPTimeout` | `OTEL_EXPORTER_OTLP_TIMEOUT` |
| `Sampler` | `sdktrace.ParentBased(sdktrace.AlwaysSample())` | not nil | `CheckSampler` | `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` |
| `LogLevel` | `log.SeverityInfo` | `log.SeverityTrace1` to `log.SeverityFatal4` | `CheckLogLevel` | none |
| `ShutdownTimeout` | `5s` | positive | `CheckShutdownTimeout` | none |

The godoc of each field is the reference; the table is its summary. The
`OTLP` fields are checked even when no signal is exported over OTLP. The
OTLP exporters dial the collector through the `Dialer` the `Builder` is
given, which is the Host's `Dial`, and over `http/protobuf` each signal is
sent to its spec path: `/v1/traces`, `/v1/metrics` and `/v1/logs`.

**humus reads none of these variables.** The last column names the
specification's variable with the same meaning, for a `Builder` that wants to
stay compatible with it, as the one above does. It is not a variable humus
reads: there is no `FromEnvironment` and no parser for the specification's
formats. So a platform that injects `OTEL_*` variables, such as the
[OpenTelemetry Operator](https://opentelemetry.io/docs/platforms/kubernetes/operator/),
has no effect on an App unless its `Builder` reads them.

`telemetry.New` gives the OpenTelemetry SDK every option explicitly, at the
SDK's own default where `Settings` has no field, so the SDK's own reads of the
real environment are overridden too. The few that still take effect, such as
`OTEL_GO_X_*`, are listed under
[the lifecycle's ambient leaks]({{< relref "lifecycle#ambient-leaks" >}}).

## The defaults

With `DefaultSettings()`:

- **Logs are written to standard output**, as one line of JSON per record, in
  [the console log format](#the-console-log-format).
- **Traces are not exported, but spans still get IDs.** At `ExporterNone`, the
  `TracerProvider` is still the SDK's, built with the `Sampler` and the
  resource but with no span processor. Every span gets a real trace ID and
  span ID, and a record logged in it carries them, but nothing is exported.
  That is the usual local setup: the logs on standard output can be grouped by
  request without running a collector.
- **Metrics are a no-op.**

Sampling is the same with or without an exporter: the `TracerProvider` keeps
the configured `Sampler`, so the sampled flag passed on to downstream services
does not change when `Traces` does.

Traces at `none` have a cost **in the middle of a shared trace**. A service at
`none` in a traced call chain passes on span IDs that no backend ever
receives. The trace ID is still the caller's, so the trace holds together, but
the downstream spans point to a parent that is missing. **A service that takes
part in a traced system sets `Traces` to `otlp`.**

## The console log format

At `ExporterConsole`, each log record is written to standard output as one
line of JSON, terminated by a newline:

```json
{"timestamp":"2026-10-16T06:56:32.447829601Z","severity_text":"INFO","severity_number":9,"body":"cart created","trace_id":"e2f668b6c06f2de82e61e2517c2d7a07","span_id":"ff25eb972a5557d5","trace_flags":"01","scope":"carts","attributes":{"cart.id":"c-123"}}
```

**This format is part of humus's API.** Renaming, reordering, adding or
removing a key, or encoding a value differently, is a breaking change. It is
humus's own, written by an exporter of the `telemetry` package, so upgrading
OpenTelemetry does not change it. A golden test pins it byte for byte.

The keys are always written in this order, and each one only when the record
has what it holds:

| Key | Value | Present |
|---|---|---|
| `timestamp` | the record's timestamp or, when that is unset, its observed timestamp, in UTC, as RFC 3339 with exactly nine fractional digits | always |
| `severity_text` | the record's severity text or, when that is empty, the short name of its severity number: `TRACE`, `TRACE2`, ..., `INFO`, ..., `FATAL4` | when the severity number is set, that is when it is one of OpenTelemetry's severities 1 to 24 |
| `severity_number` | the record's OpenTelemetry severity number, 1 to 24, for filtering by threshold | under the same condition as `severity_text` |
| `event_name` | the record's event name | when it is not empty |
| `body` | the record's body, encoded as below | when the body is set |
| `trace_id`, `span_id`, `trace_flags` | the trace context of the span the record was logged in, as lowercase hex: 32, 16 and 2 digits | only when the record has a valid span context, one whose trace ID and span ID are both non-zero |
| `scope` | the name of the record's instrumentation scope, the name its logger was created with | always, and empty when that name is |
| `attributes` | an object whose keys are the attribute keys, in the record's order | when the record has at least one attribute |

The keys are the OpenTelemetry log data model's field names, in snake case.
The trace context keys follow OpenTelemetry's stable specification for
[trace context in non-OTLP log formats](https://opentelemetry.io/docs/specs/otel/compatibility/logging_trace_context/),
so a log pipeline links a line to its trace by the names the specification
gives. A record logged outside a span has none of the three, rather than
zeros, because a zero ID would point to a trace that does not exist.

The record's resource is never written: OTLP carries the resource, and on
standard output whatever collects the logs, such as a Kubernetes log agent,
adds its own metadata. Neither is its count of dropped attributes.

A body and an attribute value are encoded the same way:

- a string as a JSON string, a bool as `true` or `false`, and an integer as a
  JSON number;
- a finite float as a JSON number, and NaN and the infinities as the strings
  `"NaN"`, `"+Inf"` and `"-Inf"`, so that every line is valid JSON;
- bytes as a string holding their standard, padded base64 encoding;
- a slice, typed slices included, as an array, and a map as an object with
  its keys sorted by their bytes, each element encoded by these rules;
- an empty value as `null`, as an attribute's value or inside a slice or map.
  An empty body is not written at all.

An attribute key a record holds more than once, which happens only when its
logger provider allows duplicate keys, is written once for each value, and so
is a key a map holds more than once. Strings are escaped as JSON requires and
no further: `<`, `>` and `&` are written as they are. Invalid UTF-8 is
replaced by U+FFFD.

The [`telemetry` package doc](https://pkg.go.dev/github.com/z5labs/humus/telemetry#hdr-Console_log_format)
is the specification of the format, and this section its summary.

Traces and metrics at `ExporterConsole` are written by OpenTelemetry's
`stdouttrace` and `stdoutmetric`. They are for debugging, and humus makes no
promise about their format.

## Trace context in logs

`telemetry.SDK` exposes a `LoggerProvider`, not a logger: humus has no logging
bridge of its own. Use the OpenTelemetry Logs API directly, or bridge the
`LoggerProvider` to the logging library you already use, such as
[otelslog](https://pkg.go.dev/go.opentelemetry.io/contrib/bridges/otelslog)
for `log/slog`, as `CartsInit` [above](#where-the-providers-come-from) does.

A record carries a trace context only when the log call is given the span's
context. With otelslog, that means `InfoContext`, not `Info`: `Info` logs with
`context.Background()`, so its record has no trace fields, whatever exporter
it goes to. In a handler, pass the request's context, `r.Context()`. This
Service logs both ways inside one span:

```go
func (j *cartJob) Serve(ctx context.Context) error {
	ctx, span := j.tracer.Start(ctx, "create cart")
	defer span.End()

	// InfoContext, not Info: the record takes its trace context from ctx,
	// so it carries the span's trace_id and span_id.
	j.logger.InfoContext(ctx, "cart created", slog.String("cart.id", "c-123"))

	// Info logs with context.Background(), so this record has no trace
	// context, even though it is logged inside the span.
	j.logger.Info("cart created, untraced")
	return nil
}
```

With the defaults, the first line on standard output has `trace_id`,
`span_id` and `trace_flags`, and the second has none of them.

## Other settings

- **`LogLevel`** is the lowest severity the `LoggerProvider` exports, as an
  OpenTelemetry severity rather than a `slog.Level`. It is applied in the
  provider: its loggers' `Enabled` reports false below it, so a bridge that
  asks `Enabled` before building a record, as otelslog's handler does, skips a
  filtered record at no cost, and a record below it is dropped either way. A
  record with no severity, `log.SeverityUndefined`, is not on the scale and is
  kept.
- **`ShutdownTimeout`** bounds `SDK.Shutdown`: every provider is flushed and
  stopped within it. The App shuts telemetry down after the OnStop functions,
  separately from the grace period and with a context that a cancelled root
  context does not cancel, so `ShutdownTimeout` is what bounds it.
- **`Disabled`** is the counterpart of `OTEL_SDK_DISABLED`. `telemetry.New`
  ignores every other field and returns `telemetry.Noop()`, building and
  dialling nothing. The propagator stays W3C trace context plus baggage,
  because the specification disables signals, not propagation, so a request's
  trace passes through. Unlike traces at `none`, a disabled SDK makes no IDs
  of its own: it passes on the span context a request brought.
- **Resource detectors are opt-in.** The resource is `ServiceName` plus
  whatever `ResourceOptions` adds, and by default no detector runs, so the
  resource is `service.name` alone. A detector, such as `resource.WithHost()`
  or a contrib cloud detector, reads the real host rather than the
  `humus.Host`: the host, process and container detectors read `/etc`, `/proc`
  and cgroups, and a cloud detector dials its metadata server. Opt in
  knowingly. `ServiceName` wins over a `service.name` any detector finds.
  `telemetry.New` fails if any option fails, even partly, and some detectors
  fail where their attribute cannot be found: `resource.WithProcess` includes
  the process owner, which cannot be looked up in an image without a passwd
  entry for its user. Opt in to the detectors whose attributes the
  environment has, such as `resource.WithProcessPID`, rather than to a group
  of them.

## Testing

A test that asserts on what an App emits runs it with `humustest.Start`, and
passes
[`humustest.NewTelemetry()`](https://pkg.go.dev/github.com/z5labs/humus/humustest#Telemetry)'s
`Builder` with `humustest.WithApp(humus.WithTelemetry(…))`, in place of the
one production uses. The recorder uses the real
OpenTelemetry SDK with in-memory exporters, records every severity, samples
every span, starts no goroutine, so it works under `synctest`, and keeps what
it recorded after the App has shut it down:

```go
tel := humustest.NewTelemetry()
_, status := humustest.Start(t, CartsInit,
	humustest.WithApp(humus.WithTelemetry(tel.Builder())),
)
st := <-status
```

Once the App has exited, `tel.Spans()`, `tel.Metrics(ctx)` and `tel.Logs()`
return what it emitted. Here they show the span `create cart`, and that the
record logged with `InfoContext` is in it:

```go
span := tel.Spans()[0]
fmt.Println("span:", span.Name())
for _, r := range tel.Logs() {
	if r.InstrumentationScope().Name != "carts" {
		continue // the lifecycle's own records
	}
	inSpan := r.TraceID() == span.SpanContext().TraceID() && r.SpanID() == span.SpanContext().SpanID()
	fmt.Printf("logged %s: %s, in the span: %t\n", r.SeverityText(), r.Body().AsString(), inSpan)
}
```

The lifecycle's own records, `serving`, `shutting down`, `stopped` and every
failure after the telemetry is built, are in `tel.Logs()` too, under the scope
`github.com/z5labs/humus`.

A test that does not assert on telemetry keeps `Main`'s
[default](#mains-default), or passes `telemetry.NoopBuilder`, whose SDK
discards everything. A test of the `Builder` itself passes it with
`humustest.WithApp(humus.WithTelemetry(…))`, sets the environment it reads
with `humustest.WithEnv`, and reads standard output, where the console logs
go, and standard error, where a bad variable goes. The root package's
[examples](https://pkg.go.dev/github.com/z5labs/humus#pkg-examples) run the
`Builder` above that way.

[Testing a humus App]({{< relref "testing" >}}) covers the rest of a whole-App
test.

## The error handler

Telemetry cannot report its own failure: a failing OTLP log exporter would
swallow the errors about itself, and each error would queue another record
bound to fail. So once the SDK is built, `Run` sets OpenTelemetry's
process-wide error handler to one that writes each export error to the
Process's standard error, as one line starting with `telemetry: `. Export
errors join the failures standard error already gets, those that come before
there is any telemetry or that are about telemetry itself: a failure to build
or shut it down, and a malformed lifecycle setting.

This is the one piece of OpenTelemetry's global state humus sets, and its one
call to `otel.SetErrorHandler`. Being process-wide, it has two consequences:

- In a process that runs several Apps, the last App to start gets every App's
  export errors. So a test that asserts on export errors does not run in
  parallel with another App.
- An end user who wants a different handler sets it in `Init`, which runs
  after `Run` has set its own.

OpenTelemetry's internal logger is separate from the handler, and writes its
own errors to the real standard error. It is on
[the lifecycle's list of ambient leaks]({{< relref "lifecycle#ambient-leaks" >}}).
