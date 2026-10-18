---
title: Configuring a humus service
linkTitle: Configuration
description: >
  How a humus service's settings get their values: humus packages take plain
  Go values, and the service's Init sources each one, usually with bedrock's
  config package, so that a bad value fails startup with exit 78 and an error
  naming the variable.
type: docs
weight: 20
---

humus packages are configured with plain Go values. They read no environment
variables for their settings, define no variable names and read no files on
your behalf; the few variables humus does read are
[listed below](#the-variables-humus-reads). Every
value is sourced in your own `Init`, with whatever names and formats your
service already uses: an environment variable called `READ_TIMEOUT` or
`MYSVC_HTTP_READ_TIMEOUT`, a duration written `5s`, a file of PEM, a flag. The
usual tool is bedrock's
[`config`](https://pkg.go.dev/github.com/z5labs/bedrock/config) package, and
this page uses it throughout.

A framework that defined variable names would have to teach them, keep them
consistent and document them twice. With Go values, godoc is the only
documentation of a package's settings, and a service's variables are whatever
its team chooses.

The design behind everything on this page is in
[the configuration revision](https://github.com/z5labs/humus/discussions/631#discussioncomment-18965681)
of [Proposal: configuration for humus apps](https://github.com/z5labs/humus/discussions/631).

## What a package with settings exports

Every humus package with settings exports the same three things:

- a `Settings` struct, whose fields are plain values;
- `DefaultSettings()`, which returns the package's defaults;
- a check function, `Check<Field>`, for each field that has a valid range.

A zero field means zero, never "use the default". A package's defaults are
the values `DefaultSettings` returns, and nothing else: a field left at zero
is checked, and used, as zero. This keeps zero meaningful, as it is for
`net/http`'s timeouts, where a zero `ReadTimeout` means no timeout.

The godoc of each field gives its default, its valid range and its meaning,
and names its check. A check is a `func(T) error`. Its error says what is
wrong, such as `must not be negative`, and does not name the field: whatever
reads the value adds the name.

The samples on this page use a stand-in, declared in the root package's
tests in the shape every package gives it. The
[`telemetry`](https://pkg.go.dev/github.com/z5labs/humus/telemetry) package's
`Settings` has the same shape, and [Telemetry]({{< relref "telemetry#writing-the-builder" >}})
sources it with the same pattern, in the App's telemetry `Builder` rather than
in `Init`:

```go
// Settings configures a Greeter. Every field is a plain value, and a field's
// zero value means zero, never "use the default".
type Settings struct {
	// Greeting is what the Greeter says. Default "hello".
	Greeting string

	// ReadTimeout bounds reading a whole request. Default 5s. Zero means
	// no timeout. Must not be negative; checked by CheckReadTimeout.
	ReadTimeout time.Duration

	// MaxConns is how many connections the Greeter serves at once.
	// Default 100. Must be positive; checked by CheckMaxConns.
	MaxConns int
}

// DefaultSettings returns the defaults of a Greeter.
func DefaultSettings() Settings {
	return Settings{
		Greeting:    "hello",
		ReadTimeout: 5 * time.Second,
		MaxConns:    100,
	}
}

// CheckReadTimeout reports why d is not a valid ReadTimeout, or nil if it
// is.
func CheckReadTimeout(d time.Duration) error {
	if d < 0 {
		return errors.New("must not be negative")
	}
	return nil
}

// CheckMaxConns reports why n is not a valid MaxConns, or nil if it is.
func CheckMaxConns(n int) error {
	if n <= 0 {
		return errors.New("must be positive")
	}
	return nil
}
```

In a real service these come from the package, as `httpserver.Settings`,
`httpserver.DefaultSettings()` and `httpserver.CheckReadTimeout`, and you
never copy a range out of godoc. [Serving HTTP]({{< relref "http#settings" >}})
sources `httpserver.Settings` this way.

## Sourcing Settings in Init

`Init` reads the package's `Settings` with
[`config.Struct`](https://pkg.go.dev/github.com/z5labs/bedrock/config#Struct).
Each field is one `config.Value` chain: where the value comes from, how it is
parsed, `.Default` with the field of `DefaultSettings()`, and then the
package's check, chained with
[`.Validate`](https://pkg.go.dev/github.com/z5labs/bedrock/config#Value.Validate):

```go
func GreeterInit(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) Settings {
		return Settings{
			// The file is read as part of the setting, so a missing file
			// is a config error named after the variable.
			Greeting:    r.Get(env("GREETING_FILE").Parse(readFile(s.Host.FS)).Default(d.Greeting)),
			ReadTimeout: r.Get(env("READ_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadTimeout).Validate(CheckReadTimeout)),
			MaxConns:    r.Get(env("MAX_CONNS").Parse(strconv.Atoi).Default(d.MaxConns).Validate(CheckMaxConns)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78: every bad variable, one line each
	}

	g, err := NewGreeter(settings, s.Process.Stdout)
	if err != nil {
		return nil, err
	}
	return []humus.Service{g}, nil
}
```

The check goes **after** `.Default`, and the order matters for two reasons:

- **The default is checked too.** A value that is not set is never checked, so
  a check chained before `.Default` would let a bad default through. Chained
  after it, a bad default fails exactly as a bad variable does.
- **The error names the variable.** A check in a field's chain fails under
  that value's name, so the error says `READ_TIMEOUT`, which is what an
  operator sets. A check on the whole struct could only name the field,
  `ReadTimeout`, which no operator sets.

`config.Struct` attempts every `Get`, even after one has failed, so a
misconfigured service reports all of its problems in one error rather than one
per restart. Read every value through the `Process` and the `Host` in `Setup`,
never through package `os`, so that the whole App runs in a test.

## Reading a file

A setting that names a file reads the file inside the field's chain. Here
`GREETING_FILE` is parsed by `readFile`, which opens the file through
`s.Host.FS`:

```go
// readFile returns a parser for config.Value.Parse that reads the whole
// file a variable names, through fsys.
func readFile(fsys humus.FS) func(name string) (string, error) {
	return func(name string) (string, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(f)
		return string(b), err
	}
}
```

Because the file is read as part of the setting, a missing or unreadable file
is a config error named after its variable, and it exits 78 with the rest.
`humus.FS` has no `ReadFile`: a helper this small is your code, not humus's
API, and it stays unexported. The same goes for decoding: a package takes a
decoded value, such as a `*tls.Config`, never a path or a file's contents, so
how a certificate is stored and encoded is yours to decide. A further
`.Parse` in the chain does the decoding.

## How a bad value fails

A bad value renders as `config <variable>: <cause>`. With
`READ_TIMEOUT=-5s` and `MAX_CONNS=lots`, the App above logs both at ERROR
through its telemetry, in one record of two lines, and exits 78:

```
init: config READ_TIMEOUT: must not be negative
config MAX_CONNS: strconv.Atoi: parsing "lots": invalid syntax
```

The lifecycle wraps the error `Init` returns once, as `init: <error>`, and
logs it as it renders. `config.Struct` joins its failures with
`errors.Join`, which renders one line per failure, so only the first line
carries the `init: ` prefix. Every line that names a bad variable starts
`config `, prefix or not, and that is the word to search for.

And with `GREETING_FILE` naming a file that does not exist:

```
init: config GREETING_FILE: openat etc/greeter/greeting.txt: no such file or directory
```

The cause is whatever the Host's `FS` returned. This output is from the
example, on a humustest Host, whose `FS` opens files through an `os.Root`,
hence `openat`. On `humus.OSHost()` the same file fails with
`open <path>: no such file or directory`.

Every error a `config.Value` returns is a
[`*config.Error`](https://pkg.go.dev/github.com/z5labs/bedrock/config#Error).
When one is in the chain of the error `Init` returns, the App exits 78
(`humus.ExitConfig`, `EX_CONFIG` from `sysexits.h`), which tells an operator
"fix the configuration" apart from "the service failed".
`errors.AsType[*config.Error]` finds the first bad variable in the chain, which
is `READ_TIMEOUT` in the run above. A test that checks every bad variable
walks the joined error, whose `Unwrap() []error` returns one `*config.Error`
per variable, or compares the whole of the record the App logged. The
[lifecycle's exit codes]({{< relref "lifecycle#exit-codes" >}}) list the rest.

## Building Settings in code

`config` is a convenience, not a requirement. Settings can come from flags,
a YAML file, or plain code:

```go
settings := DefaultSettings()
settings.ReadTimeout = -time.Second
settings.MaxConns = 0

_, err := NewGreeter(settings, io.Discard)
fmt.Println(err)
```

A `Settings` built this way never passes through a check in a `config` chain,
so the package's constructor runs every check itself. It reports every bad
field in one error, naming each field rather than a variable:

```
ReadTimeout: must not be negative
MaxConns: must be positive
```

The constructor of the stand-in shows the shape:

```go
func NewGreeter(s Settings, out io.Writer) (*Greeter, error) {
	var errs []error
	if err := CheckReadTimeout(s.ReadTimeout); err != nil {
		errs = append(errs, fmt.Errorf("ReadTimeout: %w", err))
	}
	if err := CheckMaxConns(s.MaxConns); err != nil {
		errs = append(errs, fmt.Errorf("MaxConns: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Greeter{settings: s, out: out}, nil
}
```

Its error is not a `*config.Error`, so an `Init` that returns it exits 1. A
service that sources its settings with `config` and chains each check never
reaches it: the bad value has already exited 78, named after its variable.

## The variables humus reads

humus reads the environment in two places, and neither is a setting of a
package:

- **The lifecycle reads its two variables**, `HUMUS_SHUTDOWN_GRACE_PERIOD`
  and `HUMUS_SHUTDOWN_DRAIN_DELAY`. `humus.App.Run` reads them before it
  calls `Init`, with defaults from `App`'s own fields, and checks each after
  its default, as on this page:
  - `HUMUS_SHUTDOWN_GRACE_PERIOD` defaults to the App's `ShutdownGracePeriod`
    field: `humus.Main`'s 20s, or whatever an option, such as a runtime
    package's defaults or `humus.WithShutdownGracePeriod`, or an App literal
    set it to.
  - `HUMUS_SHUTDOWN_DRAIN_DELAY`, when it is set, replaces the App's `Drain`,
    whatever that is, with `humus.DrainDelay` of its value, and adds a drain
    to an App that has none. When it is not set, the App's `Drain` is the
    drain.

  [The lifecycle's settings]({{< relref "lifecycle#settings" >}}) describes
  them.
- **`job` reads the variables its propagator's keys name.** A job's run
  joins the trace of the process that started it: `job.Service` extracts a
  trace context from the Process's environment with the telemetry's
  propagator, reading each of the propagator's keys under the name
  OpenTelemetry's environment-variable carriers give it. With
  `telemetry.New`'s propagator, those are `TRACEPARENT`, `TRACESTATE` and
  `BAGGAGE`; a `Builder` with a propagator of another kind, such as B3,
  changes them. When none is set, or the value is malformed, the run starts
  a new trace and nothing fails.
  [Jobs]({{< relref "jobs#joining-the-trace-that-started-the-job" >}})
  describes them.

Apart from the lifecycle's two, humus still defines no variable names. The
names `job` reads are OpenTelemetry's: its specification names them, from
the propagator's keys, so a process that starts a job sets the same
variables whatever language the job is written in. The `telemetry` package
reads no `OTEL_*` variable either: a `Builder` that wants them maps them onto
`telemetry.Settings` itself, as [Telemetry]({{< relref "telemetry#settings" >}})
shows.

## Logging and secrets

humus logs no settings, and it does not redact any. Whether to log the values
a service started with, which ones, and how to hide a secret among them are
your decisions. `Init` is given the same `Process` and `Host` that humus has,
so it can log anything it wants, environment variables included, to
`s.Process.Stderr` or to a logger of its own.

What humus does log is the error `Init` returns, through the App's telemetry,
and a parse error often quotes the value it could not parse: `parsing "lots"`
above. A malformed secret, such as a token in the wrong format or a key that
does not decode, would be logged the same way. Keeping a secret out of that error is
part of the same choice: give the secret's chain a parser of your own whose
error says what is wrong without quoting the input.

## Testing it

Settings are tested by running the whole App with
[`humustest.Start`](https://pkg.go.dev/github.com/z5labs/humus/humustest#Start):
`humustest.WithEnv` sets the variables, and a
[humustest](https://pkg.go.dev/github.com/z5labs/humus/humustest) Host holds the
files the settings name. One of the root package's
[examples](https://pkg.go.dev/github.com/z5labs/humus#pkg-examples)
runs the App above this way:

```go
h := humustest.NewHost(t, humustest.WithFiles(files))
tel := humustest.NewTelemetry()
p, status := humustest.Start(t, GreeterInit,
	humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
	humustest.WithEnv(env),
)
st := <-status
```

`humus.WithHost` replaces the Host `Start` builds with one the example made
with its files, and `humus.WithTelemetry` passes `humustest.Telemetry`'s
`Builder`, so the example prints the errors the App logged from the
recorder's `Logs()`. Both are humus's own options, passed with
`humustest.WithApp` as `main` passes them to `humus.Main`.

The App runs with `Main`'s defaults, so its grace period is `Main`'s 20s, as
in production, and a `HUMUS_SHUTDOWN_GRACE_PERIOD` or
`HUMUS_SHUTDOWN_DRAIN_DELAY` the test passes in `WithEnv` overrides it, as an
operator's would. `WithEnv` given twice keeps only the last map, so a test
that adds to a base environment merges the maps itself, as
[Testing a humus App]({{< relref "testing#a-base-environment" >}}) shows.

A relative name given to the humustest Host's `FS` is relative to the test's
temp directory, and an absolute one, from `h.Path`, must be inside it, so a
setting naming a file never reaches the real disk.
[Testing a humus App]({{< relref "testing" >}}) covers the Host in full.
