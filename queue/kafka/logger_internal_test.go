// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus/humustest"
)

// newKgoLogger returns a kgoLogger over the Logger of franz-go's scope
// from tel's SDK.
func newKgoLogger(t *testing.T, tel *humustest.Telemetry) kgoLogger {
	t.Helper()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	return kgoLogger{ctx: t.Context(), logger: sdk.LoggerProvider.Logger(kgoScope)}
}

// onlyRecord returns the one record tel holds.
func onlyRecord(t *testing.T, tel *humustest.Telemetry) sdklog.Record {
	t.Helper()
	logs := tel.Logs()
	require.Len(t, logs, 1)
	return logs[0]
}

// recordAttrs returns r's attributes by key.
func recordAttrs(r sdklog.Record) map[string]attribute.Value {
	out := make(map[string]attribute.Value)
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value
		return true
	})
	return out
}

func TestKgoLogger_Log(t *testing.T) {
	t.Run("each level is its severity", func(t *testing.T) {
		for level, want := range map[kgo.LogLevel]log.Severity{
			kgo.LogLevelError: log.SeverityError,
			kgo.LogLevelWarn:  log.SeverityWarn,
			kgo.LogLevelInfo:  log.SeverityInfo,
			kgo.LogLevelDebug: log.SeverityDebug,
		} {
			t.Run(level.String(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					tel := humustest.NewTelemetry()
					newKgoLogger(t, tel).Log(level, "a message")

					r := onlyRecord(t, tel)
					require.Equal(t, kgoScope, r.InstrumentationScope().Name)
					require.Equal(t, "a message", r.Body().AsString())
					require.Equal(t, want, r.Severity())
					require.Equal(t, want.String(), r.SeverityText())
					require.True(t, r.Timestamp().Equal(time.Now()), "the record is timestamped when it is logged")
				})
			})
		}
	})

	t.Run("key-value pairs are attributes, keeping a string's, a number's and a bool's type", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			type opaque struct{ A int }
			newKgoLogger(t, tel).Log(kgo.LogLevelInfo, "kinds",
				"string", "s",
				"bool", true,
				"int", 1,
				"int8", int8(-8),
				"int16", int16(16),
				"int32", int32(-32),
				"int64", int64(64),
				"uint", uint(1),
				"uint8", uint8(8),
				"uint16", uint16(16),
				"uint32", uint32(32),
				"uint64", uint64(64),
				"uintptr", uintptr(7),
				"huge", uint64(math.MaxUint64),
				"float32", float32(0.5),
				"float64", 2.25,
				"err", errors.New("connection refused"),
				"duration", 1500*time.Millisecond,
				"struct", opaque{A: 1},
				"nil", nil,
			)

			got := recordAttrs(onlyRecord(t, tel))
			require.Equal(t, map[string]attribute.Value{
				"string":   attribute.StringValue("s"),
				"bool":     attribute.BoolValue(true),
				"int":      attribute.Int64Value(1),
				"int8":     attribute.Int64Value(-8),
				"int16":    attribute.Int64Value(16),
				"int32":    attribute.Int64Value(-32),
				"int64":    attribute.Int64Value(64),
				"uint":     attribute.Int64Value(1),
				"uint8":    attribute.Int64Value(8),
				"uint16":   attribute.Int64Value(16),
				"uint32":   attribute.Int64Value(32),
				"uint64":   attribute.Int64Value(64),
				"uintptr":  attribute.Int64Value(7),
				"huge":     attribute.StringValue("18446744073709551615"),
				"float32":  attribute.Float64Value(0.5),
				"float64":  attribute.Float64Value(2.25),
				"err":      attribute.StringValue("connection refused"),
				"duration": attribute.StringValue("1.5s"),
				"struct":   attribute.StringValue("{1}"),
				"nil":      attribute.StringValue("<nil>"),
			}, got)
		})
	})

	t.Run("a key that is not a string is its fmt text, and a key with no value has an empty one", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			newKgoLogger(t, tel).Log(kgo.LogLevelWarn, "odd", 7, "seven", "dangling")

			require.Equal(t, map[string]attribute.Value{
				"7":        attribute.StringValue("seven"),
				"dangling": attribute.StringValue(""),
			}, recordAttrs(onlyRecord(t, tel)))
		})
	})

	t.Run("no key-value pairs, no attributes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			newKgoLogger(t, tel).Log(kgo.LogLevelDebug, "bare")

			r := onlyRecord(t, tel)
			require.Zero(t, r.AttributesLen())
		})
	})
}

func TestKgoLogger_Level(t *testing.T) {
	t.Run("a Logger that records every severity is logged at debug", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			require.Equal(t, kgo.LogLevelDebug, newKgoLogger(t, humustest.NewTelemetry()).Level())
		})
	})
}
