// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package ui_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/example/http/polls/store/storetest"
	"github.com/z5labs/humus/humustest"
)

type subscriptions struct {
	*storetest.Store
	mu   sync.Mutex
	open map[uuid.UUID]int
}

func watch(s *storetest.Store) *subscriptions {
	return &subscriptions{Store: s, open: make(map[uuid.UUID]int)}
}

func (s *subscriptions) Subscribe(pollID uuid.UUID) (<-chan store.Poll, func()) {
	results, unsubscribe := s.Store.Subscribe(pollID)
	s.mu.Lock()
	s.open[pollID]++
	s.mu.Unlock()
	var once sync.Once
	return results, func() {
		once.Do(func() {
			s.mu.Lock()
			s.open[pollID]--
			s.mu.Unlock()
			unsubscribe()
		})
	}
}

func (s *subscriptions) count(pollID uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open[pollID]
}

type feed struct {
	header http.Header
	events <-chan string
	ended  <-chan feedEnd
}

type feedEnd struct {
	err error
	at  time.Time
}

func (p poll) feedURL() string {
	return base + "/polls/" + p.id.String() + "/feed"
}

func open(t *testing.T, ctx context.Context, c *http.Client, target string) feed {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	events := make(chan string, 16)
	ended := make(chan feedEnd, 1)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		r := bufio.NewReader(resp.Body)
		var block []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				if errors.Is(err, io.EOF) && line == "" && len(block) == 0 {
					err = nil
				}
				ended <- feedEnd{err: err, at: time.Now()}
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case strings.HasPrefix(line, ":"):
				events <- line
			case line != "":
				block = append(block, line)
			default:
				events <- strings.Join(block, "\n")
				block = nil
			}
		}
	}()
	return feed{header: resp.Header, events: events, ended: ended}
}

func results(counts ...string) string {
	lines := []string{"event: results"}
	for _, c := range counts {
		text, votes, _ := strings.Cut(c, "=")
		lines = append(lines, `data: <li><span class="option">`+text+`</span> <span class="votes">`+votes+`</span></li>`)
	}
	return strings.Join(lines, "\n")
}

func TestSite_a_polls_feed_opens_with_its_current_results(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		p := newPoll(t, polls, "Tabs or <spaces>?", "Tabs", "<Spaces>", "Both")
		for voter, option := range map[string]int{"alice": 0, "bob": 0, "carol": 2} {
			require.NoError(t, polls.Vote(t.Context(), p.id, p.options[option], voter))
		}
		c, stop := start(t, polls)

		page := do(t, c, http.MethodGet, base+"/polls/"+p.id.String())
		require.Contains(t, page.body, `<div hx-ext="sse" sse-connect="/polls/`+p.id.String()+`/feed">`+"\n"+`<div id="results">`+"\n"+`<ul id="options" sse-swap="results">`,
			"the page connects to its poll's feed, and swaps each results event into its options")

		f := open(t, t.Context(), c, p.feedURL())
		require.Equal(t, "text/event-stream", f.header.Get("Content-Type"))
		require.Equal(t, "no-store", f.header.Get("Cache-Control"))
		require.Equal(t, csp, f.header.Get("Content-Security-Policy"))
		require.Equal(t, results("Tabs=2", "&lt;Spaces&gt;=0", "Both=1"), <-f.events)

		head := do(t, c, http.MethodHead, p.feedURL())
		require.Equal(t, http.StatusOK, head.status)
		require.Equal(t, "text/event-stream", head.header.Get("Content-Type"))
		require.Empty(t, head.body)

		stop()
	})
}

func TestSite_a_vote_on_any_replica_sends_the_polls_new_results_and_a_vote_on_another_poll_sends_nothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		p := newPoll(t, polls, "Tabs or spaces?", "Tabs", "Spaces")
		other := newPoll(t, polls, "Vim or Emacs?", "Vim", "Emacs")
		watching, stopWatching := start(t, polls)
		voting, stopVoting := start(t, polls)

		f := open(t, t.Context(), watching, p.feedURL())
		require.Equal(t, results("Tabs=0", "Spaces=0"), <-f.events)

		voter := browser(t, voting)
		require.Equal(t, http.StatusOK, vote(t, voter, p, choose(p.options[1]), sameOrigin).status)
		require.Equal(t, results("Tabs=0", "Spaces=1"), <-f.events, "a vote through another replica")
		require.Equal(t, http.StatusOK, vote(t, browser(t, watching), p, choose(p.options[0]), sameOrigin).status)
		require.Equal(t, results("Tabs=1", "Spaces=1"), <-f.events, "a vote through its own replica")

		require.Equal(t, http.StatusOK, vote(t, browser(t, voting), other, choose(other.options[0]), sameOrigin).status)
		synctest.Wait()
		require.Empty(t, f.events, "a vote on another poll sends nothing")
		require.Equal(t, http.StatusConflict, vote(t, voter, p, choose(p.options[0]), sameOrigin).status)
		synctest.Wait()
		require.Empty(t, f.events, "a refused vote sends nothing")

		stopVoting()
		stopWatching()
	})
}

func TestSite_an_idle_feed_sends_a_keep_alive_every_15s(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		p := newPoll(t, polls, "Tabs or spaces?", "Tabs", "Spaces")
		c, stop := start(t, polls)

		f := open(t, t.Context(), c, p.feedURL())
		opened := time.Now()
		require.Equal(t, results("Tabs=0", "Spaces=0"), <-f.events)
		require.Equal(t, ": keep-alive", <-f.events)
		require.Equal(t, 15*time.Second, time.Since(opened), "15s after the feed opened, past the 10s WriteTimeout")
		require.Equal(t, ": keep-alive", <-f.events)
		require.Equal(t, 30*time.Second, time.Since(opened))

		time.Sleep(5 * time.Second)
		require.NoError(t, polls.Vote(t.Context(), p.id, p.options[0], "alice"))
		voted := time.Now()
		require.Equal(t, results("Tabs=1", "Spaces=0"), <-f.events)
		require.Equal(t, ": keep-alive", <-f.events)
		require.Equal(t, 15*time.Second, time.Since(voted), "15s after the last event, while no vote arrives")

		stop()
	})
}

func TestSite_a_feed_whose_client_goes_away_ends_and_is_unsubscribed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := watch(storetest.New())
		p := newPoll(t, polls.Store, "Tabs or spaces?", "Tabs", "Spaces")
		tel := humustest.NewTelemetry()
		c, stop := start(t, polls, humus.WithTelemetry(tel.Builder()))

		ctx, leave := context.WithCancel(t.Context())
		f := open(t, ctx, c, p.feedURL())
		require.Equal(t, results("Tabs=0", "Spaces=0"), <-f.events)
		require.Equal(t, 1, polls.count(p.id))

		leave()
		<-f.ended
		synctest.Wait()
		require.Zero(t, polls.count(p.id), "the feed unsubscribed")
		require.NoError(t, polls.Vote(t.Context(), p.id, p.options[0], "alice"), "a vote after it left goes nowhere")

		stop()
		requireCleanFeeds(t, tel)
	})
}

func TestSite_an_open_feed_ends_cleanly_when_Shutdown_begins_and_is_unsubscribed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := watch(storetest.New())
		p := newPoll(t, polls.Store, "Tabs or spaces?", "Tabs", "Spaces")
		tel := humustest.NewTelemetry()
		c, stop := start(t, polls, humus.WithTelemetry(tel.Builder()))

		f := open(t, t.Context(), c, p.feedURL())
		require.Equal(t, results("Tabs=0", "Spaces=0"), <-f.events)

		sigterm := time.Now()
		stop()
		end := <-f.ended
		require.NoError(t, end.err, "a clean end, which htmx's SSE extension reconnects after")
		require.Equal(t, 5*time.Second, end.at.Sub(sigterm), "the feed ends as Shutdown begins, after the drain, not at the end of the grace period")
		require.Zero(t, polls.count(p.id), "the feed unsubscribed")
		requireCleanFeeds(t, tel)
	})
}

func requireCleanFeeds(t *testing.T, tel *humustest.Telemetry) {
	t.Helper()
	feeds := 0
	for _, s := range tel.Spans() {
		if s.Name() != "GET /polls/{pollId}/feed" {
			continue
		}
		feeds++
		require.NotEqual(t, codes.Error, s.Status().Code, "the feed returned no error")
		require.Empty(t, s.Events(), "the feed returned no error")
	}
	require.Equal(t, 1, feeds)
	for _, r := range tel.Logs() {
		require.Less(t, r.Severity(), log.SeverityError, "%v", r.Body())
	}
}

func TestSite_an_unknown_polls_feed_is_the_404_page(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := watch(storetest.New())
		c, stop := start(t, polls)

		unknown := uuid.New()
		for _, target := range []string{
			"/polls/" + unknown.String() + "/feed",
			"/polls/not-a-poll/feed",
		} {
			r := do(t, c, http.MethodGet, base+target)
			require.Equal(t, http.StatusNotFound, r.status, target)
			require.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"), target)
			require.Empty(t, r.header.Get("Cache-Control"), target)
			require.Contains(t, r.body, "<h1>Not Found</h1>", target)
			require.Contains(t, r.body, "<p>There&#39;s no such page.</p>", target)
		}
		require.Zero(t, polls.count(unknown), "the feed of an unknown poll unsubscribed")

		stop()
	})
}
