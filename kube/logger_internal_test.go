// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus/humustest"
)

// stringer is a value with a String method, which a record shows as its
// text.
type stringer struct{}

func (stringer) String() string { return "a stringer" }

func TestLogger_emits_client_gos_records(t *testing.T) {
	t.Parallel()

	tel := humustest.NewTelemetry()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	logger := newLogger(sdk.LoggerProvider.Logger(scopeName))

	logger.V(2).Info("a detail klog drops by default")
	logger.WithName("reflector").WithName("memcacheds").WithValues("resource", "memcacheds").
		Info("Caches populated", "count", 3, "int32", int32(4), "int64", int64(5), "uint32", uint32(6),
			"uint64", uint64(7), "huge", uint64(math.MaxUint64), "ratio", 0.5, "ok", true,
			"err", errors.New("refused"), "type", stringer{}, 7, "a key that isn't a string", "no value")
	logger.Error(errors.New("refused"), "Failed to watch")
	logger.Error(nil, "Unable to sync caches")

	records := tel.Logs()
	require.Len(t, records, 3, "verbosity 2 is dropped")

	info := records[0]
	require.Equal(t, "Caches populated", info.Body().AsString())
	require.Equal(t, log.SeverityInfo, info.Severity())
	require.Equal(t, scopeName, info.InstrumentationScope().Name)
	require.Equal(t, map[string]attribute.Value{
		"logger":   attribute.StringValue("reflector/memcacheds"),
		"resource": attribute.StringValue("memcacheds"),
		"count":    attribute.Int64Value(3),
		"int32":    attribute.Int64Value(4),
		"int64":    attribute.Int64Value(5),
		"uint32":   attribute.Int64Value(6),
		"uint64":   attribute.Int64Value(7),
		"huge":     attribute.StringValue("18446744073709551615"),
		"ratio":    attribute.Float64Value(0.5),
		"ok":       attribute.BoolValue(true),
		"err":      attribute.StringValue("refused"),
		"type":     attribute.StringValue("a stringer"),
		"7":        attribute.StringValue("a key that isn't a string"),
		"no value": attribute.StringValue(""),
	}, attrsOf(&info))

	failed := records[1]
	require.Equal(t, "Failed to watch", failed.Body().AsString())
	require.Equal(t, log.SeverityError, failed.Severity())
	require.Equal(t, map[string]attribute.Value{"exception.message": attribute.StringValue("refused")}, attrsOf(&failed))

	unsynced := records[2]
	require.Equal(t, log.SeverityError, unsynced.Severity())
	require.Empty(t, attrsOf(&unsynced))
}

// attrsOf returns r's attributes by their keys.
func attrsOf(r *sdklog.Record) map[string]attribute.Value {
	attrs := map[string]attribute.Value{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	return attrs
}
