// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus/telemetry"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
var fieldNames = []string{
	"ServiceName", "Traces", "Metrics", "Logs", "Exporter", "Endpoint",
	"Protocol", "Timeout", "Sampler", "LogLevel", "ShutdownTimeout", "OTLP",
	"service name", "endpoint", "protocol", "timeout", "sampler", "level",
}

func TestChecks(t *testing.T) {
	t.Parallel()

	type check struct {
		name   string
		accept []func() error
		reject []func() error
	}
	checks := []check{
		{
			name:   "CheckServiceName",
			accept: []func() error{func() error { return telemetry.CheckServiceName("carts") }},
			reject: []func() error{func() error { return telemetry.CheckServiceName("") }},
		},
		{
			name: "CheckExporter",
			accept: []func() error{
				func() error { return telemetry.CheckExporter(telemetry.ExporterNone) },
				func() error { return telemetry.CheckExporter(telemetry.ExporterConsole) },
				func() error { return telemetry.CheckExporter(telemetry.ExporterOTLP) },
			},
			reject: []func() error{
				func() error { return telemetry.CheckExporter("") },
				func() error { return telemetry.CheckExporter("jaeger") },
				func() error { return telemetry.CheckExporter("Otlp") },
			},
		},
		{
			name: "CheckOTLPEndpoint",
			accept: []func() error{
				func() error { return telemetry.CheckOTLPEndpoint("localhost:4317") },
				func() error { return telemetry.CheckOTLPEndpoint("[::1]:4318") },
				func() error { return telemetry.CheckOTLPEndpoint("otel-collector.observability:65535") },
			},
			reject: []func() error{
				func() error { return telemetry.CheckOTLPEndpoint("") },
				func() error { return telemetry.CheckOTLPEndpoint("localhost") },
				func() error { return telemetry.CheckOTLPEndpoint("http://localhost:4317") },
				func() error { return telemetry.CheckOTLPEndpoint(":4317") },
				func() error { return telemetry.CheckOTLPEndpoint("localhost:0") },
				func() error { return telemetry.CheckOTLPEndpoint("localhost:65536") },
				func() error { return telemetry.CheckOTLPEndpoint("localhost:otlp") },
			},
		},
		{
			name: "CheckOTLPProtocol",
			accept: []func() error{
				func() error { return telemetry.CheckOTLPProtocol(telemetry.ProtocolGRPC) },
				func() error { return telemetry.CheckOTLPProtocol(telemetry.ProtocolHTTPProtobuf) },
			},
			reject: []func() error{
				func() error { return telemetry.CheckOTLPProtocol("") },
				func() error { return telemetry.CheckOTLPProtocol("http/json") },
			},
		},
		{
			name:   "CheckOTLPTimeout",
			accept: []func() error{func() error { return telemetry.CheckOTLPTimeout(time.Nanosecond) }},
			reject: []func() error{
				func() error { return telemetry.CheckOTLPTimeout(0) },
				func() error { return telemetry.CheckOTLPTimeout(-time.Second) },
			},
		},
		{
			name:   "CheckShutdownTimeout",
			accept: []func() error{func() error { return telemetry.CheckShutdownTimeout(time.Second) }},
			reject: []func() error{
				func() error { return telemetry.CheckShutdownTimeout(0) },
				func() error { return telemetry.CheckShutdownTimeout(-time.Second) },
			},
		},
		{
			name:   "CheckSampler",
			accept: []func() error{func() error { return telemetry.CheckSampler(sdktrace.NeverSample()) }},
			reject: []func() error{func() error { return telemetry.CheckSampler(nil) }},
		},
		{
			name: "CheckLogLevel",
			accept: []func() error{
				func() error { return telemetry.CheckLogLevel(log.SeverityTrace1) },
				func() error { return telemetry.CheckLogLevel(log.SeverityInfo) },
				func() error { return telemetry.CheckLogLevel(log.SeverityFatal4) },
			},
			reject: []func() error{
				func() error { return telemetry.CheckLogLevel(log.SeverityUndefined) },
				func() error { return telemetry.CheckLogLevel(log.SeverityFatal4 + 1) },
				func() error { return telemetry.CheckLogLevel(-1) },
			},
		},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for i, accept := range c.accept {
				require.NoError(t, accept(), "in-range value %d", i)
			}
			for i, reject := range c.reject {
				err := reject()
				require.Error(t, err, "out-of-range value %d", i)
				for _, name := range fieldNames {
					require.NotContains(t, err.Error(), name, "the error of out-of-range value %d names a field", i)
				}
			}
		})
	}
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	s := telemetry.DefaultSettings()
	require.False(t, s.Disabled)
	require.Equal(t, "unknown_service", s.ServiceName)
	require.Empty(t, s.ResourceOptions)
	require.Equal(t, telemetry.ExporterNone, s.Traces)
	require.Equal(t, telemetry.ExporterNone, s.Metrics)
	require.Equal(t, telemetry.ExporterConsole, s.Logs)
	require.Equal(t, "localhost:4317", s.OTLP.Endpoint)
	require.Equal(t, telemetry.ProtocolGRPC, s.OTLP.Protocol)
	require.Empty(t, s.OTLP.Headers)
	require.Nil(t, s.OTLP.TLS)
	require.Equal(t, 10*time.Second, s.OTLP.Timeout)
	require.Equal(t, sdktrace.ParentBased(sdktrace.AlwaysSample()).Description(), s.Sampler.Description())
	require.Equal(t, log.SeverityInfo, s.LogLevel)
	require.Equal(t, 5*time.Second, s.ShutdownTimeout)
}

func TestNew_names_each_bad_field(t *testing.T) {
	t.Parallel()

	fields := []struct {
		name string
		set  func(*telemetry.Settings)
	}{
		{"ServiceName", func(s *telemetry.Settings) { s.ServiceName = "" }},
		{"Traces", func(s *telemetry.Settings) { s.Traces = "jaeger" }},
		{"Metrics", func(s *telemetry.Settings) { s.Metrics = "prometheus" }},
		{"Logs", func(s *telemetry.Settings) { s.Logs = "syslog" }},
		{"OTLP.Endpoint", func(s *telemetry.Settings) { s.OTLP.Endpoint = "localhost" }},
		{"OTLP.Protocol", func(s *telemetry.Settings) { s.OTLP.Protocol = "http/json" }},
		{"OTLP.Timeout", func(s *telemetry.Settings) { s.OTLP.Timeout = 0 }},
		{"Sampler", func(s *telemetry.Settings) { s.Sampler = nil }},
		{"LogLevel", func(s *telemetry.Settings) { s.LogLevel = log.SeverityUndefined }},
		{"ShutdownTimeout", func(s *telemetry.Settings) { s.ShutdownTimeout = -time.Second }},
	}
	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			s := telemetry.DefaultSettings()
			f.set(&s)

			sdk, err := telemetry.New(t.Context(), s, failingDialer(t), &strings.Builder{})
			require.Error(t, err)
			require.Contains(t, err.Error(), "Settings."+f.name+":")
			require.Zero(t, sdk.TracerProvider, "New built something")
			require.Nil(t, sdk.Shutdown, "New built something")
		})
	}
}

func TestNew_reports_two_bad_fields_together(t *testing.T) {
	t.Parallel()

	s := telemetry.DefaultSettings()
	s.ServiceName = ""
	s.OTLP.Timeout = -time.Second
	var out strings.Builder

	_, err := telemetry.New(t.Context(), s, failingDialer(t), &out)
	require.ErrorContains(t, err, "Settings.ServiceName: must not be empty")
	require.ErrorContains(t, err, "Settings.OTLP.Timeout: must be positive")
	require.Empty(t, out.String())
}

func TestNew_DefaultSettings_pass(t *testing.T) {
	t.Parallel()

	sdk, err := telemetry.New(t.Context(), telemetry.DefaultSettings(), failingDialer(t), &strings.Builder{})
	require.NoError(t, err)
	require.NoError(t, sdk.Shutdown(t.Context()))
}
