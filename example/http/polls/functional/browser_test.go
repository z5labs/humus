//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package functional

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
	"github.com/stretchr/testify/require"
)

const (
	headlessShell = "headless-shell"
	devtoolsPort  = 9222
	site          = "https://polls:8443"
	stepLimit     = 15 * time.Second
	feedLimit     = 2 * time.Minute
)

var (
	pollPath = regexp.MustCompile(`^/polls/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	feedPath = regexp.MustCompile(`^/polls/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/feed$`)
)

func TestPolls_runs_its_htmx_in_headless_Chrome_on_kind(t *testing.T) {
	for _, d := range []string{"postgres", polls, headlessShell} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}
	alloc, cancel := remote.NewAllocator(context.Background(), "ws://"+forward(t, "deployment/"+headlessShell, devtoolsPort)+"/")
	t.Cleanup(cancel)

	t.Run("AVoteReachesAnotherBrowser", func(t *testing.T) {
		a, b := newChrome(t, alloc), newChrome(t, alloc)
		poll := a.createPoll(t, "Which colour?", "Red", "Blue")
		b.open(t, poll)

		a.vote(t, "Red")
		a.waitNote(t, "Your vote is counted.")
		a.waitVotes(t, stepLimit, "Red", "1")
		b.waitVotes(t, stepLimit, "Red", "1")

		require.Empty(t, b.requests(http.MethodPost), "browser B sent no POST")
		require.Equal(t, []string{poll}, b.navigations(), "browser B stays on the poll's page")
		a.clean(t)
		b.clean(t)
	})

	t.Run("AFormWithProblemsInPlace", func(t *testing.T) {
		a := newChrome(t, alloc)
		a.openHome(t)
		a.fill(t, map[string]string{"question": " ", "option1": "Red", "option2": "Red"})
		a.submit(t)
		a.waitText(t, `#new-poll p:has(input[name="question"]) .problem`, "Write the poll's question.")
		a.waitText(t, `#new-poll p:has(input[name="option2"]) .problem`, "This option is given already.")
		require.Equal(t, []string{site + "/"}, a.navigations(), "the problems are shown in place")

		a.fill(t, map[string]string{"question": "Which colour?", "option2": "Blue"})
		a.submit(t)
		poll := a.waitPoll(t)
		a.waitText(t, "h1", "Which colour?")
		require.Equal(t, []string{site + "/", poll}, a.navigations())

		posts := a.requests(http.MethodPost)
		require.Len(t, posts, 2, "a form with problems and a good one")
		for _, r := range posts {
			require.Equal(t, site+"/polls", r.URL)
			require.Equal(t, "true", header(r.Headers, "HX-Request"), "sent by htmx: %v", r.Headers)
		}
		a.clean(t)
	})

	t.Run("ARolloutWithAPageOpen", func(t *testing.T) {
		a, b := newChrome(t, alloc), newChrome(t, alloc)
		poll := a.createPoll(t, "Which colour?", "Red", "Blue")
		b.open(t, poll)

		old := readyPods(t)
		kubectl(t, "rollout", "restart", "deployment/"+polls)
		kubectl(t, "rollout", "status", "deployment/"+polls, "--timeout", "5m")
		gone := []string{"wait", "--for", "delete", "--timeout", "5m"}
		for _, pod := range old {
			gone = append(gone, "pod/"+pod)
		}
		kubectl(t, gone...)

		a.open(t, poll)
		a.vote(t, "Blue")
		a.waitNote(t, "Your vote is counted.")
		b.waitVotes(t, feedLimit, "Blue", "1")

		require.GreaterOrEqual(t, b.feedsOpened(), 2, "browser B's feed opened again after the rollout ended it")
		require.Empty(t, b.requests(http.MethodPost), "browser B sent no POST")
		require.Equal(t, []string{poll}, b.navigations(), "browser B stays on the poll's page")
		a.clean(t)
		b.clean(t)
	})
}

type chrome struct {
	ctx context.Context

	mu         sync.Mutex
	sent       []*network.Request
	navigated  []string
	exceptions []string
	security   []string
	feeds      int
}

func newChrome(t *testing.T, alloc context.Context) *chrome {
	t.Helper()
	b := &chrome{}
	ctx, cancel := chromedp.NewContext(alloc, chromedp.WithNewBrowserContext(), chromedp.WithListener(b.record))
	t.Cleanup(cancel)
	b.ctx = ctx
	require.NoError(t, chromedp.Do(ctx), "opening a browser context in headless-shell")
	return b
}

func (b *chrome) record(ev any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch ev := ev.(type) {
	case *network.EventRequestWillBeSent:
		b.sent = append(b.sent, ev.Request)
	case *page.EventFrameNavigated:
		if ev.Frame.ParentID == "" {
			b.navigated = append(b.navigated, ev.Frame.URL)
		}
	case *network.EventResponseReceived:
		path, _, _ := strings.Cut(strings.TrimPrefix(ev.Response.URL, site), "?")
		if feedPath.MatchString(path) && ev.Response.Status == http.StatusOK {
			b.feeds++
		}
	case *runtime.EventExceptionThrown:
		b.exceptions = append(b.exceptions, (&chromedp.ExceptionError{ExceptionDetails: ev.ExceptionDetails}).Error())
	case *log.EventEntryAdded:
		if ev.Entry.Source == log.EntrySourceSecurity {
			b.security = append(b.security, ev.Entry.Text)
		}
	}
}

func (b *chrome) openHome(t *testing.T) {
	t.Helper()
	require.NoError(t, chromedp.Do(b.ctx,
		chromedp.Navigate(site+"/"),
		chromedp.WaitVisible(chromedp.CSS(`#new-poll button[type="submit"]`)),
	))
}

func (b *chrome) createPoll(t *testing.T, question string, options ...string) string {
	t.Helper()
	b.openHome(t)
	values := map[string]string{"question": question}
	for i, o := range options {
		values[fmt.Sprintf("option%d", i+1)] = o
	}
	b.fill(t, values)
	b.submit(t)
	poll := b.waitPoll(t)
	b.waitText(t, "h1", question)
	return poll
}

func (b *chrome) open(t *testing.T, poll string) {
	t.Helper()
	opened := b.feedsOpened()
	require.NoError(t, chromedp.Do(b.ctx,
		chromedp.Navigate(poll),
		chromedp.WaitVisible(chromedp.CSS("#results form button")),
	))
	require.Eventually(t, func() bool { return b.feedsOpened() > opened }, stepLimit, 50*time.Millisecond, "the page's feed never opened; exceptions: %q", b.thrown())
}

func (b *chrome) fill(t *testing.T, values map[string]string) {
	t.Helper()
	for name, value := range values {
		require.NoError(t, chromedp.Do(b.ctx, chromedp.SetValue(chromedp.CSS(fmt.Sprintf(`#new-poll input[name=%q]`, name)), value)), "filling in %s", name)
	}
}

func (b *chrome) submit(t *testing.T) {
	t.Helper()
	b.poll(t, stepLimit, `(f => f !== null && f["htmx-internal-data"]?.initHash !== undefined && !f.classList.contains("htmx-added"))(document.querySelector("#new-poll"))`, "htmx to take the form's submit")
	require.NoError(t, chromedp.Do(b.ctx, chromedp.Click(chromedp.CSS(`#new-poll button[type="submit"]`))))
}

func (b *chrome) vote(t *testing.T, option string) {
	t.Helper()
	b.poll(t, stepLimit, `(f => f !== null && f["htmx-internal-data"]?.initHash !== undefined && !f.classList.contains("htmx-added"))(document.querySelector("#results form"))`, "htmx to take the vote's submit")
	require.NoError(t, chromedp.Do(b.ctx, chromedp.Click(chromedp.Search(fmt.Sprintf(`//div[@id="results"]//button[text()=%q]`, option)))))
}

func (b *chrome) waitPoll(t *testing.T) string {
	t.Helper()
	var location string
	require.Eventually(t, func() bool {
		var err error
		location, err = chromedp.Run(b.ctx, chromedp.Location())
		return err == nil && pollPath.MatchString(strings.TrimPrefix(location, site))
	}, stepLimit, 100*time.Millisecond, "the browser didn't go to the new poll's page")
	return location
}

func (b *chrome) waitNote(t *testing.T, want string) {
	t.Helper()
	b.waitText(t, "#results .note", want)
}

func (b *chrome) waitVotes(t *testing.T, limit time.Duration, option, votes string) {
	t.Helper()
	b.poll(t, limit, fmt.Sprintf(`[...document.querySelectorAll("#options li")].some(li => li.querySelector(".option")?.textContent === %q && li.querySelector(".votes")?.textContent === %q)`, option, votes), fmt.Sprintf("%s to have %s votes", option, votes))
}

func (b *chrome) waitText(t *testing.T, sel, want string) {
	t.Helper()
	b.poll(t, stepLimit, fmt.Sprintf(`(document.querySelector(%q)?.textContent ?? "").includes(%q)`, sel, want), fmt.Sprintf("%q in %s", want, sel))
}

func (b *chrome) poll(t *testing.T, limit time.Duration, expression, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(b.ctx, limit)
	defer cancel()
	_, err := chromedp.Run(ctx, chromedp.Poll[bool](expression))
	if err != nil {
		body, _ := chromedp.Run(b.ctx, chromedp.Evaluate[string](`document.body?.innerHTML ?? ""`))
		require.NoError(t, err, "waiting for %s; the page holds %q; exceptions: %q", what, body, b.thrown())
	}
}

func (b *chrome) clean(t *testing.T) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	require.Empty(t, b.exceptions, "the page threw")
	require.Empty(t, b.security, "the browser reported a security problem, such as a CSP violation")
}

func (b *chrome) requests(method string) []*network.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*network.Request
	for _, r := range b.sent {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func (b *chrome) navigations() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.navigated...)
}

func (b *chrome) feedsOpened() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.feeds
}

func (b *chrome) thrown() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.exceptions...)
}

func header(h network.Headers, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return fmt.Sprint(v)
		}
	}
	return ""
}
