// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the polls service's black-box cases, run against a deployment of two or more of its replicas.
package cases

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	csp           = "default-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	htmlType      = "text/html; charset=utf-8"
	voterCookie   = "__Host-voter"
	voterMaxAge   = 400 * 24 * 60 * 60
	elsewhere     = "https://elsewhere.example"
	counted       = "Your vote is counted."
	alreadyVoted  = "This browser has voted on this poll already."
	forbidden     = "Only this site's pages can send that."
	notFound      = "There's no such page."
	notAllowed    = "This page can't do that."
	resultsEvent  = "results"
	newPollForm   = "new-poll"
	resultsTarget = "results"
)

var (
	optionPattern  = regexp.MustCompile(`<li><span class="option">([^<]*)</span> <span class="votes">(\d+)</span></li>`)
	buttonPattern  = regexp.MustCompile(`<button type="submit" name="option" value="(\d+)">([^<]*)</button>`)
	notePattern    = regexp.MustCompile(`<p class="note">([^<]*)</p>`)
	headingPattern = regexp.MustCompile(`<h1>([^<]*)</h1>`)
	fieldPattern   = regexp.MustCompile(`<input name="([^"]+)" value="([^"]*)"[^>]*></label>(?: <span class="problem">([^<]*)</span>)?</p>`)
	errorPattern   = regexp.MustCompile(`<h1>([^<]*)</h1>\n<p>([^<]*)</p>`)
	scriptPattern  = regexp.MustCompile(`<script src="([^"]+)"></script>`)
	voterPattern   = regexp.MustCompile(`^[A-Z2-7]{26}$`)
	pollPattern    = regexp.MustCompile(`^/polls/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	formFields = []string{"question", "option1", "option2", "option3", "option4", "option5"}
)

type Target struct {
	Replicas []*url.URL
	Browser  func() *http.Client
	Timeout  time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var Cases = []Case{
	{Name: "APollsLife", Run: aPollsLife},
	{Name: "AFormWithProblems", Run: aFormWithProblems},
	{Name: "OncePerBrowser", Run: oncePerBrowser},
	{Name: "CrossSite", Run: crossSite},
	{Name: "NotFoundAndWrongMethods", Run: notFoundAndWrongMethods},
	{Name: "EveryReplica", Run: everyReplica},
	{Name: "Headers", Run: headers},
}

func aPollsLife(t testing.TB, target Target) {
	ctx := bounded(t, target)
	first := newBrowser(t, target, 0)
	p := first.createPoll(ctx, t, "Tabs", "Spaces", "Both")

	seen := first.open(ctx, t, p)
	require.Equal(t, tally("Tabs=0", "Spaces=0", "Both=0"), seen.options)
	require.Equal(t, []string{"Tabs", "Spaces", "Both"}, seen.choices)
	require.Empty(t, seen.note)

	voted := first.vote(ctx, t, p, seen, "Tabs")
	voted.html(t, http.StatusOK)
	require.Equal(t, counted, voted.note)
	require.Equal(t, tally("Tabs=1", "Spaces=0", "Both=0"), voted.options)
	require.Empty(t, voted.choices, "a counted vote leaves nothing to vote for")

	second := newBrowser(t, target, last(target))
	seen = second.open(ctx, t, p)
	require.Equal(t, tally("Tabs=1", "Spaces=0", "Both=0"), seen.options)
	voted = second.vote(ctx, t, p, seen, "Spaces")
	voted.html(t, http.StatusOK)
	require.Equal(t, counted, voted.note)
	require.Equal(t, tally("Tabs=1", "Spaces=1", "Both=0"), voted.options)

	require.Equal(t, tally("Tabs=1", "Spaces=1", "Both=0"), first.open(ctx, t, p).options)
}

func aFormWithProblems(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := newBrowser(t, target, 0)
	for _, tc := range []struct {
		name     string
		change   func(url.Values)
		problems map[string]string
	}{
		{
			name:     "no question",
			change:   func(f url.Values) { f.Del("question") },
			problems: map[string]string{"question": "Write the poll's question."},
		},
		{
			name:     "a blank question",
			change:   func(f url.Values) { f.Set("question", "   ") },
			problems: map[string]string{"question": "Write the poll's question."},
		},
		{
			name:     "a question too long",
			change:   func(f url.Values) { f.Set("question", strings.Repeat("q", 201)) },
			problems: map[string]string{"question": "Keep the question to 200 characters."},
		},
		{
			name:     "a question sent twice",
			change:   func(f url.Values) { f.Add("question", "Vim or Emacs?") },
			problems: map[string]string{"question": "Send one question."},
		},
		{
			name:     "no first option",
			change:   func(f url.Values) { f.Del("option1") },
			problems: map[string]string{"option1": "Write this option: a poll needs at least 2."},
		},
		{
			name:     "a blank second option",
			change:   func(f url.Values) { f.Set("option2", "  ") },
			problems: map[string]string{"option2": "Write this option: a poll needs at least 2."},
		},
		{
			name:     "an option too long",
			change:   func(f url.Values) { f.Set("option3", strings.Repeat("o", 101)) },
			problems: map[string]string{"option3": "Keep each option to 100 characters."},
		},
		{
			name:     "a blank optional option",
			change:   func(f url.Values) { f.Set("option4", " ") },
			problems: map[string]string{"option4": "Write this option, or leave it empty."},
		},
		{
			name:     "an option sent twice",
			change:   func(f url.Values) { f.Add("option1", "Both") },
			problems: map[string]string{"option1": "Send this option once."},
		},
		{
			name:     "an option given twice",
			change:   func(f url.Values) { f.Set("option3", "Both"); f.Set("option5", "Tabs") },
			problems: map[string]string{"option5": "This option is given already."},
		},
		{
			name: "every problem at once",
			change: func(f url.Values) {
				f.Del("question")
				f.Set("option2", "Tabs")
				f.Set("option3", `<b>"Tabs" & 'Spaces'</b>`+strings.Repeat("o", 100))
			},
			problems: map[string]string{
				"question": "Write the poll's question.",
				"option2":  "This option is given already.",
				"option3":  "Keep each option to 100 characters.",
			},
		},
	} {
		form := pollForm(question(), "Tabs", "Spaces")
		tc.change(form)

		r := b.htmx(ctx, t, http.MethodPost, "/polls", form, "/", newPollForm)
		r.html(t, http.StatusUnprocessableEntity)
		require.Empty(t, r.header.Get("HX-Redirect"), "%s creates nothing", tc.name)
		require.Empty(t, r.header.Get("Location"), "%s creates nothing", tc.name)
		require.True(t, strings.HasPrefix(r.body, `<form id="new-poll"`), "%s: %s", tc.name, r.body)
		require.NotContains(t, r.body, "<b>", "%s: what was sent is escaped", tc.name)

		got := fields(t, r)
		for _, name := range formFields {
			require.Contains(t, got, name, "%s: the form has %s", tc.name, name)
			require.Equal(t, form.Get(name), got[name].value, "%s: %s keeps the value sent", tc.name, name)
			require.Equal(t, tc.problems[name], got[name].problem, "%s: %s's problem", tc.name, name)
		}
	}
}

func oncePerBrowser(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := newBrowser(t, target, 0).createPoll(ctx, t, "Tabs", "Spaces")

	b := newBrowser(t, target, 0)
	seen := b.open(ctx, t, p)
	b.vote(ctx, t, p, seen, "Tabs").html(t, http.StatusOK)
	again := b.vote(ctx, t, p, seen, "Spaces")
	again.html(t, http.StatusConflict)
	require.Equal(t, alreadyVoted, again.note)
	require.Equal(t, tally("Tabs=1", "Spaces=0"), again.options)
	require.Equal(t, tally("Tabs=1", "Spaces=0"), b.open(ctx, t, p).options)

	other := newBrowser(t, target, last(target))
	seen = other.open(ctx, t, p)
	choices := []string{seen.option(t, "Tabs"), seen.option(t, "Spaces")}
	var wg sync.WaitGroup
	answers := make([]result, len(choices))
	for i, choice := range choices {
		wg.Go(func() {
			answers[i] = other.try(ctx, http.MethodPost, p.votes(), url.Values{"option": {choice}}, other.fromHTMX(p.path, resultsTarget))
		})
	}
	wg.Wait()
	var codes []int
	for _, a := range answers {
		require.NoError(t, a.err)
		a.page.html(t, a.page.code)
		codes = append(codes, a.page.code)
	}
	slices.Sort(codes)
	require.Equal(t, []int{http.StatusOK, http.StatusConflict}, codes, "two votes sent at once count once")

	var votes int64
	for _, o := range other.open(ctx, t, p).options {
		votes += o.votes
	}
	require.EqualValues(t, 2, votes, "one vote from each browser")
}

func crossSite(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := newBrowser(t, target, 0).createPoll(ctx, t, "Tabs", "Spaces")
	b := newBrowser(t, target, 0)
	vote := url.Values{"option": {b.open(ctx, t, p).option(t, "Tabs")}}

	for name, header := range map[string]map[string]string{
		"a form on another site": {
			"Sec-Fetch-Site": "cross-site",
			"Sec-Fetch-Mode": "navigate",
			"Sec-Fetch-Dest": "document",
			"Origin":         elsewhere,
		},
		"htmx on another site": {
			"HX-Request":     "true",
			"HX-Current-URL": elsewhere + "/",
			"Sec-Fetch-Site": "cross-site",
			"Sec-Fetch-Mode": "cors",
			"Sec-Fetch-Dest": "empty",
			"Origin":         elsewhere,
		},
	} {
		r := b.send(ctx, t, http.MethodPost, p.votes(), vote, header)
		r.errorPage(t, http.StatusForbidden, "Forbidden", forbidden)
		require.NotContains(t, r.body, counted, name)
	}

	require.Equal(t, tally("Tabs=0", "Spaces=0"), b.open(ctx, t, p).options, "a cross-site vote counts nothing")
}

func notFoundAndWrongMethods(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := newBrowser(t, target, 0)

	home := b.navigate(ctx, t, "/")
	home.html(t, http.StatusOK)
	scripts := scriptPattern.FindAllStringSubmatch(home.body, -1)
	require.Len(t, scripts, 2, "the home page loads htmx and its SSE extension")
	for _, s := range scripts {
		b.script(ctx, t, s[1]).ok(t)
		b.script(ctx, t, path.Join(path.Dir(s[1]), "missing.js")).errorPage(t, http.StatusNotFound, "Not Found", notFound)
	}

	unknown := "/polls/" + uuid.NewString()
	b.navigate(ctx, t, unknown).errorPage(t, http.StatusNotFound, "Not Found", notFound)
	b.navigate(ctx, t, "/polls/not-a-poll").errorPage(t, http.StatusNotFound, "Not Found", notFound)
	b.htmx(ctx, t, http.MethodPost, unknown+"/votes", url.Values{"option": {"1"}}, unknown, resultsTarget).errorPage(t, http.StatusNotFound, "Not Found", notFound)

	p := newBrowser(t, target, 0).createPoll(ctx, t, "Tabs", "Spaces")
	for _, tc := range []struct {
		method string
		path   string
		allow  string
	}{
		{method: http.MethodDelete, path: p.path, allow: "GET, HEAD"},
		{method: http.MethodGet, path: p.votes(), allow: "POST"},
		{method: http.MethodPost, path: p.feed(), allow: "GET, HEAD"},
		{method: http.MethodGet, path: "/polls", allow: "POST"},
		{method: http.MethodPut, path: "/", allow: "GET, HEAD"},
	} {
		r := b.htmx(ctx, t, tc.method, tc.path, nil, p.path, resultsTarget)
		r.errorPage(t, http.StatusMethodNotAllowed, "Method Not Allowed", notAllowed)
		require.Equal(t, tc.allow, r.header.Get("Allow"), r.request)
	}
}

func everyReplica(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := newBrowser(t, target, 0).createPoll(ctx, t, "Tabs", "Spaces")
	first := newBrowser(t, target, 0)
	first.vote(ctx, t, p, first.open(ctx, t, p), "Tabs").html(t, http.StatusOK)

	var feeds []*feed
	for i := range target.Replicas {
		f := newBrowser(t, target, i).openFeed(ctx, t, p)
		require.Equal(t, tally("Tabs=1", "Spaces=0"), f.next(t), "%s starts with the current results", f.request)
		feeds = append(feeds, f)
	}

	for i, want := range [][]count{tally("Tabs=1", "Spaces=1"), tally("Tabs=1", "Spaces=2")} {
		voter := newBrowser(t, target, i*last(target))
		voter.vote(ctx, t, p, voter.open(ctx, t, p), "Spaces").html(t, http.StatusOK)
		for _, f := range feeds {
			f.until(t, want)
		}
	}
}

func headers(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := newBrowser(t, target, 0).createPoll(ctx, t, "Tabs", "Spaces")
	b := newBrowser(t, target, last(target))

	first := b.navigate(ctx, t, p.path)
	first.html(t, http.StatusOK)
	raw := first.header.Values("Set-Cookie")
	require.Len(t, raw, 1, "the first page sets one cookie")
	require.NotContains(t, strings.ToLower(raw[0]), "domain=", "the cookie has no Domain")
	cookie, err := http.ParseSetCookie(raw[0])
	require.NoError(t, err)
	require.Equal(t, voterCookie, cookie.Name)
	require.Regexp(t, voterPattern, cookie.Value)
	require.Equal(t, "/", cookie.Path)
	require.Empty(t, cookie.Domain)
	require.Equal(t, voterMaxAge, cookie.MaxAge)
	require.True(t, cookie.Secure)
	require.True(t, cookie.HttpOnly)
	require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)

	again := b.navigate(ctx, t, p.path)
	again.html(t, http.StatusOK)
	require.Empty(t, again.header.Values("Set-Cookie"), "a browser with the cookie keeps it")

	seen := again.view(t)
	pages := []page{
		b.navigate(ctx, t, "/"),
		first,
		again,
		b.vote(ctx, t, p, seen, "Tabs").page,
		b.vote(ctx, t, p, seen, "Spaces").page,
		b.htmx(ctx, t, http.MethodPost, "/polls", url.Values{"question": {question()}}, "/", newPollForm),
		b.navigate(ctx, t, "/polls/"+uuid.NewString()),
		b.htmx(ctx, t, http.MethodDelete, p.path, nil, p.path, resultsTarget),
		b.send(ctx, t, http.MethodPost, p.votes(), url.Values{"option": {seen.option(t, "Tabs")}}, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": elsewhere}),
	}
	for _, r := range pages {
		require.Equal(t, htmlType, r.header.Get("Content-Type"), r.request)
		require.Equal(t, csp, r.header.Get("Content-Security-Policy"), r.request)
	}
}

type count struct {
	option string
	votes  int64
}

func tally(counts ...string) []count {
	var got []count
	for _, c := range counts {
		option, votes, _ := strings.Cut(c, "=")
		n, _ := strconv.ParseInt(votes, 10, 64)
		got = append(got, count{option: option, votes: n})
	}
	return got
}

func question() string {
	return "Tabs or spaces? " + rand.Text()
}

func pollForm(question string, options ...string) url.Values {
	form := url.Values{"question": {question}}
	for i, o := range options {
		form.Set(fmt.Sprintf("option%d", i+1), o)
	}
	return form
}

func last(target Target) int {
	return len(target.Replicas) - 1
}

type poll struct {
	path     string
	question string
}

func (p poll) votes() string { return p.path + "/votes" }

func (p poll) feed() string { return p.path + "/feed" }

type browser struct {
	client  *http.Client
	replica *url.URL
}

func newBrowser(t testing.TB, target Target, replica int) browser {
	t.Helper()
	require.GreaterOrEqual(t, len(target.Replicas), 2, "Target.Replicas")
	require.NotNil(t, target.Replicas[replica], "Target.Replicas[%d]", replica)
	require.NotNil(t, target.Browser, "Target.Browser")
	c := target.Browser()
	require.NotNil(t, c, "Target.Browser()")
	require.NotNil(t, c.Jar, "Target.Browser()'s cookie jar")
	return browser{client: c, replica: target.Replicas[replica]}
}

func (b browser) origin() string {
	return b.replica.Scheme + "://" + b.replica.Host
}

func (b browser) navigate(ctx context.Context, t testing.TB, path string) page {
	t.Helper()
	return b.send(ctx, t, http.MethodGet, path, nil, map[string]string{
		"Sec-Fetch-Site": "none",
		"Sec-Fetch-Mode": "navigate",
		"Sec-Fetch-Dest": "document",
		"Sec-Fetch-User": "?1",
	})
}

func (b browser) script(ctx context.Context, t testing.TB, path string) page {
	t.Helper()
	return b.send(ctx, t, http.MethodGet, path, nil, map[string]string{
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Mode": "no-cors",
		"Sec-Fetch-Dest": "script",
	})
}

func (b browser) fromHTMX(from, target string) map[string]string {
	return map[string]string{
		"HX-Request":     "true",
		"HX-Current-URL": b.replica.JoinPath(from).String(),
		"HX-Target":      target,
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
		"Origin":         b.origin(),
	}
}

func (b browser) htmx(ctx context.Context, t testing.TB, method, path string, form url.Values, from, target string) page {
	t.Helper()
	header := b.fromHTMX(from, target)
	if target == newPollForm {
		header["HX-Trigger"] = newPollForm
	}
	return b.send(ctx, t, method, path, form, header)
}

func (b browser) createPoll(ctx context.Context, t testing.TB, options ...string) poll {
	t.Helper()
	q := question()
	r := b.htmx(ctx, t, http.MethodPost, "/polls", pollForm(q, options...), "/", newPollForm)
	require.Equal(t, http.StatusNoContent, r.code, "%s: %s", r.request, r.body)
	to := r.header.Get("HX-Redirect")
	require.Regexp(t, pollPattern, to, r.request)
	return poll{path: to, question: q}
}

func (b browser) open(ctx context.Context, t testing.TB, p poll) view {
	t.Helper()
	r := b.navigate(ctx, t, p.path)
	r.html(t, http.StatusOK)
	m := headingPattern.FindStringSubmatch(r.body)
	require.NotNil(t, m, "%s: %s", r.request, r.body)
	require.Equal(t, p.question, html.UnescapeString(m[1]), r.request)
	return r.view(t)
}

func (b browser) vote(ctx context.Context, t testing.TB, p poll, v view, choice string) view {
	t.Helper()
	r := b.send(ctx, t, http.MethodPost, p.votes(), url.Values{"option": {v.option(t, choice)}}, b.fromHTMX(p.path, resultsTarget))
	return r.view(t)
}

type result struct {
	page page
	err  error
}

func (b browser) try(ctx context.Context, method, path string, form url.Values, header map[string]string) result {
	resp, request, err := b.do(ctx, method, path, form, header)
	if err != nil {
		return result{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return result{err: fmt.Errorf("%s: %w", request, err)}
	}
	return result{page: page{request: request, code: resp.StatusCode, header: resp.Header, body: string(body)}}
}

func (b browser) send(ctx context.Context, t testing.TB, method, path string, form url.Values, header map[string]string) page {
	t.Helper()
	r := b.try(ctx, method, path, form, header)
	require.NoError(t, r.err)
	return r.page
}

func (b browser) do(ctx context.Context, method, path string, form url.Values, header map[string]string) (*http.Response, string, error) {
	u := b.replica.JoinPath(path)
	request := method + " " + u.String()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, request, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, request, fmt.Errorf("%s: %w", request, err)
	}
	return resp, request, nil
}

type page struct {
	request string
	code    int
	header  http.Header
	body    string
}

func (r page) ok(t testing.TB) {
	t.Helper()
	require.Equal(t, http.StatusOK, r.code, "%s: %s", r.request, r.body)
}

func (r page) html(t testing.TB, code int) {
	t.Helper()
	require.Equal(t, code, r.code, "%s: %s", r.request, r.body)
	require.Equal(t, htmlType, r.header.Get("Content-Type"), r.request)
}

func (r page) errorPage(t testing.TB, code int, title, message string) {
	t.Helper()
	r.html(t, code)
	m := errorPattern.FindStringSubmatch(r.body)
	require.NotNil(t, m, "%s: %s", r.request, r.body)
	require.Equal(t, title, html.UnescapeString(m[1]), r.request)
	require.Equal(t, message, html.UnescapeString(m[2]), r.request)
}

type view struct {
	page
	options []count
	choices []string
	buttons map[string]string
	note    string
}

func (r page) view(t testing.TB) view {
	t.Helper()
	v := view{page: r, options: counts(t, r.request, r.body), buttons: make(map[string]string)}
	for _, m := range buttonPattern.FindAllStringSubmatch(r.body, -1) {
		text := html.UnescapeString(m[2])
		v.choices = append(v.choices, text)
		v.buttons[text] = m[1]
	}
	if m := notePattern.FindStringSubmatch(r.body); m != nil {
		v.note = html.UnescapeString(m[1])
	}
	return v
}

func (v view) option(t testing.TB, choice string) string {
	t.Helper()
	id, ok := v.buttons[choice]
	require.True(t, ok, "%s: no button for %q", v.request, choice)
	return id
}

func counts(t testing.TB, request, body string) []count {
	t.Helper()
	var got []count
	for _, m := range optionPattern.FindAllStringSubmatch(body, -1) {
		votes, err := strconv.ParseInt(m[2], 10, 64)
		require.NoError(t, err, request)
		got = append(got, count{option: html.UnescapeString(m[1]), votes: votes})
	}
	return got
}

type field struct {
	value   string
	problem string
}

func fields(t testing.TB, r page) map[string]field {
	t.Helper()
	got := make(map[string]field)
	for _, m := range fieldPattern.FindAllStringSubmatch(r.body, -1) {
		got[m[1]] = field{value: html.UnescapeString(m[2]), problem: html.UnescapeString(m[3])}
	}
	return got
}

type feed struct {
	request string
	body    io.ReadCloser
	lines   *bufio.Reader
	cancel  context.CancelFunc
	last    []count
}

func (b browser) openFeed(ctx context.Context, t testing.TB, p poll) *feed {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	resp, request, err := b.do(ctx, http.MethodGet, p.feed(), nil, map[string]string{
		"Accept":         "text/event-stream",
		"Cache-Control":  "no-cache",
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
	})
	if err != nil {
		cancel()
		require.NoError(t, err)
	}
	f := &feed{request: request, body: resp.Body, lines: bufio.NewReader(resp.Body), cancel: cancel}
	t.Cleanup(f.close)
	require.Equal(t, http.StatusOK, resp.StatusCode, request)
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	require.NoError(t, err, request)
	require.Equal(t, "text/event-stream", media, request)
	return f
}

func (f *feed) close() {
	f.cancel()
	_ = f.body.Close()
}

func (f *feed) next(t testing.TB) []count {
	t.Helper()
	var (
		event string
		data  []string
	)
	for {
		line, err := f.lines.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			require.NoError(t, err, "%s: reading the feed, whose last results were %v", f.request, f.last)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, ":"):
		case line == "":
			if event == resultsEvent {
				f.last = counts(t, f.request, strings.Join(data, "\n"))
				return f.last
			}
			event, data = "", nil
		default:
			name, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch name {
			case "event":
				event = value
			case "data":
				data = append(data, value)
			}
		}
	}
}

func (f *feed) until(t testing.TB, want []count) {
	t.Helper()
	for {
		if slices.Equal(f.next(t), want) {
			return
		}
	}
}

func bounded(t testing.TB, target Target) context.Context {
	require.Positive(t, target.Timeout, "Target.Timeout")
	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout)
	t.Cleanup(cancel)
	return ctx
}
