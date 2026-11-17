// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package ui_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/polls/store/storetest"
)

var htmx = map[string]string{"HX-Request": "true"}

type recording struct {
	*storetest.Store
	creates atomic.Int64
}

func (r *recording) CreatePoll(ctx context.Context, question string, options []string) (uuid.UUID, error) {
	r.creates.Add(1)
	return r.Store.CreatePoll(ctx, question, options)
}

func post(t *testing.T, c *http.Client, form url.Values, header map[string]string) response {
	t.Helper()
	noFollow := *c
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return send(t, &noFollow, http.MethodPost, base+"/polls", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), header)
}

var fieldPattern = regexp.MustCompile(`<p><label>[^<]*<input name="([^"]*)" value="([^"]*)" maxlength="([0-9]+)"( required)?></label>(?: <span class="problem">([^<]*)</span>)?</p>`)

type shown struct {
	value     string
	maxLength string
	required  bool
	problem   string
}

func fields(t *testing.T, body string) map[string]shown {
	t.Helper()
	got := make(map[string]shown)
	for _, m := range fieldPattern.FindAllStringSubmatch(body, -1) {
		got[m[1]] = shown{value: html.UnescapeString(m[2]), maxLength: m[3], required: m[4] != "", problem: html.UnescapeString(m[5])}
	}
	require.Len(t, got, 6, "the question and five options, in %s", body)
	return got
}

func goodForm() url.Values {
	return url.Values{"question": {"Tabs or spaces?"}, "option1": {"Tabs"}, "option2": {"Spaces"}}
}

func TestSite_the_home_page_holds_the_form_that_creates_a_poll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, stop := start(t, storetest.New())

		home := do(t, c, http.MethodGet, base+"/")
		require.Equal(t, http.StatusOK, home.status)
		require.Equal(t, "text/html; charset=utf-8", home.header.Get("Content-Type"))
		require.Equal(t, csp, home.header.Get("Content-Security-Policy"))
		require.Contains(t, home.body, "<!DOCTYPE html>")
		require.Contains(t, home.body, "<title>Polls</title>")
		require.Contains(t, home.body, `<form id="new-poll" method="post" action="/polls" hx-post="/polls" hx-swap="outerHTML">`)
		require.Equal(t, map[string]shown{
			"question": {maxLength: "200", required: true},
			"option1":  {maxLength: "100", required: true},
			"option2":  {maxLength: "100", required: true},
			"option3":  {maxLength: "100"},
			"option4":  {maxLength: "100"},
			"option5":  {maxLength: "100"},
		}, fields(t, home.body))

		stop()
	})
}

func TestSite_a_form_with_problems_is_a_422_that_shows_each_in_place_and_creates_nothing(t *testing.T) {
	testCases := []struct {
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
			change:   func(f url.Values) { f["question"] = []string{"Tabs or spaces?", "Vim or Emacs?"} },
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
			change:   func(f url.Values) { f["option1"] = []string{"Tabs", "Both"} },
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
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				polls := &recording{Store: storetest.New()}
				c, stop := start(t, polls)

				form := goodForm()
				tc.change(form)
				want := make(map[string]shown)
				for _, name := range []string{"question", "option1", "option2", "option3", "option4", "option5"} {
					want[name] = shown{value: form.Get(name), problem: tc.problems[name]}
				}

				page := post(t, c, form, nil)
				fragment := post(t, c, form, htmx)
				for _, r := range []response{page, fragment} {
					require.Equal(t, http.StatusUnprocessableEntity, r.status)
					require.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"))
					require.Equal(t, []string{"HX-Request"}, r.header.Values("Vary"))
					got := fields(t, r.body)
					for name, w := range want {
						require.Equal(t, w.value, got[name].value, "%s keeps the value sent", name)
						require.Equal(t, w.problem, got[name].problem, "%s's problem", name)
					}
				}
				require.Contains(t, page.body, "<!DOCTYPE html>")
				require.Contains(t, page.body, `<form id="new-poll"`)
				require.True(t, strings.HasPrefix(fragment.body, `<form id="new-poll" method="post" action="/polls" hx-post="/polls" hx-swap="outerHTML">`), "body = %q", fragment.body)
				require.NotContains(t, fragment.body, "<html")
				require.NotContains(t, page.body, "<b>", "what was sent is escaped")
				require.Zero(t, polls.creates.Load(), "nothing is created")

				stop()
			})
		})
	}
}

func TestSite_a_good_form_creates_the_poll_and_leads_to_its_page(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := &recording{Store: storetest.New()}
		c, stop := start(t, polls)
		pollURL := regexp.MustCompile(`^/polls/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

		form := goodForm()
		form.Set("option3", "Both")
		form.Set("option4", "")
		created := post(t, c, form, nil)
		require.Equal(t, http.StatusSeeOther, created.status)
		require.Equal(t, []string{"HX-Request"}, created.header.Values("Vary"))
		location := created.header.Get("Location")
		require.Regexp(t, pollURL, location)

		page := do(t, c, http.MethodGet, base+location)
		require.Equal(t, http.StatusOK, page.status)
		require.Equal(t, "text/html; charset=utf-8", page.header.Get("Content-Type"))
		require.Contains(t, page.body, "<title>Tabs or spaces?</title>")
		require.Contains(t, page.body, "<h1>Tabs or spaces?</h1>")
		require.Contains(t, page.body, "<li><span class=\"option\">Tabs</span> <span class=\"votes\">0</span></li>\n"+
			"<li><span class=\"option\">Spaces</span> <span class=\"votes\">0</span></li>\n"+
			"<li><span class=\"option\">Both</span> <span class=\"votes\">0</span></li>\n</ul>", "the options given, in order, and no empty one")
		require.Contains(t, page.body, `<a href="/">Create another poll</a>`)

		byHTMX := post(t, c, goodForm(), htmx)
		require.Equal(t, http.StatusNoContent, byHTMX.status)
		require.Empty(t, byHTMX.body)
		require.Equal(t, []string{"HX-Request"}, byHTMX.header.Values("Vary"))
		redirect := byHTMX.header.Get("HX-Redirect")
		require.Regexp(t, pollURL, redirect)
		require.NotEqual(t, location, redirect, "a second poll")
		require.Equal(t, http.StatusOK, do(t, c, http.MethodGet, base+redirect).status)

		require.EqualValues(t, 2, polls.creates.Load())

		stop()
	})
}

func TestSite_the_polls_page_shows_each_options_count(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		ctx := t.Context()
		id, err := polls.CreatePoll(ctx, `Tabs or <spaces>?`, []string{"Tabs", "Spaces", "Both"})
		require.NoError(t, err)
		poll, err := polls.Poll(ctx, id)
		require.NoError(t, err)
		for voter, option := range map[string]int{"alice": 0, "bob": 0, "carol": 2} {
			require.NoError(t, polls.Vote(ctx, id, poll.Options[option].ID, voter))
		}
		c, stop := start(t, polls)

		page := do(t, c, http.MethodGet, base+"/polls/"+id.String())
		require.Equal(t, http.StatusOK, page.status)
		require.Equal(t, csp, page.header.Get("Content-Security-Policy"))
		require.Contains(t, page.body, "<h1>Tabs or &lt;spaces&gt;?</h1>")
		require.Contains(t, page.body, "<ul id=\"options\">\n"+
			"<li><span class=\"option\">Tabs</span> <span class=\"votes\">2</span></li>\n"+
			"<li><span class=\"option\">Spaces</span> <span class=\"votes\">0</span></li>\n"+
			"<li><span class=\"option\">Both</span> <span class=\"votes\">1</span></li>\n</ul>")

		stop()
	})
}

func TestSite_an_unknown_poll_and_an_ID_that_doesnt_parse_are_the_404_page(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := storetest.New()
		id, err := polls.CreatePoll(t.Context(), "Tabs or spaces?", []string{"Tabs", "Spaces"})
		require.NoError(t, err)
		c, stop := start(t, polls)

		for _, target := range []string{
			"/polls/" + uuid.NewString(),
			"/polls/not-a-poll",
			"/polls/" + strings.ToUpper(id.String()),
			"/polls/urn:uuid:" + id.String(),
			"/polls/" + strings.ReplaceAll(id.String(), "-", ""),
		} {
			r := do(t, c, http.MethodGet, base+target)
			require.Equal(t, http.StatusNotFound, r.status, target)
			require.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"), target)
			require.Contains(t, r.body, "<h1>Not Found</h1>", target)
			require.Contains(t, r.body, "<p>There&#39;s no such page.</p>", target)
		}
		require.Equal(t, http.StatusOK, do(t, c, http.MethodGet, base+"/polls/"+id.String()).status)

		stop()
	})
}

func TestSite_a_body_over_the_limit_is_the_413_page_and_one_that_isnt_a_form_the_415_page(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		polls := &recording{Store: storetest.New()}
		c, stop := start(t, polls)

		tooLarge := goodForm()
		tooLarge.Set("question", strings.Repeat("q", 16<<10))
		for _, header := range []map[string]string{nil, htmx} {
			r := post(t, c, tooLarge, header)
			require.Equal(t, http.StatusRequestEntityTooLarge, r.status)
			require.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"))
			require.Contains(t, r.body, "<h1>Request Entity Too Large</h1>")
			require.Contains(t, r.body, "<p>What was sent is too large.</p>")

			for _, contentType := range []string{"application/json", ""} {
				r = send(t, c, http.MethodPost, base+"/polls", contentType, strings.NewReader(`{"question":"Tabs or spaces?","options":["Tabs","Spaces"]}`), header)
				require.Equal(t, http.StatusUnsupportedMediaType, r.status, contentType)
				require.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"), contentType)
				require.Contains(t, r.body, "<h1>Unsupported Media Type</h1>", contentType)
				require.Contains(t, r.body, "<p>What was sent isn&#39;t a form.</p>", contentType)
			}
		}
		require.Zero(t, polls.creates.Load(), "nothing is created")

		justUnder := goodForm()
		justUnder.Set("question", "q")
		padding := 16<<10 - len(justUnder.Encode()) - len("&pad=")
		justUnder.Set("pad", strings.Repeat("p", padding))
		require.Len(t, justUnder.Encode(), 16<<10)
		require.Equal(t, http.StatusSeeOther, post(t, c, justUnder, nil).status, "a body of exactly the limit is read")

		stop()
	})
}
