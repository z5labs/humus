// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/noop"
)

func TestNewServer_passes_each_QUIC_limit_explicitly(t *testing.T) {
	t.Parallel()

	settings := DefaultSettings()
	settings.TLS = &tls.Config{}
	srv := newServer(settings, http.NotFoundHandler(), noop.NewLoggerProvider().Logger(loggerName))
	c := srv.QUICConfig
	require.NotNil(t, c, "a nil QUICConfig accepts 0-RTT")
	require.Equal(t, 30*time.Second, c.MaxIdleTimeout)
	require.Equal(t, int64(100), c.MaxIncomingStreams)
	require.Equal(t, int64(100), c.MaxIncomingUniStreams)
	require.Equal(t, uint64(6<<20), c.MaxStreamReceiveWindow)
	require.Equal(t, uint64(15<<20), c.MaxConnectionReceiveWindow)
	require.False(t, c.Allow0RTT)
	require.Zero(t, c.KeepAlivePeriod)
	require.False(t, c.EnableDatagrams)
	require.False(t, srv.EnableDatagrams)
	require.NotNil(t, srv.Logger, "a nil Logger logs to slog.Default()")
	require.NotSame(t, settings.TLS, srv.TLSConfig, "the server has a clone of the end user's config")
}

func TestNewServer_applies_the_Settings(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                  string
		header, read, idle    time.Duration
		wantHeader, wantIdle  time.Duration
		maxHeaderBytes, wantN int
	}{
		{"each field set", 2 * time.Second, 5 * time.Second, 120 * time.Second, 2 * time.Second, 120 * time.Second, 1 << 20, 1 << 20},
		{"ReadTimeout when ReadHeaderTimeout and IdleTimeout are zero", 0, 5 * time.Second, 0, 5 * time.Second, 5 * time.Second, 4096, 4096},
		{"no timeout at all", 0, 0, 0, 0, 0, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings := DefaultSettings()
			settings.TLS = &tls.Config{}
			settings.ReadHeaderTimeout = tc.header
			settings.ReadTimeout = tc.read
			settings.IdleTimeout = tc.idle
			settings.MaxHeaderBytes = tc.maxHeaderBytes
			srv := newServer(settings, http.NotFoundHandler(), noop.NewLoggerProvider().Logger(loggerName))
			require.Equal(t, tc.wantHeader, srv.ReadHeaderTimeout)
			require.Equal(t, tc.wantIdle, srv.IdleTimeout)
			require.Equal(t, tc.wantN, srv.MaxHeaderBytes)
		})
	}
}

func TestHandshakeIdleTimeout_is_half_the_smallest_timeout_set(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                string
		header, read, write time.Duration
		want                time.Duration
	}{
		{"the defaults", 2 * time.Second, 5 * time.Second, 10 * time.Second, time.Second},
		{"ReadTimeout the smallest", 8 * time.Second, 4 * time.Second, 10 * time.Second, 2 * time.Second},
		{"WriteTimeout the smallest", 8 * time.Second, 6 * time.Second, 3 * time.Second, 1500 * time.Millisecond},
		{"a zero timeout is not set", 0, 6 * time.Second, 0, 3 * time.Second},
		{"none set", 0, 0, 0, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings := DefaultSettings()
			settings.ReadHeaderTimeout = tc.header
			settings.ReadTimeout = tc.read
			settings.WriteTimeout = tc.write
			require.Equal(t, tc.want, quicConfig(settings).HandshakeIdleTimeout)
		})
	}
}

// recordingLogger is a log.Logger that keeps every record emitted through
// it.
type recordingLogger struct {
	noop.Logger
	records []log.Record
}

func (l *recordingLogger) Emit(_ context.Context, r log.Record) {
	l.records = append(l.records, r)
}

// attrs returns r's attributes as a map of their keys to their values.
func attrs(r log.Record) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		m[string(kv.Key)] = kv.Value
		return true
	})
	return m
}

func TestSlogLogger_emits_only_Error_records(t *testing.T) {
	t.Parallel()

	rec := &recordingLogger{}
	logger := newSlogLogger(rec)
	logger.Debug("handling request", "method", "GET")
	logger.Info("listening")
	logger.Warn("odd")
	require.Empty(t, rec.records)

	logger.Error("http3: panic serving", "arg", "boom")
	require.Len(t, rec.records, 1)
	require.Equal(t, log.SeverityError, rec.records[0].Severity())
	require.Equal(t, "ERROR", rec.records[0].SeverityText())
	require.Equal(t, "http3: panic serving", rec.records[0].Body().AsString())
	require.Equal(t, "boom", attrs(rec.records[0])["arg"].AsString())
}

func TestSlogLogger_keeps_the_attributes_types_and_groups(t *testing.T) {
	t.Parallel()

	rec := &recordingLogger{}
	logger := newSlogLogger(rec).With("conn", "c1").WithGroup("").WithGroup("req")
	logger.Error("failed",
		"path", "/orders",
		"retried", true,
		"attempt", 3,
		"ratio", 0.5,
		"err", errors.New("boom"),
		slog.Group("peer", "port", 8443),
		slog.Attr{},
	)
	require.Len(t, rec.records, 1)
	got := attrs(rec.records[0])
	require.Equal(t, map[string]attribute.Value{
		"conn":          attribute.StringValue("c1"),
		"req.path":      attribute.StringValue("/orders"),
		"req.retried":   attribute.BoolValue(true),
		"req.attempt":   attribute.Int64Value(3),
		"req.ratio":     attribute.Float64Value(0.5),
		"req.err":       attribute.StringValue("boom"),
		"req.peer.port": attribute.Int64Value(8443),
	}, got)
}
