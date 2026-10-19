// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"crypto/rand"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
)

func spansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var named []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			named = append(named, s)
		}
	}
	return named
}

func recorded(tel *humustest.Telemetry) humustest.Option {
	return humustest.WithApp(humus.WithTelemetry(tel.Builder()))
}

func TestInit_joins_the_trace_in_TRACEPARENT(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := newJob(t, "acct-1", "acct-2")
		tel := humustest.NewTelemetry()
		var (
			traceID trace.TraceID
			spanID  trace.SpanID
		)
		_, _ = rand.Read(traceID[:])
		_, _ = rand.Read(spanID[:])

		st := j.run(t, j.inMemory, map[string]string{"TRACEPARENT": "00-" + traceID.String() + "-" + spanID.String() + "-01"}, recorded(tel))

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		spans := tel.Spans()
		job := spansNamed(spans, "job")
		require.Len(t, job, 1)
		require.Equal(t, traceID, job[0].SpanContext().TraceID())
		require.Equal(t, spanID, job[0].Parent().SpanID())
		require.True(t, job[0].Parent().IsRemote())

		issue := spansNamed(spans, "issue")
		require.Len(t, issue, 1)
		require.Equal(t, job[0].SpanContext().SpanID(), issue[0].Parent().SpanID())
		charges := spansNamed(spans, "charge")
		require.Len(t, charges, 2)
		under := map[trace.SpanID]int{}
		for _, c := range charges {
			require.Equal(t, job[0].SpanContext().SpanID(), c.Parent().SpanID())
			under[c.SpanContext().SpanID()] = 0
		}
		attempts := spansNamed(spans, http.MethodPost)
		require.Len(t, attempts, 2)
		for _, a := range attempts {
			_, ok := under[a.Parent().SpanID()]
			require.True(t, ok, "an attempt's span is under no charge span")
			under[a.Parent().SpanID()]++
		}
		for _, n := range under {
			require.Equal(t, 1, n)
		}
		for _, s := range spans {
			require.Equal(t, traceID, s.SpanContext().TraceID(), s.Name())
		}
	})
}

func TestInit_without_TRACEPARENT_roots_a_new_trace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := newJob(t, "acct-1")
		tel := humustest.NewTelemetry()

		st := j.run(t, j.inMemory, nil, recorded(tel))

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		job := spansNamed(tel.Spans(), "job")
		require.Len(t, job, 1)
		require.False(t, job[0].Parent().IsValid())
	})
}

func TestInit_never_writes_its_secrets_to_a_span_or_a_record(t *testing.T) {
	t.Run("a run that bills", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			j := newJob(t, "acct-1", "acct-2")
			tel := humustest.NewTelemetry()

			st := j.run(t, j.inMemory, nil, recorded(tel))

			require.Zero(t, st.Code(), "cause: %v", st.Cause())
			require.Len(t, spansNamed(tel.Spans(), http.MethodPost), 2)
			require.NotEmpty(t, tel.Logs())
			requireNoSecret(t, tel, j.key, j.password, j.databaseURL())
		})
	})
	t.Run("a run whose database refuses it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			j := newJob(t, "acct-1")
			tel := humustest.NewTelemetry()
			l, err := j.host.Listen("tcp", "db.internal:5432")
			require.NoError(t, err)
			go func() {
				for {
					conn, err := l.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()

			st := j.run(t, Init, nil, recorded(tel))

			require.NoError(t, l.Close())
			require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
			job := spansNamed(tel.Spans(), "job")
			require.Len(t, job, 1)
			require.Equal(t, codes.Error, job[0].Status().Code)
			issue := spansNamed(tel.Spans(), "issue")
			require.Len(t, issue, 1)
			require.Equal(t, codes.Error, issue[0].Status().Code)
			require.NotEmpty(t, tel.Logs())
			requireNoSecret(t, tel, j.key, j.password, j.databaseURL())
		})
	})
}

func (j *target) databaseURL() string {
	return "postgres://billing:" + j.password + "@db.internal:5432/billing?sslmode=disable"
}

func requireNoSecret(t *testing.T, tel *humustest.Telemetry, secrets ...string) {
	t.Helper()
	var written []string
	attrs := func(kvs []attribute.KeyValue) {
		for _, kv := range kvs {
			written = append(written, string(kv.Key), kv.Value.String())
		}
	}
	for _, s := range tel.Spans() {
		written = append(written, s.Name(), s.Status().Description)
		attrs(s.Attributes())
		for _, ev := range s.Events() {
			written = append(written, ev.Name)
			attrs(ev.Attributes)
		}
		for _, l := range s.Links() {
			attrs(l.Attributes)
		}
	}
	for _, r := range tel.Logs() {
		written = append(written, r.Body().String())
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			written = append(written, string(kv.Key), kv.Value.String())
			return true
		})
	}
	for _, w := range written {
		for _, secret := range secrets {
			require.NotContains(t, w, secret)
		}
	}
}
