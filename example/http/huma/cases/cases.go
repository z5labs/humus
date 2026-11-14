// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the users API's black-box cases, which every step of the Huma migration guide passes.
package cases

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const maxBodyBytes = 1 << 20

type Target struct {
	URL    *url.URL
	Client *http.Client
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "AUsersLife", Run: AUsersLife},
	{Name: "Pages", Run: Pages},
	{Name: "Retries", Run: Retries},
	{Name: "BadParameters", Run: BadParameters},
	{Name: "BadBodies", Run: BadBodies},
	{Name: "Unmatched", Run: Unmatched},
}

func AUsersLife(t testing.TB, target Target) {
	created := createUser(t, target, "", newUser("Ada", "ada@example.com"))
	require.Positive(t, created.ID)
	require.Equal(t, user{ID: created.ID, Name: "Ada", Email: "ada@example.com"}, created)

	var got user
	call(t, target, http.MethodGet, userPath(created.ID), nil).decode(t, http.StatusOK, &got)
	require.Equal(t, created, got)

	var updated user
	call(t, target, http.MethodPut, userPath(created.ID), newUser("Ada Lovelace", "ada@example.com")).decode(t, http.StatusOK, &updated)
	require.Equal(t, user{ID: created.ID, Name: "Ada Lovelace", Email: "ada@example.com"}, updated)

	call(t, target, http.MethodGet, userPath(created.ID), nil).decode(t, http.StatusOK, &got)
	require.Equal(t, updated, got)

	call(t, target, http.MethodDelete, userPath(created.ID), nil).empty(t, http.StatusNoContent)

	gone := "There is no user " + strconv.Itoa(created.ID) + "."
	require.Equal(t, gone, call(t, target, http.MethodGet, userPath(created.ID), nil).problem(t, http.StatusNotFound))
	require.Equal(t, gone, call(t, target, http.MethodPut, userPath(created.ID), newUser("Ada", "ada@example.com")).problem(t, http.StatusNotFound))
	require.Equal(t, gone, call(t, target, http.MethodDelete, userPath(created.ID), nil).problem(t, http.StatusNotFound))
}

func Pages(t testing.TB, target Target) {
	var all []user
	call(t, target, http.MethodGet, usersPath, nil).decode(t, http.StatusOK, &all)
	require.Empty(t, all)
	require.NotNil(t, all)

	for _, name := range []string{"Ada", "Grace", "Barbara"} {
		all = append(all, createUser(t, target, "", newUser(name, strings.ToLower(name)+"@example.com")))
	}

	cases := []struct {
		query string
		want  []user
	}{
		{"", all},
		{"?limit=2", all[:2]},
		{"?limit=2&offset=2", all[2:]},
		{"?offset=1", all[1:]},
		{"?offset=3", []user{}},
	}
	for _, c := range cases {
		var got []user
		call(t, target, http.MethodGet, usersPath+c.query, nil).decode(t, http.StatusOK, &got)
		require.Equal(t, c.want, got, c.query)
	}
}

func Retries(t testing.TB, target Target) {
	ada := createUser(t, target, "ada-1", newUser("Ada", "ada@example.com"))
	require.Equal(t, ada, createUser(t, target, "ada-1", newUser("Ada", "ada@example.com")))

	grace := createUser(t, target, "grace-1", newUser("Grace", "grace@example.com"))
	require.NotEqual(t, ada.ID, grace.ID)

	var all []user
	call(t, target, http.MethodGet, usersPath, nil).decode(t, http.StatusOK, &all)
	require.Equal(t, []user{ada, grace}, all)
}

func BadParameters(t testing.TB, target Target) {
	for _, path := range []string{
		usersPath + "/ada",
		usersPath + "?limit=0",
		usersPath + "?limit=101",
		usersPath + "?limit=ten",
		usersPath + "?offset=-1",
	} {
		call(t, target, http.MethodGet, path, nil).rejected(t)
	}

	b, err := json.Marshal(newUser("Ada", "ada@example.com"))
	require.NoError(t, err)
	header := http.Header{"Content-Type": {"application/json"}, "Idempotency-Key": {strings.Repeat("k", 65)}}
	send(t, target, http.MethodPost, usersPath, header, b).rejected(t)

	var all []user
	call(t, target, http.MethodGet, usersPath, nil).decode(t, http.StatusOK, &all)
	require.Empty(t, all)
}

func BadBodies(t testing.TB, target Target) {
	created := createUser(t, target, "", newUser("Ada", "ada@example.com"))

	for _, path := range []string{usersPath, userPath(created.ID)} {
		method := http.MethodPost
		if path != usersPath {
			method = http.MethodPut
		}
		sendBody(t, target, method, path, "application/json", []byte(`{"name":`)).problem(t, http.StatusBadRequest)
		sendBody(t, target, method, path, "text/plain", []byte("Ada")).problem(t, http.StatusUnsupportedMediaType)
		call(t, target, method, path, newUser("", "ada@example.com")).problem(t, http.StatusUnprocessableEntity)
		call(t, target, method, path, newUser("Ada", "not an email")).problem(t, http.StatusUnprocessableEntity)
		big := append(append([]byte(`{"name":"`), bytes.Repeat([]byte("a"), maxBodyBytes)...), `","email":"ada@example.com"}`...)
		sendBody(t, target, method, path, "application/json", big).problem(t, http.StatusRequestEntityTooLarge)
	}

	var all []user
	call(t, target, http.MethodGet, usersPath, nil).decode(t, http.StatusOK, &all)
	require.Equal(t, []user{created}, all)
}

func Unmatched(t testing.TB, target Target) {
	for _, path := range []string{"/", "/users", "/v1", "/v1/user", usersPath + "/"} {
		call(t, target, http.MethodGet, path, nil).status(t, http.StatusNotFound)
	}

	cases := []struct {
		method, path string
		allow        []string
	}{
		{http.MethodDelete, usersPath, []string{http.MethodGet, http.MethodPost}},
		{http.MethodPatch, userPath(1), []string{http.MethodDelete, http.MethodGet, http.MethodPut}},
	}
	for _, c := range cases {
		a := call(t, target, c.method, c.path, nil)
		a.status(t, http.StatusMethodNotAllowed)
		require.Equal(t, c.allow, a.allow(), "%s: Allow %q", a.request, a.header.Values("Allow"))
	}
}

const usersPath = "/v1/users"

func userPath(id int) string { return usersPath + "/" + strconv.Itoa(id) }

type user struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func newUser(name, email string) map[string]string {
	return map[string]string{"name": name, "email": email}
}

func createUser(t testing.TB, target Target, key string, body map[string]string) user {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	header := http.Header{"Content-Type": {"application/json"}}
	if key != "" {
		header.Set("Idempotency-Key", key)
	}
	var created user
	send(t, target, http.MethodPost, usersPath, header, b).decode(t, http.StatusCreated, &created)
	return created
}

type answer struct {
	request string
	code    int
	header  http.Header
	body    []byte
}

func call(t testing.TB, target Target, method, path string, body any) answer {
	t.Helper()
	if body == nil {
		return send(t, target, method, path, nil, nil)
	}
	b, err := json.Marshal(body)
	require.NoError(t, err)
	return sendBody(t, target, method, path, "application/json", b)
}

func sendBody(t testing.TB, target Target, method, path, contentType string, body []byte) answer {
	t.Helper()
	return send(t, target, method, path, http.Header{"Content-Type": {contentType}}, body)
}

func send(t testing.TB, target Target, method, path string, header http.Header, body []byte) answer {
	t.Helper()
	u, err := target.URL.Parse(path)
	require.NoError(t, err)
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, u.String(), r)
	require.NoError(t, err)
	maps.Copy(req.Header, header)
	resp, err := target.Client.Do(req)
	require.NoError(t, err, "%s %s", method, u)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "%s %s", method, u)
	return answer{request: method + " " + u.String(), code: resp.StatusCode, header: resp.Header, body: b}
}

func (a answer) status(t testing.TB, code int) {
	t.Helper()
	require.Equal(t, code, a.code, "%s: %s", a.request, a.body)
}

func (a answer) empty(t testing.TB, code int) {
	t.Helper()
	a.status(t, code)
	require.Empty(t, a.body, a.request)
}

func (a answer) decode(t testing.TB, code int, v any) {
	t.Helper()
	a.status(t, code)
	require.Equal(t, "application/json", a.mediaType(t), a.request)
	require.NoError(t, json.Unmarshal(a.body, v), "%s: %s", a.request, a.body)
}

func (a answer) rejected(t testing.TB) {
	t.Helper()
	require.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, a.code, "%s: %s", a.request, a.body)
	a.problem(t, a.code)
}

func (a answer) problem(t testing.TB, code int) string {
	t.Helper()
	a.status(t, code)
	require.Equal(t, "application/problem+json", a.mediaType(t), a.request)
	var p struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal(a.body, &p), "%s: %s", a.request, a.body)
	require.Contains(t, []string{"", "about:blank"}, p.Type, "%s: %s", a.request, a.body)
	require.Equal(t, http.StatusText(code), p.Title, "%s: %s", a.request, a.body)
	require.Equal(t, code, p.Status, "%s: %s", a.request, a.body)
	return p.Detail
}

func (a answer) mediaType(t testing.TB) string {
	t.Helper()
	m, _, err := mime.ParseMediaType(a.header.Get("Content-Type"))
	require.NoError(t, err, "%s: Content-Type %q", a.request, a.header.Get("Content-Type"))
	return m
}

func (a answer) allow() []string {
	var methods []string
	for _, v := range a.header.Values("Allow") {
		for m := range strings.SplitSeq(v, ",") {
			if m = strings.TrimSpace(m); m != "" && m != http.MethodHead && m != http.MethodOptions {
				methods = append(methods, m)
			}
		}
	}
	slices.Sort(methods)
	return methods
}
