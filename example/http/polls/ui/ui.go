// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package ui is the polls service's hypermedia UI: its Site, its error page, its pages and the scripts they load.
package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/hypermedia"
)

//go:embed static
var static embed.FS

//go:embed templates
var templates embed.FS

const (
	csp          = "default-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	maxBodyBytes = 16 << 10
)

var (
	scriptHash = hypermedia.PathParam[string]("hash")
	scriptName = hypermedia.PathParam[string]("script")
	scriptPath = hypermedia.Root().Lit("scripts").Param(scriptHash).Rest(scriptName)
	pollID     = hypermedia.PathParam[string]("pollId").Pattern(regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`))
	homePath   = hypermedia.Root()
	pollsPath  = hypermedia.Root().Lit("polls")
	pollPath   = pollsPath.Param(pollID)
	votesPath  = pollPath.Lit("votes")
	feedPath   = pollPath.Lit("feed")
)

type Store interface {
	CreatePoll(ctx context.Context, question string, options []string) (uuid.UUID, error)
	Poll(ctx context.Context, id uuid.UUID) (store.Poll, error)
	Vote(ctx context.Context, pollID uuid.UUID, optionID int64, voter string) error
}

type Results interface {
	Subscribe(pollID uuid.UUID) (<-chan store.Poll, func())
}

type UI struct {
	polls        Store
	results      Results
	writeTimeout time.Duration
	files        fs.FS
	hashes       map[string]string
	pages        map[string]*template.Template
}

func New(polls Store, results Results, writeTimeout time.Duration) (*UI, error) {
	files, err := fs.Sub(static, "static")
	if err != nil {
		return nil, err
	}
	ui, err := build(polls, files, templates)
	if err != nil {
		return nil, err
	}
	ui.results = results
	ui.writeTimeout = writeTimeout
	return ui, nil
}

func build(polls Store, files, tmpls fs.FS) (*UI, error) {
	ui := &UI{polls: polls, files: files, hashes: make(map[string]string), pages: make(map[string]*template.Template)}
	names, err := fs.Glob(files, "*.js")
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		b, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		ui.hashes[name] = hex.EncodeToString(sum[:])
	}
	pages, err := fs.Glob(tmpls, "templates/*.html")
	if err != nil {
		return nil, err
	}
	funcs := template.FuncMap{"script": ui.scriptURL}
	for _, file := range pages {
		name := path.Base(file)
		if name == "layout.html" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(tmpls, "templates/layout.html", file)
		if err != nil {
			return nil, err
		}
		ui.pages[name] = t
	}
	return ui, nil
}

func (ui *UI) scriptURL(name string) (string, error) {
	hash, ok := ui.hashes[name]
	if !ok {
		return "", fmt.Errorf("%q isn't a script", name)
	}
	return scriptPath.URL(hypermedia.Arg(scriptHash, hash), hypermedia.Arg(scriptName, name)).String(), nil
}

func (ui *UI) Site(s *humus.Setup) (http.Handler, error) {
	site, err := hypermedia.NewSite(s, ui.ErrorPage, ui.routes())
	if err != nil {
		return nil, err
	}
	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(ui.crossOrigin))
	protected := protection.Handler(http.MaxBytesHandler(site, maxBodyBytes))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		protected.ServeHTTP(w, r)
	}), nil
}

func (ui *UI) crossOrigin(w http.ResponseWriter, r *http.Request) {
	err := ui.ErrorPage(w, r, &hypermedia.RequestError{Status: http.StatusForbidden, Err: errors.New("a cross-origin request")})
	if err != nil {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	}
}

func (ui *UI) routes() []hypermedia.Route {
	return []hypermedia.Route{
		hypermedia.NewRoute(http.MethodGet, scriptPath, ui.script),
		hypermedia.NewRoute(http.MethodGet, homePath, ui.home),
		hypermedia.NewRoute(http.MethodPost, pollsPath, ui.create),
		hypermedia.NewRoute(http.MethodGet, pollPath, ui.poll),
		hypermedia.NewRoute(http.MethodPost, votesPath, ui.vote),
		hypermedia.NewRoute(http.MethodGet, feedPath, ui.feed),
	}
}

func (ui *UI) script(w http.ResponseWriter, r *http.Request) error {
	name := hypermedia.ParamFrom(r, scriptName)
	hash, ok := ui.hashes[name]
	if !ok {
		return &hypermedia.RequestError{Status: http.StatusNotFound, Param: "script", Err: fmt.Errorf("%q isn't a script", name)}
	}
	if got := hypermedia.ParamFrom(r, scriptHash); got != hash {
		return &hypermedia.RequestError{Status: http.StatusNotFound, Param: "hash", Err: fmt.Errorf("%s isn't the hash of %s", got, name)}
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFileFS(w, r, ui.files, name)
	return nil
}

func (ui *UI) render(w http.ResponseWriter, status int, page string, data any) error {
	return ui.renderTemplate(w, status, page, page, data)
}

func (ui *UI) execute(page, name string, data any) (*bytes.Buffer, error) {
	t, ok := ui.pages[page]
	if !ok {
		return nil, fmt.Errorf("%q isn't a page", page)
	}
	var b bytes.Buffer
	if err := t.ExecuteTemplate(&b, name, data); err != nil {
		return nil, err
	}
	return &b, nil
}

func (ui *UI) renderTemplate(w http.ResponseWriter, status int, page, name string, data any) error {
	b, err := ui.execute(page, name, data)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err = b.WriteTo(w)
	return err
}

type errorView struct {
	Title   string
	Message string
}

var messages = map[int]string{
	http.StatusBadRequest:            "The request couldn't be read.",
	http.StatusForbidden:             "Only this site's pages can send that.",
	http.StatusNotFound:              "There's no such page.",
	http.StatusMethodNotAllowed:      "This page can't do that.",
	http.StatusRequestEntityTooLarge: "What was sent is too large.",
	http.StatusUnsupportedMediaType:  "What was sent isn't a form.",
	http.StatusServiceUnavailable:    "The polls can't be reached right now. Try again in a moment.",
	http.StatusInternalServerError:   "Something went wrong on our side.",
}

func (ui *UI) ErrorPage(w http.ResponseWriter, r *http.Request, err error) error {
	status := http.StatusInternalServerError
	if re, ok := errors.AsType[*hypermedia.RequestError](err); ok {
		status = re.Status
	} else if errors.Is(err, store.ErrUnavailable) {
		status = http.StatusServiceUnavailable
	}
	message, ok := messages[status]
	if !ok {
		message = http.StatusText(status) + "."
	}
	return ui.render(w, status, "error.html", errorView{Title: http.StatusText(status), Message: message})
}
