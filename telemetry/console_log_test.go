// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

// emit logs r through an sdklog.LoggerProvider whose simple processor
// exports to a jsonLogExporter, from a logger named scope, and returns
// everything the exporter wrote.
func emit(t *testing.T, ctx context.Context, scope string, rs ...log.Record) string {
	t.Helper()
	var out bytes.Buffer
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(newJSONLogExporter(&out))),
	)
	defer func() { require.NoError(t, lp.Shutdown(context.Background())) }()

	logger := lp.Logger(scope)
	for _, r := range rs {
		logger.Emit(ctx, r)
	}
	requireValidLines(t, out.String())
	return out.String()
}

// requireValidLines checks that out is a sequence of newline-terminated
// lines, each of them valid JSON.
func requireValidLines(t *testing.T, out string) {
	t.Helper()
	require.True(t, strings.HasSuffix(out, "\n"), "output ends with a newline: %q", out)
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		require.True(t, json.Valid([]byte(line)), "valid JSON: %s", line)
	}
}

func spanContext(t *testing.T) context.Context {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("e2f668b6c06f2de82e61e2517c2d7a07")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("ff25eb972a5557d5")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	require.True(t, sc.IsValid())
	return trace.ContextWithSpanContext(t.Context(), sc)
}

var testTimestamp = time.Date(2026, time.October, 16, 6, 56, 32, 447829601, time.UTC)

func TestJSONLogExporter_golden(t *testing.T) {
	var r log.Record
	r.SetTimestamp(testTimestamp)
	r.SetSeverity(log.SeverityInfo)
	r.SetSeverityText("INFO")
	r.SetBody(attribute.StringValue("cart created"))
	r.AddAttributes(attribute.String("cart.id", "c-123"))

	got := emit(t, spanContext(t), "carts", r)

	want := `{"timestamp":"2026-10-16T06:56:32.447829601Z","severity_text":"INFO","severity_number":9,"body":"cart created","trace_id":"e2f668b6c06f2de82e61e2517c2d7a07","span_id":"ff25eb972a5557d5","trace_flags":"01","scope":"carts","attributes":{"cart.id":"c-123"}}` + "\n"
	require.Equal(t, want, got)
}

func TestJSONLogExporter_without_a_span_context_has_no_trace_fields(t *testing.T) {
	var r log.Record
	r.SetTimestamp(testTimestamp)
	r.SetSeverity(log.SeverityInfo)
	r.SetBody(attribute.StringValue("cart created"))

	got := emit(t, t.Context(), "carts", r)

	require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","severity_text":"INFO","severity_number":9,"body":"cart created","scope":"carts"}`+"\n", got)
	require.NotContains(t, got, "trace_id")
	require.NotContains(t, got, "span_id")
	require.NotContains(t, got, "trace_flags")
}

func TestJSONLogExporter_an_invalid_span_context_has_no_trace_fields(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("e2f668b6c06f2de82e61e2517c2d7a07")
	require.NoError(t, err)

	var r sdklog.Record
	r.SetTimestamp(testTimestamp)
	r.SetTraceID(traceID)
	r.SetTraceFlags(trace.FlagsSampled)

	var out bytes.Buffer
	require.NoError(t, newJSONLogExporter(&out).Export(t.Context(), []sdklog.Record{r}))
	require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","scope":""}`+"\n", out.String())
}

func TestJSONLogExporter_fallbacks(t *testing.T) {
	t.Run("an unset timestamp is replaced by the observed timestamp", func(t *testing.T) {
		var r log.Record
		r.SetObservedTimestamp(time.Date(2026, time.October, 16, 8, 56, 32, 1, time.FixedZone("CEST", 2*60*60)))

		got := emit(t, t.Context(), "carts", r)

		require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.000000001Z","scope":"carts"}`+"\n", got)
	})

	t.Run("the timestamp wins over the observed timestamp", func(t *testing.T) {
		var r log.Record
		r.SetTimestamp(testTimestamp)
		r.SetObservedTimestamp(testTimestamp.Add(time.Hour))

		got := emit(t, t.Context(), "carts", r)

		require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","scope":"carts"}`+"\n", got)
	})

	t.Run("an empty severity text is replaced by the severity's short name", func(t *testing.T) {
		// Every one of the 24, since the names are part of the format.
		names := []string{
			"TRACE", "TRACE2", "TRACE3", "TRACE4",
			"DEBUG", "DEBUG2", "DEBUG3", "DEBUG4",
			"INFO", "INFO2", "INFO3", "INFO4",
			"WARN", "WARN2", "WARN3", "WARN4",
			"ERROR", "ERROR2", "ERROR3", "ERROR4",
			"FATAL", "FATAL2", "FATAL3", "FATAL4",
		}
		for i, name := range names {
			sev := log.Severity(i + 1)
			t.Run(name, func(t *testing.T) {
				var r log.Record
				r.SetTimestamp(testTimestamp)
				r.SetSeverity(sev)

				got := emit(t, t.Context(), "carts", r)

				want := fmt.Sprintf(`{"timestamp":"2026-10-16T06:56:32.447829601Z","severity_text":%q,"severity_number":%d,"scope":"carts"}`+"\n", name, sev)
				require.Equal(t, want, got)
			})
		}
	})

	t.Run("a severity text is written as it is", func(t *testing.T) {
		var r log.Record
		r.SetTimestamp(testTimestamp)
		r.SetSeverity(log.SeverityWarn)
		r.SetSeverityText("warning")

		got := emit(t, t.Context(), "carts", r)

		require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","severity_text":"warning","severity_number":13,"scope":"carts"}`+"\n", got)
	})
}

func TestJSONLogExporter_absent_keys(t *testing.T) {
	t.Run("a record with no severity has neither severity key", func(t *testing.T) {
		var r log.Record
		r.SetTimestamp(testTimestamp)
		r.SetBody(attribute.StringValue("hello"))

		got := emit(t, t.Context(), "carts", r)

		require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","body":"hello","scope":"carts"}`+"\n", got)
	})

	t.Run("a severity outside 1 to 24 is not written", func(t *testing.T) {
		var r log.Record
		r.SetTimestamp(testTimestamp)
		r.SetSeverity(log.Severity(25))
		r.SetSeverityText("LOUD")

		got := emit(t, t.Context(), "carts", r)

		require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","scope":"carts"}`+"\n", got)
	})

	t.Run("a record with an empty body has no body", func(t *testing.T) {
		var r log.Record
		r.SetTimestamp(testTimestamp)
		r.SetSeverity(log.SeverityInfo)
		r.SetBody(attribute.Value{})

		got := emit(t, t.Context(), "carts", r)

		require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","severity_text":"INFO","severity_number":9,"scope":"carts"}`+"\n", got)
	})

	t.Run("an empty string is a body", func(t *testing.T) {
		var r log.Record
		r.SetTimestamp(testTimestamp)
		r.SetBody(attribute.StringValue(""))

		got := emit(t, t.Context(), "carts", r)

		require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","body":"","scope":"carts"}`+"\n", got)
	})

	t.Run("a record with no attributes has no attributes", func(t *testing.T) {
		var r log.Record
		r.SetTimestamp(testTimestamp)
		r.SetBody(attribute.StringValue("hello"))

		got := emit(t, t.Context(), "carts", r)

		require.NotContains(t, got, "attributes")
	})

	t.Run("no resource and no dropped-attribute count are written", func(t *testing.T) {
		var out bytes.Buffer
		lp := sdklog.NewLoggerProvider(
			sdklog.WithAttributeCountLimit(1),
			sdklog.WithProcessor(sdklog.NewSimpleProcessor(newJSONLogExporter(&out))),
		)
		defer func() { require.NoError(t, lp.Shutdown(context.Background())) }()

		var r log.Record
		r.SetTimestamp(testTimestamp)
		r.AddAttributes(attribute.String("a", "1"), attribute.String("b", "2"))
		lp.Logger("carts").Emit(t.Context(), r)

		require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","scope":"carts","attributes":{"a":"1"}}`+"\n", out.String())
	})
}

func TestJSONLogExporter_event_name(t *testing.T) {
	var r log.Record
	r.SetTimestamp(testTimestamp)
	r.SetSeverity(log.SeverityInfo)
	r.SetEventName("cart.created")
	r.SetBody(attribute.StringValue("cart created"))

	got := emit(t, t.Context(), "carts", r)

	require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","severity_text":"INFO","severity_number":9,"event_name":"cart.created","body":"cart created","scope":"carts"}`+"\n", got)
}

func TestJSONLogExporter_values(t *testing.T) {
	cases := []struct {
		name  string
		value attribute.Value
		want  string
	}{
		{"string", attribute.StringValue("c-123"), `"c-123"`},
		{"bool", attribute.BoolValue(true), `true`},
		{"int", attribute.Int64Value(-42), `-42`},
		{"float", attribute.Float64Value(1.5), `1.5`},
		{"whole float", attribute.Float64Value(3), `3`},
		{"small float", attribute.Float64Value(1e-9), `1e-9`},
		{"large float", attribute.Float64Value(1e21), `1e+21`},
		{"bytes", attribute.ByteSliceValue([]byte("hello, world")), `"aGVsbG8sIHdvcmxk"`},
		{"empty bytes", attribute.ByteSliceValue([]byte{}), `""`},
		{"bool slice", attribute.BoolSliceValue([]bool{true, false}), `[true,false]`},
		{"int slice", attribute.Int64SliceValue([]int64{1, 2, 3}), `[1,2,3]`},
		{"float slice", attribute.Float64SliceValue([]float64{1.5, math.Inf(1)}), `[1.5,"+Inf"]`},
		{"string slice", attribute.StringSliceValue([]string{"a", "b"}), `["a","b"]`},
		{"empty string slice", attribute.StringSliceValue([]string{}), `[]`},
		{
			"slice",
			attribute.SliceValue(
				attribute.StringValue("a"),
				attribute.Int64Value(1),
				attribute.ByteSliceValue([]byte{0xff}),
				attribute.Value{},
			),
			`["a",1,"/w==",null]`,
		},
		{
			"map",
			attribute.MapValue(
				attribute.String("id", "c-123"),
				attribute.Int64("items", 2),
				attribute.Int64Slice("sizes", []int64{1, 2}),
			),
			`{"id":"c-123","items":2,"sizes":[1,2]}`,
		},
		{
			"nested",
			attribute.MapValue(
				attribute.Slice("lines", attribute.MapValue(
					attribute.String("sku", "s-1"),
					attribute.KeyValue{Key: "raw", Value: attribute.ByteSliceValue([]byte("x"))},
				)),
			),
			`{"lines":[{"raw":"eA==","sku":"s-1"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("body", func(t *testing.T) {
				var r log.Record
				r.SetTimestamp(testTimestamp)
				r.SetBody(tc.value)

				got := emit(t, t.Context(), "carts", r)

				require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","body":`+tc.want+`,"scope":"carts"}`+"\n", got)
			})

			t.Run("attribute", func(t *testing.T) {
				var r log.Record
				r.SetTimestamp(testTimestamp)
				r.AddAttributes(attribute.KeyValue{Key: "v", Value: tc.value})

				got := emit(t, t.Context(), "carts", r)

				require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","scope":"carts","attributes":{"v":`+tc.want+`}}`+"\n", got)
			})
		})
	}
}

func TestJSONLogExporter_attributes_keep_their_order(t *testing.T) {
	var r log.Record
	r.SetTimestamp(testTimestamp)
	r.AddAttributes(
		attribute.String("z", "1"),
		attribute.String("a", "2"),
		attribute.String("m", "3"),
	)

	got := emit(t, t.Context(), "carts", r)

	require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","scope":"carts","attributes":{"z":"1","a":"2","m":"3"}}`+"\n", got)
}

func TestJSONLogExporter_an_empty_attribute_value_is_null(t *testing.T) {
	var r log.Record
	r.SetTimestamp(testTimestamp)
	r.AddAttributes(attribute.KeyValue{Key: "e"})

	got := emit(t, t.Context(), "carts", r)

	require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","scope":"carts","attributes":{"e":null}}`+"\n", got)
}

func TestJSONLogExporter_map_keys_are_sorted(t *testing.T) {
	var r log.Record
	r.SetTimestamp(testTimestamp)
	r.SetBody(attribute.MapValue(
		attribute.String("z", "1"),
		attribute.String("a", "2"),
		attribute.String("m", "3"),
	))

	got := emit(t, t.Context(), "carts", r)

	require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","body":{"a":"2","m":"3","z":"1"},"scope":"carts"}`+"\n", got)
}

func TestJSONLogExporter_duplicate_attribute_keys_are_each_written(t *testing.T) {
	var out bytes.Buffer
	lp := sdklog.NewLoggerProvider(
		sdklog.WithAllowKeyDuplication(),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(newJSONLogExporter(&out))),
	)
	defer func() { require.NoError(t, lp.Shutdown(context.Background())) }()

	var r log.Record
	r.SetTimestamp(testTimestamp)
	r.AddAttributes(attribute.String("k", "1"), attribute.String("k", "2"))
	lp.Logger("carts").Emit(t.Context(), r)

	require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","scope":"carts","attributes":{"k":"1","k":"2"}}`+"\n", out.String())
}

func TestJSONLogExporter_NaN(t *testing.T) {
	var r log.Record
	r.SetTimestamp(testTimestamp)
	r.SetBody(attribute.Float64Value(math.Inf(-1)))
	r.AddAttributes(
		attribute.Float64("nan", math.NaN()),
		attribute.Float64("inf", math.Inf(1)),
		attribute.Float64Slice("nans", []float64{math.NaN(), 1}),
		attribute.Map("m", attribute.Float64("nan", math.NaN())),
	)

	got := emit(t, t.Context(), "carts", r)

	require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","body":"-Inf","scope":"carts","attributes":{"nan":"NaN","inf":"+Inf","nans":["NaN",1],"m":{"nan":"NaN"}}}`+"\n", got)
	require.True(t, json.Valid([]byte(strings.TrimSuffix(got, "\n"))))
}

func TestJSONLogExporter_strings_are_escaped(t *testing.T) {
	var r log.Record
	r.SetTimestamp(testTimestamp)
	r.SetBody(attribute.StringValue("say \"hi\"\\\n\r\t\x00\x1f <b>&é\xff"))
	r.AddAttributes(attribute.String("k\"ey", "v"))

	got := emit(t, t.Context(), "sc\"ope", r)

	require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","body":"say \"hi\"\\\n\r\t\u0000\u001f <b>&é�","scope":"sc\"ope","attributes":{"k\"ey":"v"}}`+"\n", got)

	var decoded struct {
		Body string `json:"body"`
	}
	require.NoError(t, json.Unmarshal([]byte(got), &decoded))
	require.Equal(t, "say \"hi\"\\\n\r\t\x00\x1f <b>&é�", decoded.Body)
}

func TestJSONLogExporter_Export(t *testing.T) {
	t.Run("writes each record of a batch as its own line", func(t *testing.T) {
		var a, b sdklog.Record
		a.SetTimestamp(testTimestamp)
		a.SetBody(attribute.StringValue("a"))
		b.SetTimestamp(testTimestamp)
		b.SetBody(attribute.StringValue("b"))

		var out bytes.Buffer
		require.NoError(t, newJSONLogExporter(&out).Export(t.Context(), []sdklog.Record{a, b}))

		require.Equal(t,
			`{"timestamp":"2026-10-16T06:56:32.447829601Z","body":"a","scope":""}`+"\n"+
				`{"timestamp":"2026-10-16T06:56:32.447829601Z","body":"b","scope":""}`+"\n",
			out.String())
	})

	t.Run("writes nothing for no records", func(t *testing.T) {
		w := &countingWriter{}
		require.NoError(t, newJSONLogExporter(w).Export(t.Context(), nil))
		require.Zero(t, w.writes)
	})

	t.Run("returns the writer's error", func(t *testing.T) {
		errWrite := errors.New("disk full")
		var r sdklog.Record
		err := newJSONLogExporter(errWriter{errWrite}).Export(t.Context(), []sdklog.Record{r})
		require.ErrorIs(t, err, errWrite)
	})

	t.Run("writes a record emitted with a cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var r log.Record
		r.SetTimestamp(testTimestamp)
		r.SetSeverity(log.SeverityError)
		r.SetBody(attribute.StringValue("client went away"))

		got := emit(t, ctx, "carts", r)

		require.Equal(t, `{"timestamp":"2026-10-16T06:56:32.447829601Z","severity_text":"ERROR","severity_number":17,"body":"client went away","scope":"carts"}`+"\n", got)
	})

	t.Run("keeps a small buffer and drops a large one", func(t *testing.T) {
		e := newJSONLogExporter(&countingWriter{})

		var small sdklog.Record
		small.SetBody(attribute.StringValue("small"))
		require.NoError(t, e.Export(t.Context(), []sdklog.Record{small}))
		require.NotNil(t, e.buf)

		var large sdklog.Record
		large.SetBody(attribute.StringValue(strings.Repeat("x", maxRetainedBuf)))
		require.NoError(t, e.Export(t.Context(), []sdklog.Record{large}))
		require.Nil(t, e.buf)
	})
}

func TestJSONLogExporter_ForceFlush_and_Shutdown_return_nil(t *testing.T) {
	e := newJSONLogExporter(&bytes.Buffer{})
	require.NoError(t, e.ForceFlush(t.Context()))
	require.NoError(t, e.Shutdown(t.Context()))
}

// TestJSONLogExporter_concurrent_Export calls Export concurrently on one
// exporter, writing to a bytes.Buffer, which is not safe for concurrent use:
// run with -race, it fails if two writes ever overlap.
func TestJSONLogExporter_concurrent_Export(t *testing.T) {
	const (
		goroutines = 16
		perG       = 50
	)
	var out bytes.Buffer
	e := newJSONLogExporter(&out)

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range perG {
				var a, b sdklog.Record
				a.SetTimestamp(testTimestamp)
				a.SetBody(attribute.StringValue(fmt.Sprintf("g%d-%d-a %s", g, i, strings.Repeat("x", 512))))
				b.SetTimestamp(testTimestamp)
				b.SetBody(attribute.StringValue(fmt.Sprintf("g%d-%d-b", g, i)))
				if err := e.Export(context.Background(), []sdklog.Record{a, b}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()

	requireValidLines(t, out.String())
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	require.Len(t, lines, goroutines*perG*2)

	seen := make(map[string]bool, len(lines))
	for i, line := range lines {
		var rec struct {
			Body string `json:"body"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		seen[rec.Body] = true
		// A batch's records are written together, so each a is followed by
		// its own b.
		if strings.HasSuffix(strings.Fields(rec.Body)[0], "-a") {
			require.Less(t, i+1, len(lines))
			require.Contains(t, lines[i+1], `"body":"`+strings.TrimSuffix(strings.Fields(rec.Body)[0], "-a")+`-b"`)
		}
	}
	require.Len(t, seen, goroutines*perG*2)
}

type countingWriter struct{ writes int }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return len(p), nil
}

type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }
