// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/config"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The telemetry page of the docs site quotes the code in this file, and
// TestDocs_code_samples checks that every quote still matches it.

// buildCartsTelemetry is the telemetry.Builder of the App in
// Example_telemetry. It keeps the OpenTelemetry specification's variable
// names, mapping each one onto a field of telemetry.Settings: humus reads
// none of them itself.
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

// CartsInit is the Init of the App in Example_telemetry. It takes the
// Service's tracer and logger from s.Telemetry, never from OpenTelemetry's
// globals, and bridges the LoggerProvider to slog with otelslog.
func CartsInit(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	tracer := s.Telemetry.TracerProvider.Tracer("carts")
	logger := otelslog.NewLogger("carts", otelslog.WithLoggerProvider(s.Telemetry.LoggerProvider))
	return []humus.Service{&cartJob{tracer: tracer, logger: logger}}, nil
}

// cartJob is the Service of the App in Example_telemetry. It records one
// span, logs in it, and returns, as a job does.
type cartJob struct {
	tracer trace.Tracer
	logger *slog.Logger
}

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

func (j *cartJob) Shutdown(ctx context.Context) error {
	return nil
}

// runCarts runs the App of Example_telemetry on a bedrocktest Process with
// env and a humustest Host, and prints the records the carts scope logged
// to stdout, what the App wrote to stderr and its exit code.
func runCarts(env map[string]string) {
	t := new(exampleTB)
	defer t.runCleanups()

	h := humustest.NewHost(t)
	app := humus.App{
		Host:                h.Host(),
		ShutdownGracePeriod: 10 * time.Second,
		Telemetry:           buildCartsTelemetry,
		Init:                CartsInit,
	}
	fp := bedrocktest.NewProcess(t, bedrocktest.WithEnv(env))
	status := app.Run(context.Background(), fp.Process())

	printConsoleLogs(fp.Stdout(), "carts")
	fmt.Print(fp.Stderr())
	fmt.Println("exit code:", status.Code())
}

// printConsoleLogs prints the records of the given scope in out, which holds
// the console log format, one record per line. Their timestamps and IDs
// differ on every run, so it prints only whether each one has a trace
// context.
func printConsoleLogs(out, scope string) {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var rec struct {
			Severity   string            `json:"severity_text"`
			Body       string            `json:"body"`
			TraceID    string            `json:"trace_id"`
			Scope      string            `json:"scope"`
			Attributes map[string]string `json:"attributes"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			fmt.Println("not a console log line:", err)
			continue
		}
		if rec.Scope != scope {
			continue
		}
		fmt.Printf("%s %q %v, trace context: %t\n", rec.Severity, rec.Body, rec.Attributes, rec.TraceID != "")
	}
}

// An App's telemetry.Builder sources telemetry.Settings as Init sources any
// other package's settings, here from the OpenTelemetry specification's own
// variables. Each value starts from DefaultSettings and is checked after it,
// so a malformed variable exits 78 before Init is called, with an error
// naming it.
func Example_telemetry() {
	// A malformed exporter, named in the cause, on stderr: the App has no
	// telemetry yet to log it through.
	runCarts(map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"})

	// Nothing set: every field takes its default, so log records are
	// written to stdout and spans get real IDs without being exported.
	runCarts(nil)

	// OTEL_SDK_DISABLED: no telemetry at all, so stdout is empty.
	runCarts(map[string]string{"OTEL_SDK_DISABLED": "true"})
	// Output:
	// build telemetry: config OTEL_TRACES_EXPORTER: "zipkin" is not one of none, console or otlp
	// exit code: 78
	// INFO "cart created" map[cart.id:c-123], trace context: true
	// INFO "cart created, untraced" map[], trace context: false
	// exit code: 0
	// exit code: 0
}

// A test that asserts on an App's telemetry runs it with humustest's
// recorder as its Builder, and reads what the App emitted from it.
func Example_telemetryRecorder() {
	t := new(exampleTB)
	defer t.runCleanups()

	h := humustest.NewHost(t)
	tel := humustest.NewTelemetry()
	app := humus.App{
		Host:                h.Host(),
		ShutdownGracePeriod: 10 * time.Second,
		Telemetry:           tel.Builder(),
		Init:                CartsInit,
	}
	fp := bedrocktest.NewProcess(t)
	status := app.Run(context.Background(), fp.Process())

	span := tel.Spans()[0]
	fmt.Println("span:", span.Name())
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name != "carts" {
			continue // the lifecycle's own records
		}
		inSpan := r.TraceID() == span.SpanContext().TraceID() && r.SpanID() == span.SpanContext().SpanID()
		fmt.Printf("logged %s: %s, in the span: %t\n", r.SeverityText(), r.Body().AsString(), inSpan)
	}
	fmt.Println("exit code:", status.Code())
	// Output:
	// span: create cart
	// logged INFO: cart created, in the span: true
	// logged INFO: cart created, untraced, in the span: false
	// exit code: 0
}
