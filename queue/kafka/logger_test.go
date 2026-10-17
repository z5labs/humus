// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"bytes"
	"context"
	stdlog "log"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
	"github.com/z5labs/humus/telemetry"
)

// kgoScope is the scope franz-go's own logs are recorded under.
const kgoScope = "github.com/twmb/franz-go/pkg/kgo"

// unreachable is a broker address nothing listens on.
const unreachable = "127.0.0.1:9092"

// kgoRecords returns the log records of logs under franz-go's scope.
func kgoRecords(logs []sdklog.Record) []sdklog.Record {
	var out []sdklog.Record
	for _, r := range logs {
		if r.InstrumentationScope().Name == kgoScope {
			out = append(out, r)
		}
	}
	return out
}

// kgoRecord returns the first record under franz-go's scope with body,
// failing the test when there is none.
func kgoRecord(t *testing.T, logs []sdklog.Record, body string) sdklog.Record {
	t.Helper()
	for _, r := range kgoRecords(logs) {
		if r.Body().AsString() == body {
			return r
		}
	}
	t.Fatalf("franz-go logged no %q", body)
	return sdklog.Record{}
}

// typedAttrs returns r's attributes by key.
func typedAttrs(r sdklog.Record) map[string]attribute.Value {
	out := make(map[string]attribute.Value)
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value
		return true
	})
	return out
}

// minSeverityProvider is a LoggerProvider whose Loggers are Enabled only
// from min. Its Loggers still emit every record they are given, to the
// LoggerProvider underneath, so a record below min in the Telemetry's Logs
// is one that was built although Enabled said it would be dropped.
type minSeverityProvider struct {
	embedded.LoggerProvider
	lp  log.LoggerProvider
	min log.Severity
}

func (p minSeverityProvider) Logger(name string, opts ...log.LoggerOption) log.Logger {
	return minSeverityLogger{Logger: p.lp.Logger(name, opts...), min: p.min}
}

type minSeverityLogger struct {
	log.Logger
	min log.Severity
}

func (l minSeverityLogger) Enabled(ctx context.Context, param log.EnabledParameters) bool {
	return param.Severity >= l.min && l.Logger.Enabled(ctx, param)
}

// enabledFrom returns tel's Builder with its LoggerProvider Enabled only
// from min.
func enabledFrom(tel *humustest.Telemetry, min log.Severity) telemetry.Builder {
	build := tel.Builder()
	return func(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
		sdk, err := build(ctx, p, dial)
		sdk.LoggerProvider = minSeverityProvider{lp: sdk.LoggerProvider, min: min}
		return sdk, err
	}
}

// enabledForNothing is above every severity.
const enabledForNothing = log.SeverityFatal4 + 1

// serveUnreachable runs an App whose only Service consumes from a broker
// nothing listens on, with telemetry from b, for a minute, then stops it
// with SIGTERM. It returns the Level of the Service's franz-go logger.
func serveUnreachable(t *testing.T, b telemetry.Builder) kgo.LogLevel {
	t.Helper()
	// The log package's default logger writes to the real standard error;
	// it is captured for the run. The tests that call this don't run in
	// parallel.
	var std bytes.Buffer
	prev := stdlog.Writer()
	stdlog.SetOutput(&std)
	defer stdlog.SetOutput(prev)

	h := humustest.NewHost(t)
	var level kgo.LogLevel
	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := kafka.NewService(s, settings(unreachable), queue.ProcessorFunc[kafka.Message](func(context.Context, kafka.Message) error { return nil }))
		if err != nil {
			return nil, err
		}
		level = kafka.KgoLevel(svc)
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(b)))

	time.Sleep(time.Minute)
	p.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), st.Cause())
	require.Empty(t, p.Stderr(), "franz-go wrote to standard error")
	require.Empty(t, std.String(), "franz-go wrote to the log package's default logger")
	return level
}

func TestService_sends_franz_gos_logs_to_the_Setups_telemetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 1)

		tel := humustest.NewTelemetry()
		// consumeAll fails the test if anything reached p.Stderr().
		require.Zero(t, consumeAll(t, h, tel, settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error { return nil }))

		logs := kgoRecords(tel.Logs())
		require.NotEmpty(t, logs)
		severities := map[string]bool{}
		for _, r := range logs {
			require.Contains(t, []log.Severity{log.SeverityError, log.SeverityWarn, log.SeverityInfo, log.SeverityDebug}, r.Severity(), r.Body().AsString())
			require.Equal(t, r.Severity().String(), r.SeverityText())
			severities[r.SeverityText()] = true
		}
		require.True(t, severities["INFO"], "franz-go logged at INFO")
		require.True(t, severities["DEBUG"], "franz-go logged at DEBUG")

		joining := kgoRecord(t, tel.Logs(), "joining group")
		require.Equal(t, log.SeverityInfo, joining.Severity())
		require.Equal(t, map[string]attribute.Value{"group": attribute.StringValue(group)}, typedAttrs(joining))

		versions := typedAttrs(kgoRecord(t, tel.Logs(), "issuing api versions request"))
		require.Equal(t, attribute.INT64, versions["version"].Type(), "a number keeps its type")
		require.Equal(t, attribute.STRING, versions["broker"].Type())
	})
}

func TestService_logs_franz_gos_connection_failures_while_the_brokers_are_unreachable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		require.Equal(t, kgo.LogLevelDebug, serveUnreachable(t, tel.Builder()))

		r := kgoRecord(t, tel.Logs(), "unable to open connection to broker")
		require.Equal(t, log.SeverityWarn, r.Severity())
		require.Equal(t, "WARN", r.SeverityText())
		got := typedAttrs(r)
		require.Equal(t, attribute.StringValue(unreachable), got["addr"])
		require.Equal(t, attribute.STRING, got["err"].Type(), "an error is recorded as its text")
		require.NotEmpty(t, got["err"].AsString())

		var failures int
		for _, r := range kgoRecords(tel.Logs()) {
			if r.Body().AsString() == "unable to open connection to broker" {
				failures++
			}
		}
		require.Greater(t, failures, 1, "franz-go logs every attempt while it retries")
	})
}

func TestService_franz_gos_logs_follow_the_Loggers_severities(t *testing.T) {
	t.Run("a Logger enabled from WARN is logged at warn, with no INFO or DEBUG record", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			require.Equal(t, kgo.LogLevelWarn, serveUnreachable(t, enabledFrom(tel, log.SeverityWarn)))

			logs := kgoRecords(tel.Logs())
			require.NotEmpty(t, logs, "franz-go still logs its connection failures at WARN")
			for _, r := range logs {
				require.GreaterOrEqual(t, r.Severity(), log.SeverityWarn, "franz-go built %s %q", r.SeverityText(), r.Body().AsString())
			}
		})
	})

	t.Run("a Logger enabled for nothing is logged at none, with no record", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			require.Equal(t, kgo.LogLevelNone, serveUnreachable(t, enabledFrom(tel, enabledForNothing)))

			require.Empty(t, kgoRecords(tel.Logs()))
		})
	})
}
