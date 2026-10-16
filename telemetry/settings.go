// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Exporter names where a signal is sent.
type Exporter string

const (
	// ExporterNone exports nothing. For traces, the TracerProvider is still
	// the SDK's, so spans get real IDs; for metrics and logs, the provider
	// is a no-op.
	ExporterNone Exporter = "none"

	// ExporterConsole writes the signal to the stdout given to New.
	ExporterConsole Exporter = "console"

	// ExporterOTLP sends the signal over OTLP, as configured by
	// Settings.OTLP.
	ExporterOTLP Exporter = "otlp"
)

// Protocol names the transport of the OTLP exporters.
type Protocol string

const (
	// ProtocolGRPC is OTLP over gRPC.
	ProtocolGRPC Protocol = "grpc"

	// ProtocolHTTPProtobuf is OTLP over HTTP, with protobuf payloads.
	ProtocolHTTPProtobuf Protocol = "http/protobuf"
)

// Settings configures New. Every field is a plain value, and a field's zero
// value means zero, never "use the default": start from DefaultSettings and
// change the fields you source.
//
// humus reads no environment variables for telemetry. Where a field has the
// same meaning as a variable of the OpenTelemetry specification, its doc
// names the variable, for a Builder that wants to stay compatible with it.
type Settings struct {
	// Disabled turns telemetry off: New ignores every other field and
	// returns Noop().
	//
	// Default: false. Valid: any. Spec variable: OTEL_SDK_DISABLED.
	Disabled bool

	// ServiceName is the service.name of the resource. It wins over a
	// service.name that any of ResourceOptions sets.
	//
	// Default: "unknown_service". Valid: not empty. Check: CheckServiceName.
	// Spec variable: OTEL_SERVICE_NAME.
	ServiceName string

	// ResourceOptions build the resource, before ServiceName is applied.
	// With none, no resource detector runs, so the resource is service.name
	// alone. A detector reads the real host, not the humus.Host: opt in to
	// one, such as resource.WithHost(), knowingly.
	//
	// Default: none. Valid: any. Spec variable: OTEL_RESOURCE_ATTRIBUTES,
	// as resource.WithAttributes.
	ResourceOptions []resource.Option

	// Traces is where spans are exported. At ExporterNone, the
	// TracerProvider is still the SDK's, built with Sampler and the
	// resource but no span processor, so spans get real IDs and logs carry
	// them, but nothing is exported.
	//
	// Default: ExporterNone. Valid: none, console, otlp. Check:
	// CheckExporter. Spec variable: OTEL_TRACES_EXPORTER.
	Traces Exporter

	// Metrics is where metrics are exported. At ExporterNone, the
	// MeterProvider is a no-op.
	//
	// Default: ExporterNone. Valid: none, console, otlp. Check:
	// CheckExporter. Spec variable: OTEL_METRICS_EXPORTER.
	Metrics Exporter

	// Logs is where log records are exported. At ExporterConsole, each
	// record is written to stdout as one line of JSON, in the format the
	// package doc specifies. At ExporterNone, the LoggerProvider is a
	// no-op.
	//
	// Default: ExporterConsole. Valid: none, console, otlp. Check:
	// CheckExporter. Spec variable: OTEL_LOGS_EXPORTER.
	Logs Exporter

	// OTLP configures the exporters of the signals set to ExporterOTLP. It
	// is checked even when no signal uses it.
	OTLP OTLPSettings

	// Sampler decides which spans are sampled. It is used whatever Traces
	// is, so the sampled flag passed on to other services is the same with
	// or without an exporter.
	//
	// Default: sdktrace.ParentBased(sdktrace.AlwaysSample()). Valid: not
	// nil. Check: CheckSampler. Spec variables: OTEL_TRACES_SAMPLER and
	// OTEL_TRACES_SAMPLER_ARG.
	Sampler sdktrace.Sampler

	// LogLevel is the lowest severity the LoggerProvider exports. Its
	// loggers' Enabled reports false below it, and a record below it is
	// dropped. A record with no severity, log.SeverityUndefined, is not on
	// the scale and is kept, as the OpenTelemetry specification's minimum
	// severity requires.
	//
	// Default: log.SeverityInfo. Valid: log.SeverityTrace1 to
	// log.SeverityFatal4. Check: CheckLogLevel. Spec variable: none.
	LogLevel log.Severity

	// ShutdownTimeout bounds SDK.Shutdown: every provider is flushed and
	// stopped within it.
	//
	// Default: 5s. Valid: positive. Check: CheckShutdownTimeout. Spec
	// variable: none.
	ShutdownTimeout time.Duration
}

// OTLPSettings configures the OTLP exporters.
type OTLPSettings struct {
	// Endpoint is the collector's address. It is dialled through the
	// Dialer given to New, and for http/protobuf each signal is sent to its
	// spec path: /v1/traces, /v1/metrics and /v1/logs.
	//
	// Default: "localhost:4317". Valid: host:port. Check:
	// CheckOTLPEndpoint. Spec variable: OTEL_EXPORTER_OTLP_ENDPOINT, which
	// is a URL in the spec rather than a host and port.
	Endpoint string

	// Protocol is the OTLP transport.
	//
	// Default: ProtocolGRPC. Valid: grpc, http/protobuf. Check:
	// CheckOTLPProtocol. Spec variable: OTEL_EXPORTER_OTLP_PROTOCOL.
	Protocol Protocol

	// Headers are sent with every export: as gRPC metadata over gRPC, and
	// as HTTP headers over http/protobuf.
	//
	// Default: none. Valid: any. Spec variable: OTEL_EXPORTER_OTLP_HEADERS.
	Headers map[string]string

	// TLS secures the connection to the collector. Nil means plaintext.
	//
	// Default: nil. Valid: any. Spec variables:
	// OTEL_EXPORTER_OTLP_CERTIFICATE and the related variables.
	TLS *tls.Config

	// Timeout bounds each export.
	//
	// Default: 10s. Valid: positive. Check: CheckOTLPTimeout. Spec
	// variable: OTEL_EXPORTER_OTLP_TIMEOUT.
	Timeout time.Duration
}

// DefaultSettings returns New's defaults. With them, New exports only log
// records, to stdout. Traces are not exported, but spans still get real
// IDs, so a record logged in a span carries its trace context. Metrics are a
// no-op.
//
// DefaultSettings cannot see the process, so the service is named
// "unknown_service", the spec's name when the executable is not known. A
// Builder names the service.
func DefaultSettings() Settings {
	return Settings{
		Disabled:    false,
		ServiceName: "unknown_service",
		Traces:      ExporterNone,
		Metrics:     ExporterNone,
		Logs:        ExporterConsole,
		OTLP: OTLPSettings{
			Endpoint: "localhost:4317",
			Protocol: ProtocolGRPC,
			Timeout:  10 * time.Second,
		},
		Sampler:         sdktrace.ParentBased(sdktrace.AlwaysSample()),
		LogLevel:        log.SeverityInfo,
		ShutdownTimeout: 5 * time.Second,
	}
}

// The causes the checks report. None of them names a field: New adds the
// field's name, and a Builder sourcing a field from a variable reports the
// variable's.
var (
	errEmpty       = errors.New("must not be empty")
	errNotPositive = errors.New("must be positive")
	errNil         = errors.New("must not be nil")
)

// CheckServiceName rejects an empty service name.
func CheckServiceName(name string) error {
	if name == "" {
		return errEmpty
	}
	return nil
}

// CheckExporter rejects an Exporter other than none, console and otlp.
// Settings.Traces, Settings.Metrics and Settings.Logs share it.
func CheckExporter(e Exporter) error {
	switch e {
	case ExporterNone, ExporterConsole, ExporterOTLP:
		return nil
	}
	return fmt.Errorf("%q is not one of none, console or otlp", string(e))
}

// CheckOTLPEndpoint rejects an address that is not a host and a port: the
// host must not be empty, and the port must be a number from 1 to 65535.
func CheckOTLPEndpoint(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if host == "" {
		return fmt.Errorf("%q has no host", addr)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return fmt.Errorf("%q has no port from 1 to 65535", addr)
	}
	return nil
}

// CheckOTLPProtocol rejects a Protocol other than grpc and http/protobuf.
func CheckOTLPProtocol(p Protocol) error {
	switch p {
	case ProtocolGRPC, ProtocolHTTPProtobuf:
		return nil
	}
	return fmt.Errorf("%q is not one of grpc or http/protobuf", string(p))
}

// CheckOTLPTimeout rejects a timeout that is not positive.
func CheckOTLPTimeout(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// CheckSampler rejects a nil sampler.
func CheckSampler(s sdktrace.Sampler) error {
	if s == nil {
		return errNil
	}
	return nil
}

// CheckLogLevel rejects a severity outside log.SeverityTrace1 to
// log.SeverityFatal4, the severities the OpenTelemetry log data model
// defines.
func CheckLogLevel(l log.Severity) error {
	if l < log.SeverityTrace1 || l > log.SeverityFatal4 {
		return fmt.Errorf("%d is not a severity from %d (TRACE) to %d (FATAL4)",
			int(l), int(log.SeverityTrace1), int(log.SeverityFatal4))
	}
	return nil
}

// CheckShutdownTimeout rejects a timeout that is not positive.
func CheckShutdownTimeout(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// check runs every check on s and returns every failure, joined, each one
// naming its field.
func (s Settings) check() error {
	var errs []error
	add := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("telemetry: Settings.%s: %w", field, err))
		}
	}
	add("ServiceName", CheckServiceName(s.ServiceName))
	add("Traces", CheckExporter(s.Traces))
	add("Metrics", CheckExporter(s.Metrics))
	add("Logs", CheckExporter(s.Logs))
	add("OTLP.Endpoint", CheckOTLPEndpoint(s.OTLP.Endpoint))
	add("OTLP.Protocol", CheckOTLPProtocol(s.OTLP.Protocol))
	add("OTLP.Timeout", CheckOTLPTimeout(s.OTLP.Timeout))
	add("Sampler", CheckSampler(s.Sampler))
	add("LogLevel", CheckLogLevel(s.LogLevel))
	add("ShutdownTimeout", CheckShutdownTimeout(s.ShutdownTimeout))
	return errors.Join(errs...)
}
