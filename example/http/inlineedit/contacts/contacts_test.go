// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package contacts_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/inlineedit/contacts"
	"github.com/z5labs/humus/hypermedia"
	"github.com/z5labs/humus/telemetry"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name, email string
		want        contacts.Problems
	}{
		{name: "Joe", email: "joe@example.com", want: contacts.Problems{}},
		{name: "", email: "joe@example.com", want: contacts.Problems{"name": "Enter a name."}},
		{name: "  ", email: "joe@example.com", want: contacts.Problems{"name": "Enter a name."}},
		{name: "Joe", email: "", want: contacts.Problems{"email": "Enter an email address."}},
		{name: "Joe", email: "joe", want: contacts.Problems{"email": "Enter an email address."}},
		{name: "Joe", email: "Joe <joe@example.com>", want: contacts.Problems{"email": "Enter an email address."}},
		{name: "", email: "joe", want: contacts.Problems{"name": "Enter a name.", "email": "Enter an email address."}},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q %q", c.name, c.email), func(t *testing.T) {
			require.Equal(t, c.want, contacts.Validate(c.name, c.email))
		})
	}
}

func TestStore_Get_answers_a_contact_it_doesnt_have_with_a_404(t *testing.T) {
	_, err := contacts.NewStore().Get(99)
	re, ok := errors.AsType[*hypermedia.RequestError](err)
	require.True(t, ok, "err = %v", err)
	require.Equal(t, http.StatusNotFound, re.Status)
}

func TestStore_Put_saves_a_contact(t *testing.T) {
	s := contacts.NewStore()
	s.Put(contacts.Contact{ID: 2, Name: "Angie", Email: "angie@example.org"})
	s.Put(contacts.Contact{ID: 7, Name: "Sam", Email: "sam@example.com"})

	c, err := s.Get(2)
	require.NoError(t, err)
	require.Equal(t, contacts.Contact{ID: 2, Name: "Angie", Email: "angie@example.org"}, c)

	var ids []contacts.ID
	for _, c := range s.All() {
		ids = append(ids, c.ID)
	}
	require.Equal(t, []contacts.ID{1, 2, 3, 7}, ids)
}

func TestStore_Subscribe_gets_every_saved_contact(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := contacts.NewStore()
		first, unsubscribeFirst := s.Subscribe()
		defer unsubscribeFirst()
		second, unsubscribeSecond := s.Subscribe()
		defer unsubscribeSecond()

		saved := []contacts.Contact{
			{ID: 1, Name: "Joe", Email: "joe@example.com"},
			{ID: 2, Name: "Angie", Email: "angie@example.com"},
			{ID: 1, Name: "Joseph", Email: "joseph@example.com"},
		}
		for _, c := range saved {
			s.Put(c)
		}
		for _, changed := range []<-chan contacts.Contact{first, second} {
			for _, want := range saved {
				require.Equal(t, want, <-changed)
			}
		}
	})
}

func TestStore_Subscribe_stops_at_unsubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := contacts.NewStore()
		changed, unsubscribe := s.Subscribe()
		s.Put(contacts.Contact{ID: 1, Name: "Joe", Email: "joe@example.com"})
		unsubscribe()
		unsubscribe()
		s.Put(contacts.Contact{ID: 2, Name: "Angie", Email: "angie@example.com"})
		synctest.Wait()
		select {
		case c := <-changed:
			t.Fatalf("got %v after unsubscribe", c)
		default:
		}
	})
}

func TestErrorPage_answers_with_a_RequestErrors_status_and_500_otherwise(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{err: &hypermedia.RequestError{Status: http.StatusNotFound}, want: http.StatusNotFound},
		{err: fmt.Errorf("wrapped: %w", &hypermedia.RequestError{Status: http.StatusMethodNotAllowed}), want: http.StatusMethodNotAllowed},
		{err: errors.New("the database is down"), want: http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.err.Error(), func(t *testing.T) {
			w := httptest.NewRecorder()
			require.NoError(t, contacts.ErrorPage(w, httptest.NewRequest(http.MethodGet, "/", nil), c.err))
			require.Equal(t, c.want, w.Code)
			require.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))
			require.Contains(t, w.Body.String(), "<h1>"+http.StatusText(c.want)+"</h1>")
			require.NotContains(t, w.Body.String(), c.err.Error())
		})
	}
}

func TestAssets_serves_a_file_and_answers_anything_else_with_a_404(t *testing.T) {
	fsys := fstest.MapFS{
		"app.js":       {Data: []byte("console.log('hi')\n")},
		"lib/x.js":     {Data: []byte("x()\n")},
		"lib/y/z.html": {Data: []byte("<p>z</p>\n")},
	}
	site, err := hypermedia.NewSite(&humus.Setup{Telemetry: telemetry.Noop()}, contacts.ErrorPage, []hypermedia.Route{
		hypermedia.NewRoute(http.MethodGet, contacts.Static, contacts.Assets(fsys)),
	})
	require.NoError(t, err)

	for target, body := range map[string]string{
		"/static/app.js":   "console.log('hi')\n",
		"/static/lib/x.js": "x()\n",
	} {
		w := httptest.NewRecorder()
		site.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		require.Equal(t, http.StatusOK, w.Code, target)
		require.Equal(t, "text/javascript; charset=utf-8", w.Header().Get("Content-Type"), target)
		require.Equal(t, body, w.Body.String(), target)
	}

	for _, target := range []string{"/static/", "/static/missing.js", "/static/lib", "/static/lib/", "/static/lib/y"} {
		w := httptest.NewRecorder()
		site.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		require.Equal(t, http.StatusNotFound, w.Code, target)
		require.Contains(t, w.Body.String(), "<h1>Not Found</h1>", target)
	}
}

func TestPaths_build_their_links(t *testing.T) {
	id := hypermedia.Arg(contacts.ContactID, 7)
	require.Equal(t, "/contacts", contacts.List.URL().String())
	require.Equal(t, "/contacts/7", contacts.One.URL(id).String())
	require.Equal(t, "/contacts/7/edit", contacts.Edit.URL(id).String())
	require.Equal(t, "/contacts/changes", contacts.Changes.URL().String())
	require.Equal(t, "/static/htmx.min.js", contacts.Static.URL(hypermedia.Arg(contacts.Asset, "htmx.min.js")).String())
}
