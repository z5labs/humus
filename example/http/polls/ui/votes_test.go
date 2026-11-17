// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package ui_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/polls/store/storetest"
)

var voterID = regexp.MustCompile(`^[A-Z2-7]{26}$`)

type voting struct {
	*storetest.Store
	mu     sync.Mutex
	voters []string
}

func (v *voting) Vote(ctx context.Context, pollID uuid.UUID, optionID int64, voter string) error {
	v.mu.Lock()
	v.voters = append(v.voters, voter)
	v.mu.Unlock()
	return v.Store.Vote(ctx, pollID, optionID, voter)
}

func (v *voting) calls() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.voters...)
}

type poll struct {
	id      uuid.UUID
	options []int64
}

func newPoll(t *testing.T, polls *storetest.Store, question string, options ...string) poll {
	t.Helper()
	id, err := polls.CreatePoll(t.Context(), question, options)
	require.NoError(t, err)
	p, err := polls.Poll(t.Context(), id)
	require.NoError(t, err)
	got := poll{id: id}
	for _, o := range p.Options {
		got.options = append(got.options, o.ID)
	}
	return got
}

func (p poll) votesURL() string {
	return base + "/polls/" + p.id.String() + "/votes"
}

func counts(t *testing.T, polls *storetest.Store, id uuid.UUID) []int64 {
	t.Helper()
	p, err := polls.Poll(t.Context(), id)
	require.NoError(t, err)
	var got []int64
	for _, o := range p.Options {
		got = append(got, o.Votes)
	}
	return got
}

func browser(t *testing.T, c *http.Client) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	b := *c
	b.Jar = jar
	return &b
}

var sameOrigin = map[string]string{"HX-Request": "true", "Sec-Fetch-Site": "same-origin", "Origin": base}

func vote(t *testing.T, c *http.Client, p poll, form url.Values, header map[string]string) response {
	t.Helper()
	return send(t, c, http.MethodPost, p.votesURL(), "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), header)
}

func choose(option int64) url.Values {
	return url.Values{"option": {strconv.FormatInt(option, 10)}}
}

func voterCookie(t *testing.T, r response) *http.Cookie {
	t.Helper()
	values := r.header.Values("Set-Cookie")
	require.Len(t, values, 1, "one cookie is set")
	c, err := http.ParseSetCookie(values[0])
	require.NoError(t, err)
	return c
}

func TestSite_gives_a_browser_without_one_a___Host_voter_cookie(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := &voting{Store: storetest.New()}
		p := newPoll(t, polls.Store, "Tabs or spaces?", "Tabs", "Spaces")
		c, stop := start(t, polls)

		page := do(t, c, http.MethodGet, base+"/polls/"+p.id.String())
		require.Equal(t, http.StatusOK, page.status)
		raw := page.header.Get("Set-Cookie")
		require.NotContains(t, strings.ToLower(raw), "domain=", "the cookie has no Domain")
		cookie := voterCookie(t, page)
		require.Equal(t, "__Host-voter", cookie.Name)
		require.Regexp(t, voterID, cookie.Value)
		require.True(t, cookie.Secure)
		require.True(t, cookie.HttpOnly)
		require.Equal(t, "/", cookie.Path)
		require.Empty(t, cookie.Domain)
		require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
		require.Equal(t, 400*24*60*60, cookie.MaxAge)

		other := voterCookie(t, do(t, c, http.MethodGet, base+"/polls/"+p.id.String()))
		require.NotEqual(t, cookie.Value, other.Value, "each browser gets its own voter ID")

		b := browser(t, c)
		first := do(t, b, http.MethodGet, base+"/polls/"+p.id.String())
		given := voterCookie(t, first)
		again := do(t, b, http.MethodGet, base+"/polls/"+p.id.String())
		require.Empty(t, again.header.Values("Set-Cookie"), "a browser with the cookie keeps it")
		voted := vote(t, b, p, choose(p.options[0]), sameOrigin)
		require.Equal(t, http.StatusOK, voted.status)
		require.Empty(t, voted.header.Values("Set-Cookie"))
		require.Equal(t, []string{given.Value}, polls.calls(), "the vote is the cookie's voter's")

		fresh := vote(t, c, p, choose(p.options[1]), sameOrigin)
		require.Equal(t, http.StatusOK, fresh.status)
		byVote := voterCookie(t, fresh)
		require.Equal(t, "__Host-voter", byVote.Name)
		require.Regexp(t, voterID, byVote.Value)
		require.Equal(t, []string{given.Value, byVote.Value}, polls.calls(), "a vote with no cookie is counted under the one it gets")

		forged := browser(t, c)
		u, err := url.Parse(base)
		require.NoError(t, err)
		forged.Jar.SetCookies(u, []*http.Cookie{{Name: "__Host-voter", Value: "not-a-voter-id", Path: "/", Secure: true}})
		replaced := voterCookie(t, do(t, forged, http.MethodGet, base+"/polls/"+p.id.String()))
		require.Regexp(t, voterID, replaced.Value, "a cookie that isn't a voter ID is replaced")

		stop()
	})
}

func TestSite_a_vote_counts_and_answers_with_the_polls_results(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		p := newPoll(t, polls, "Tabs or spaces?", "Tabs", "Spaces")
		c, stop := start(t, polls)
		b := browser(t, c)

		page := do(t, b, http.MethodGet, base+"/polls/"+p.id.String())
		action := "/polls/" + p.id.String() + "/votes"
		require.Contains(t, page.body, `<form method="post" action="`+action+`" hx-post="`+action+`" hx-target="#results" hx-swap="outerHTML">`+"\n"+
			`<button type="submit" name="option" value="`+strconv.FormatInt(p.options[0], 10)+`">Tabs</button>`+"\n"+
			`<button type="submit" name="option" value="`+strconv.FormatInt(p.options[1], 10)+`">Spaces</button>`+"\n</form>")

		fragment := vote(t, b, p, choose(p.options[1]), sameOrigin)
		require.Equal(t, http.StatusOK, fragment.status)
		require.Equal(t, "text/html; charset=utf-8", fragment.header.Get("Content-Type"))
		require.Equal(t, csp, fragment.header.Get("Content-Security-Policy"))
		require.Equal(t, []string{"HX-Request"}, fragment.header.Values("Vary"))
		require.Equal(t, "<div id=\"results\">\n<ul id=\"options\">\n"+
			"<li><span class=\"option\">Tabs</span> <span class=\"votes\">0</span></li>\n"+
			"<li><span class=\"option\">Spaces</span> <span class=\"votes\">1</span></li>\n</ul>\n"+
			"<p class=\"note\">Your vote is counted.</p>\n</div>", fragment.body)
		require.Equal(t, []int64{0, 1}, counts(t, polls, p.id))

		withoutHTMX := vote(t, browser(t, c), p, choose(p.options[0]), map[string]string{"Sec-Fetch-Site": "same-origin"})
		require.Equal(t, http.StatusOK, withoutHTMX.status)
		require.Equal(t, []string{"HX-Request"}, withoutHTMX.header.Values("Vary"))
		require.Contains(t, withoutHTMX.body, "<!DOCTYPE html>")
		require.Contains(t, withoutHTMX.body, "<h1>Tabs or spaces?</h1>")
		require.Contains(t, withoutHTMX.body, "<p class=\"note\">Your vote is counted.</p>")
		require.Equal(t, []int64{1, 1}, counts(t, polls, p.id))

		stop()
	})
}

func TestSite_a_second_vote_from_one_browser_counts_nothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		p := newPoll(t, polls, "Tabs or spaces?", "Tabs", "Spaces")
		c, stop := start(t, polls)
		b := browser(t, c)
		do(t, b, http.MethodGet, base+"/polls/"+p.id.String())

		require.Equal(t, http.StatusOK, vote(t, b, p, choose(p.options[0]), sameOrigin).status)
		for _, option := range p.options {
			again := vote(t, b, p, choose(option), sameOrigin)
			require.Equal(t, http.StatusConflict, again.status)
			require.Equal(t, "text/html; charset=utf-8", again.header.Get("Content-Type"))
			require.Contains(t, again.body, "<p class=\"note\">This browser has voted on this poll already.</p>")
			require.NotContains(t, again.body, "<form", "there's nothing left to vote with")
			require.NotContains(t, again.body, "<html")
		}
		require.Equal(t, []int64{1, 0}, counts(t, polls, p.id))

		together := browser(t, c)
		do(t, together, http.MethodGet, base+"/polls/"+p.id.String())
		statuses := make([]int, 2)
		var wg sync.WaitGroup
		for i := range statuses {
			wg.Go(func() {
				statuses[i] = vote(t, together, p, choose(p.options[1]), sameOrigin).status
			})
		}
		wg.Wait()
		require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses, "two votes sent at once count once")
		require.Equal(t, []int64{1, 1}, counts(t, polls, p.id))

		stop()
	})
}

func TestSite_another_browsers_vote_counts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		p := newPoll(t, polls, "Tabs or spaces?", "Tabs", "Spaces")
		other := newPoll(t, polls, "Vim or Emacs?", "Vim", "Emacs")
		c, stop := start(t, polls)

		first := browser(t, c)
		second := browser(t, c)
		require.Equal(t, http.StatusOK, vote(t, first, p, choose(p.options[0]), sameOrigin).status)
		require.Equal(t, http.StatusOK, vote(t, second, p, choose(p.options[0]), sameOrigin).status)
		require.Equal(t, []int64{2, 0}, counts(t, polls, p.id))

		require.Equal(t, http.StatusOK, vote(t, first, other, choose(other.options[1]), sameOrigin).status, "a browser votes once on each poll")
		require.Equal(t, []int64{0, 1}, counts(t, polls, other.id))

		stop()
	})
}

func TestSite_a_cross_site_vote_is_the_403_page_and_counts_nothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		p := newPoll(t, polls, "Tabs or spaces?", "Tabs", "Spaces")
		c, stop := start(t, polls)
		b := browser(t, c)
		do(t, b, http.MethodGet, base+"/polls/"+p.id.String())

		for _, header := range []map[string]string{
			{"Sec-Fetch-Site": "cross-site"},
			{"Sec-Fetch-Site": "same-site"},
			{"Sec-Fetch-Site": "cross-site", "HX-Request": "true", "Origin": "https://attacker.example"},
			{"Origin": "https://attacker.example"},
		} {
			r := vote(t, b, p, choose(p.options[0]), header)
			require.Equal(t, http.StatusForbidden, r.status, header)
			require.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"), header)
			require.Equal(t, csp, r.header.Get("Content-Security-Policy"), header)
			require.Contains(t, r.body, "<h1>Forbidden</h1>", header)
			require.Contains(t, r.body, "<p>Only this site&#39;s pages can send that.</p>", header)
		}
		require.Equal(t, []int64{0, 0}, counts(t, polls, p.id))

		for _, header := range []map[string]string{sameOrigin, {"Origin": base}} {
			require.Equal(t, http.StatusOK, vote(t, browser(t, c), p, choose(p.options[0]), header).status, header)
		}
		require.Equal(t, []int64{2, 0}, counts(t, polls, p.id))

		stop()
	})
}

func TestSite_a_vote_for_no_option_of_the_poll_counts_nothing_and_is_a_4xx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		p := newPoll(t, polls, "Tabs or spaces?", "Tabs", "Spaces")
		other := newPoll(t, polls, "Vim or Emacs?", "Vim", "Emacs")
		c, stop := start(t, polls)
		b := browser(t, c)
		do(t, b, http.MethodGet, base+"/polls/"+p.id.String())

		for name, form := range map[string]url.Values{
			"no option":              {},
			"an empty option":        {"option": {""}},
			"an option not a number": {"option": {"Tabs"}},
			"an option sent twice":   {"option": {strconv.FormatInt(p.options[0], 10), strconv.FormatInt(p.options[1], 10)}},
			"another poll's option":  choose(other.options[0]),
			"an option of no poll":   choose(p.options[0] + 1000),
		} {
			for _, header := range []map[string]string{sameOrigin, {"Sec-Fetch-Site": "same-origin"}} {
				r := vote(t, b, p, form, header)
				require.Equal(t, http.StatusUnprocessableEntity, r.status, name)
				require.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"), name)
				require.Contains(t, r.body, "<p class=\"note\">Choose one of the poll&#39;s options.</p>", name)
				require.Contains(t, r.body, `<form method="post" action="/polls/`+p.id.String()+`/votes"`, "%s: the form is shown again", name)
			}
		}
		require.Equal(t, []int64{0, 0}, counts(t, polls, p.id))
		require.Equal(t, []int64{0, 0}, counts(t, polls, other.id))

		require.Equal(t, http.StatusOK, vote(t, b, p, choose(p.options[0]), sameOrigin).status, "the browser still has its vote")

		for _, target := range []string{
			base + "/polls/" + uuid.NewString() + "/votes",
			base + "/polls/not-a-poll/votes",
		} {
			for _, form := range []url.Values{choose(p.options[1]), {}} {
				r := send(t, b, http.MethodPost, target, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), sameOrigin)
				require.Equal(t, http.StatusNotFound, r.status, target)
				require.Contains(t, r.body, "<h1>Not Found</h1>", target)
				require.Contains(t, r.body, "<p>There&#39;s no such page.</p>", target)
			}
		}
		require.Equal(t, []int64{1, 0}, counts(t, polls, p.id))

		stop()
	})
}

func TestSite_a_GET_of_the_votes_path_is_the_405_page(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		p := newPoll(t, polls, "Tabs or spaces?", "Tabs", "Spaces")
		c, stop := start(t, polls)

		r := do(t, c, http.MethodGet, p.votesURL())
		require.Equal(t, http.StatusMethodNotAllowed, r.status)
		require.Equal(t, "POST", r.header.Get("Allow"))
		require.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"))
		require.Contains(t, r.body, "<h1>Method Not Allowed</h1>")
		require.Contains(t, r.body, "<p>This page can&#39;t do that.</p>")
		require.Equal(t, []int64{0, 0}, counts(t, polls, p.id))

		stop()
	})
}
