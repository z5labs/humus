// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/httpserver/httpservertest"
)

func rehearsing(t *testing.T, header map[string]string, rules ...fit.Rule) map[string]string {
	t.Helper()
	codec, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	h := maps.Clone(header)
	if h == nil {
		h = make(map[string]string)
	}
	h["Baggage"] = b.String()
	return h
}

var (
	abortQueries = fit.Rule{Point: store.Point, Fault: fit.Fault{Abort: true}}
	slowQueries  = fit.Rule{Point: store.Point, Fault: fit.Fault{Delay: time.Hour}}
)

func (s *service) trusted(t *testing.T) *http.Client {
	t.Helper()
	return browser(t, withCertificate(httpservertest.NewClient(s.f.h, s.f.ca.Pool()), s.f.trust.Issue(t, "cases")))
}

func TestInit_serves_the_503_page_and_counts_nothing_when_a_trusted_scenario_aborts_the_database(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase()
		p := newPoll(t, db.Store, "Tabs or spaces?", "Tabs", "Spaces")
		f := newFixture(t)
		f.env["FAULT_TRUST_CA_FILE"] = "fault/ca.crt"
		s := f.serve(t, db.init)
		c := s.trusted(t)

		page := send(t, c, http.MethodGet, p.url(""), nil, rehearsing(t, nil, abortQueries))
		require.Equal(t, http.StatusServiceUnavailable, page.status, "%s", page.body)
		require.Contains(t, page.body, "The polls can&#39;t be reached right now. Try again in a moment.")

		vote := url.Values{"option": {strconv.FormatInt(p.options[0], 10)}}
		voted := send(t, c, http.MethodPost, p.url("/votes"), vote, rehearsing(t, sameOrigin, abortQueries))
		require.Equal(t, http.StatusServiceUnavailable, voted.status, "%s", voted.body)
		require.Contains(t, voted.body, "The polls can&#39;t be reached right now. Try again in a moment.")
		require.NotContains(t, voted.body, "Your vote is counted.")
		require.Equal(t, []int64{0, 0}, counts(t, db.Store, p.id))

		voted = send(t, c, http.MethodPost, p.url("/votes"), vote, sameOrigin)
		require.Equal(t, http.StatusOK, voted.status, "%s", voted.body)
		require.Equal(t, []int64{1, 0}, counts(t, db.Store, p.id))

		s.stop(t)
	})
}

func TestInit_slows_each_query_by_a_trusted_delay_of_at_most_FAULT_MAX_DELAY(t *testing.T) {
	for _, tc := range []struct {
		maxDelay string
		want     time.Duration
	}{
		{maxDelay: "", want: 10 * time.Second},
		{maxDelay: "3s", want: 3 * time.Second},
	} {
		t.Run("FAULT_MAX_DELAY="+tc.maxDelay, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				db := newFakeDatabase()
				p := newPoll(t, db.Store, "Tabs or spaces?", "Tabs", "Spaces")
				f := newFixture(t)
				f.env["FAULT_TRUST_CA_FILE"] = "fault/ca.crt"
				f.env["WRITE_TIMEOUT"] = "30s"
				if tc.maxDelay != "" {
					f.env["FAULT_MAX_DELAY"] = tc.maxDelay
				}
				s := f.serve(t, db.init)

				sent := time.Now()
				page := send(t, s.trusted(t), http.MethodGet, p.url(""), nil, rehearsing(t, nil, slowQueries))
				require.Equal(t, http.StatusOK, page.status, "%s", page.body)
				require.Equal(t, tc.want, time.Since(sent), "the page's one query waits out the delay, clamped")

				s.stop(t)
			})
		})
	}
}

func TestInit_without_a_FAULT_TRUST_CA_FILE_trusts_no_scenario_even_from_the_cases_certificate(t *testing.T) {
	for name, set := range map[string]func(map[string]string){
		"unset": func(map[string]string) {},
		"empty": func(env map[string]string) { env["FAULT_TRUST_CA_FILE"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				db := newFakeDatabase()
				p := newPoll(t, db.Store, "Tabs or spaces?", "Tabs", "Spaces")
				f := newFixture(t)
				set(f.env)
				s := f.serve(t, db.init)
				c := s.trusted(t)

				page := send(t, c, http.MethodGet, p.url(""), nil, rehearsing(t, nil, abortQueries))
				require.Equal(t, http.StatusOK, page.status, "%s", page.body)

				voted := send(t, c, http.MethodPost, p.url("/votes"), url.Values{"option": {strconv.FormatInt(p.options[0], 10)}}, rehearsing(t, sameOrigin, abortQueries))
				require.Equal(t, http.StatusOK, voted.status, "%s", voted.body)
				require.Contains(t, voted.body, "Your vote is counted.")
				require.Equal(t, []int64{1, 0}, counts(t, db.Store, p.id))

				s.stop(t)
			})
		})
	}
}
