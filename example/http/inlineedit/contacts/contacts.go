// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package contacts is what inlineedit's UIs share: their paths, their form's fields, their store, their error page and their assets.
package contacts

import (
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sync"

	"github.com/z5labs/humus/hypermedia"
)

type ID int

var (
	ContactID = hypermedia.PathParam[ID]("contactId").Minimum(1)
	List      = hypermedia.Root().Lit("contacts")
	One       = List.Param(ContactID)
	Edit      = One.Lit("edit")
	Changes   = List.Lit("changes")
	Asset     = hypermedia.PathParam[string]("file")
	Static    = hypermedia.Root().Lit("static").Rest(Asset)
)

type Contact struct {
	ID    ID
	Name  string
	Email string
}

var (
	Name  = hypermedia.Field[string]("name").Required().Pattern(regexp.MustCompile(`\S`))
	Email = hypermedia.Field[string]("email").Required().Pattern(regexp.MustCompile(`^[^@\s]+@[^@\s]+$`))
)

type Problems map[string]string

var messages = map[string]map[hypermedia.Rule]string{
	"name": {
		hypermedia.Repeated:        "Enter one name.",
		hypermedia.Missing:         "Enter a name.",
		hypermedia.PatternMismatch: "Enter a name.",
	},
	"email": {
		hypermedia.Repeated:        "Enter one email address.",
		hypermedia.Missing:         "Enter an email address.",
		hypermedia.PatternMismatch: "Enter an email address.",
	},
}

func ProblemsIn(form *hypermedia.Form) Problems {
	problems := make(Problems)
	for _, field := range []hypermedia.AnyField{Name, Email} {
		if p := form.Problem(field); p != nil {
			problems[p.Field] = messages[p.Field][p.Rule]
		}
	}
	return problems
}

type Store struct {
	mu          sync.Mutex
	contacts    map[ID]Contact
	subscribers map[*subscriber]struct{}
}

func NewStore() *Store {
	return &Store{
		contacts: map[ID]Contact{
			1: {ID: 1, Name: "Joe Smith", Email: "joe@example.com"},
			2: {ID: 2, Name: "Angie MacDowell", Email: "angie@example.com"},
			3: {ID: 3, Name: "Fuqua Tarkenton", Email: "fuqua@example.com"},
		},
		subscribers: make(map[*subscriber]struct{}),
	}
}

func (s *Store) All() []Contact {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := slices.Sorted(maps.Keys(s.contacts))
	all := make([]Contact, 0, len(ids))
	for _, id := range ids {
		all = append(all, s.contacts[id])
	}
	return all
}

func (s *Store) Get(id ID) (Contact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.contacts[id]
	if !ok {
		return Contact{}, &hypermedia.RequestError{Status: http.StatusNotFound, Param: "contactId", Err: fmt.Errorf("no contact has the id %d", id)}
	}
	return c, nil
}

func (s *Store) Put(c Contact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contacts[c.ID] = c
	for sub := range s.subscribers {
		sub.add(c)
	}
}

func (s *Store) Subscribe() (<-chan Contact, func()) {
	sub := &subscriber{
		wake: make(chan struct{}, 1),
		out:  make(chan Contact),
		done: make(chan struct{}),
	}
	s.mu.Lock()
	s.subscribers[sub] = struct{}{}
	s.mu.Unlock()
	go sub.pump()
	var once sync.Once
	return sub.out, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subscribers, sub)
			s.mu.Unlock()
			close(sub.done)
		})
	}
}

type subscriber struct {
	mu      sync.Mutex
	pending []Contact
	wake    chan struct{}
	out     chan Contact
	done    chan struct{}
}

func (sub *subscriber) add(c Contact) {
	sub.mu.Lock()
	sub.pending = append(sub.pending, c)
	sub.mu.Unlock()
	select {
	case sub.wake <- struct{}{}:
	default:
	}
}

func (sub *subscriber) pump() {
	for {
		sub.mu.Lock()
		if len(sub.pending) == 0 {
			sub.mu.Unlock()
			select {
			case <-sub.wake:
				continue
			case <-sub.done:
				return
			}
		}
		c := sub.pending[0]
		sub.pending = sub.pending[1:]
		sub.mu.Unlock()
		select {
		case sub.out <- c:
		case <-sub.done:
			return
		}
	}
}

func HTML(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
}

var errorTmpl = template.Must(template.New("error").Parse(`<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>{{.}}</title></head>
<body><h1>{{.}}</h1></body>
</html>
`))

func ErrorPage(w http.ResponseWriter, r *http.Request, err error) error {
	status := http.StatusInternalServerError
	if re, ok := errors.AsType[*hypermedia.RequestError](err); ok {
		status = re.Status
	}
	HTML(w, status)
	return errorTmpl.Execute(w, http.StatusText(status))
}

func MethodOverride(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			switch m := r.PostFormValue("_method"); m {
			case http.MethodPut, http.MethodPatch, http.MethodDelete:
				r = r.WithContext(r.Context())
				r.Method = m
			}
		}
		next.ServeHTTP(w, r)
	})
}

func Assets(fsys fs.FS) hypermedia.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		name := hypermedia.ParamFrom(r, Asset)
		info, err := fs.Stat(fsys, name)
		if err == nil && info.IsDir() {
			err = fmt.Errorf("%q is a directory", name)
		}
		if err != nil {
			return &hypermedia.RequestError{Status: http.StatusNotFound, Param: "file", Err: err}
		}
		http.ServeFileFS(w, r, fsys, name)
		return nil
	}
}
