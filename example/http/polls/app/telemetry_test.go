// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
)

const (
	voteRoute = "POST /polls/{pollId}/votes"
	feedRoute = "GET /polls/{pollId}/feed"
)

func observe(t *testing.T, init func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error)) (*service, *humustest.Telemetry) {
	t.Helper()
	tel := humustest.NewTelemetry()
	f := newFixture(t)
	f.telemetry = tel.Builder()
	return f.serve(t, init), tel
}

func named(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var got []sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.SpanKind() == trace.SpanKindServer && span.Name() == name {
			got = append(got, span)
		}
	}
	return got
}

func only(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	got := named(spans, name)
	require.Len(t, got, 1, "the server spans named %q", name)
	return got[0]
}

func inTrace(spans []sdktrace.ReadOnlySpan, id trace.TraceID) []sdktrace.ReadOnlySpan {
	var got []sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.SpanContext().TraceID() == id {
			got = append(got, span)
		}
	}
	return got
}

func errorRecords(records []sdklog.Record) []sdklog.Record {
	var got []sdklog.Record
	for _, r := range records {
		if r.Severity() >= log.SeverityError {
			got = append(got, r)
		}
	}
	return got
}

func vote(t *testing.T, c *http.Client, p poll, option string) response {
	t.Helper()
	return send(t, c, http.MethodPost, p.url("/votes"), url.Values{"option": {option}}, sameOrigin)
}

func TestInit_names_a_votes_server_span_after_its_route(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase()
		p := newPoll(t, db.Store, "Tabs or spaces?", "Tabs", "Spaces")
		s, tel := observe(t, db.init)

		r := vote(t, browser(t, s.client), p, strconv.FormatInt(p.options[0], 10))
		require.Equal(t, http.StatusOK, r.status, "%s", r.body)
		s.stop(t)

		span := only(t, tel.Spans(), voteRoute)
		require.Equal(t, codes.Unset, span.Status().Code)
	})
}

func TestInit_traces_a_feed_as_one_span_named_after_its_route_that_ends_with_the_feed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase()
		p := newPoll(t, db.Store, "Tabs or spaces?", "Tabs", "Spaces")
		s, tel := observe(t, db.init)

		fd := open(t, s.client, p.url("/feed"))
		require.Equal(t, results("Tabs=0", "Spaces=0"), <-fd.events)
		require.Equal(t, http.StatusOK, vote(t, browser(t, s.client), p, strconv.FormatInt(p.options[1], 10)).status)
		require.Equal(t, results("Tabs=0", "Spaces=1"), <-fd.events)
		require.Empty(t, named(tel.Spans(), feedRoute), "an open feed's span hasn't ended")

		s.sigterm(t)
		end := <-fd.ended
		require.NoError(t, end.err)
		s.exited(t)

		spans := tel.Spans()
		feed := only(t, spans, feedRoute)
		require.Equal(t, end.at, feed.EndTime(), "the feed's span ends as the feed does")
		require.Len(t, inTrace(spans, feed.SpanContext().TraceID()), 1, "a feed is one span, whatever it sends")
	})
}

func routes(t *testing.T, rm metricdata.ResourceMetrics, name string) []string {
	t.Helper()
	var got []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "%s is a %T", name, m.Data)
			for _, dp := range h.DataPoints {
				route, ok := dp.Attributes.Value("http.route")
				require.True(t, ok, "a data point of %s has no http.route: %v", name, dp.Attributes.ToSlice())
				if !slices.Contains(got, route.AsString()) {
					got = append(got, route.AsString())
				}
			}
		}
	}
	slices.Sort(got)
	return got
}

func TestInit_measures_each_requests_duration_by_its_route(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase()
		p := newPoll(t, db.Store, "Tabs or spaces?", "Tabs", "Spaces")
		s, tel := observe(t, db.init)

		require.Equal(t, http.StatusOK, s.get(t, "/polls/"+p.id.String()).status)
		require.Equal(t, http.StatusOK, vote(t, browser(t, s.client), p, strconv.FormatInt(p.options[0], 10)).status)
		fd := open(t, s.client, p.url("/feed"))
		require.Equal(t, results("Tabs=1", "Spaces=0"), <-fd.events)
		require.Equal(t, http.StatusOK, s.get(t, "/health/liveness").status)
		s.stop(t)
		require.NoError(t, (<-fd.ended).err)

		rm, err := tel.Metrics(context.Background())
		require.NoError(t, err)
		require.Equal(t, []string{"/health/liveness", "/polls/{pollId}", "/polls/{pollId}/feed", "/polls/{pollId}/votes"}, routes(t, rm, "http.server.request.duration"))
	})
}

func unreachableDatabase(t *testing.T) (*service, *humustest.Telemetry) {
	t.Helper()
	s, tel := observe(t, Init)
	r := s.get(t, "/polls/"+uuid.NewString())
	require.Equal(t, http.StatusServiceUnavailable, r.status, "%s", r.body)
	return s, tel
}

func TestInit_records_a_vote_the_database_cant_take_as_one_ERROR_record_and_an_exception_on_its_span(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, tel := unreachableDatabase(t)
		p := poll{id: uuid.New()}
		r := vote(t, s.client, p, "1")
		require.Equal(t, http.StatusServiceUnavailable, r.status, "%s", r.body)
		s.stop(t)

		span := only(t, tel.Spans(), voteRoute)
		require.Equal(t, codes.Error, span.Status().Code)
		require.True(t, slices.ContainsFunc(span.Events(), func(ev sdktrace.Event) bool { return ev.Name == "exception" }), "the vote's span has no exception event: %v", span.Events())

		var records []sdklog.Record
		for _, rec := range errorRecords(tel.Logs()) {
			if rec.TraceID() == span.SpanContext().TraceID() {
				records = append(records, rec)
			}
		}
		require.Len(t, records, 1, "the vote's ERROR records")
		require.Equal(t, "request failed", records[0].Body().AsString())
		require.Equal(t, span.SpanContext().SpanID(), records[0].SpanID())
	})
}

func TestInit_records_no_error_for_a_request_the_client_got_wrong(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase()
		p := newPoll(t, db.Store, "Tabs or spaces?", "Tabs", "Spaces")
		s, tel := observe(t, db.init)
		c := browser(t, s.client)

		for _, tc := range []struct {
			status int
			got    response
		}{
			{http.StatusNotFound, s.get(t, "/polls/"+uuid.NewString())},
			{http.StatusNotFound, s.get(t, "/nowhere")},
			{http.StatusMethodNotAllowed, send(t, c, http.MethodDelete, p.url(""), nil, sameOrigin)},
			{http.StatusRequestEntityTooLarge, vote(t, c, p, strings.Repeat("1", 16<<10))},
			{http.StatusUnprocessableEntity, vote(t, c, p, "")},
			{http.StatusUnprocessableEntity, vote(t, c, p, "-1")},
		} {
			require.Equal(t, tc.status, tc.got.status, "%s", tc.got.body)
		}
		s.stop(t)

		spans := tel.Spans()
		require.NotEmpty(t, spans)
		for _, span := range spans {
			require.NotEqual(t, codes.Error, span.Status().Code, "the span %q", span.Name())
			for _, ev := range span.Events() {
				require.NotEqual(t, "exception", ev.Name, "the span %q", span.Name())
			}
		}
		require.Empty(t, errorRecords(tel.Logs()))
	})
}

func recorded(tel *humustest.Telemetry) []string {
	var out []string
	add := func(kvs []attribute.KeyValue) {
		for _, kv := range kvs {
			out = append(out, kv.Value.String())
		}
	}
	for _, span := range tel.Spans() {
		out = append(out, span.Name(), span.Status().Description)
		add(span.Attributes())
		for _, ev := range span.Events() {
			add(ev.Attributes)
		}
	}
	for _, r := range tel.Logs() {
		out = append(out, r.Body().String())
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			out = append(out, kv.Value.String())
			return true
		})
	}
	return out
}

func requireNoneHold(t *testing.T, recorded []string, secrets map[string]string) {
	t.Helper()
	require.NotEmpty(t, recorded)
	for _, v := range recorded {
		for name, secret := range secrets {
			require.NotContains(t, v, secret, "the %s was recorded", name)
		}
	}
}

func TestInit_puts_no_voter_cookie_on_a_span_or_a_log_record(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase()
		p := newPoll(t, db.Store, "Tabs or spaces?", "Tabs", "Spaces")
		s, tel := observe(t, db.init)
		c := browser(t, s.client)

		require.Equal(t, http.StatusOK, vote(t, c, p, strconv.FormatInt(p.options[0], 10)).status)
		require.Equal(t, http.StatusConflict, vote(t, c, p, strconv.FormatInt(p.options[1], 10)).status)
		require.Equal(t, http.StatusUnprocessableEntity, vote(t, c, p, "").status)
		require.Equal(t, http.StatusOK, send(t, c, http.MethodGet, p.url(""), nil, nil).status)
		u, err := url.Parse(base)
		require.NoError(t, err)
		var cookie string
		for _, ck := range c.Jar.Cookies(u) {
			if ck.Name == "__Host-voter" {
				cookie = ck.Value
			}
		}
		require.NotEmpty(t, cookie, "the browser has no voter cookie")
		s.stop(t)

		requireNoneHold(t, recorded(tel), map[string]string{"voter cookie": cookie})
	})
}

func TestInit_puts_no_voter_cookie_or_database_password_on_a_failed_votes_span_or_log_record(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, tel := unreachableDatabase(t)
		cookie := rand.Text()
		r := send(t, s.client, http.MethodPost, poll{id: uuid.New()}.url("/votes"), url.Values{"option": {"1"}}, map[string]string{
			"HX-Request":     "true",
			"Sec-Fetch-Site": "same-origin",
			"Origin":         base,
			"Cookie":         "__Host-voter=" + cookie,
		})
		require.Equal(t, http.StatusServiceUnavailable, r.status, "%s", r.body)
		s.stop(t)

		requireNoneHold(t, recorded(tel), map[string]string{"voter cookie": cookie, "database password": s.f.dbPassword})
	})
}

type resourceAttr struct {
	Key   string
	Value struct{ Value any }
}

func TestBuildTelemetry_names_the_service_polls_and_nothing_else(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"by default":           {env: map[string]string{}, want: "polls"},
		"unless it is renamed": {env: map[string]string{"OTEL_SERVICE_NAME": "polls-canary"}, want: "polls-canary"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_LOGS_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none"}
			maps.Copy(env, tc.env)
			var stdout bytes.Buffer
			sdk, err := BuildTelemetry(t.Context(), bedrock.Process{Environment: bedrock.NewStaticEnvironment(env), Stdout: &stdout}, nil)
			require.NoError(t, err)

			_, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "probe")
			span.End()
			require.NoError(t, sdk.Shutdown(context.Background()))

			var exported struct{ Resource []resourceAttr }
			require.NoError(t, json.NewDecoder(&stdout).Decode(&exported))
			require.Len(t, exported.Resource, 1, "%+v", exported.Resource)
			require.Equal(t, "service.name", exported.Resource[0].Key)
			require.Equal(t, tc.want, exported.Resource[0].Value.Value)
		})
	}
}

func TestInit_with_a_bad_telemetry_setting_exits_78(t *testing.T) {
	for name, value := range map[string]string{
		"OTEL_SDK_DISABLED":           "maybe",
		"OTEL_SERVICE_NAME":           "",
		"OTEL_TRACES_EXPORTER":        "zipkin",
		"OTEL_METRICS_EXPORTER":       "prometheus",
		"OTEL_LOGS_EXPORTER":          "file",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "collector",
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				f.env[name] = value
				_, status := httpservertest.Start(t, f.h.Host(), Init,
					humustest.WithApp(humus.WithTelemetry(BuildTelemetry)),
					humustest.WithEnv(f.env),
				)
				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
				require.ErrorContains(t, st.Cause(), "config "+name+":")
			})
		})
	}
}
