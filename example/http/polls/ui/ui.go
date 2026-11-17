// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package ui is the polls service's hypermedia UI: its Site, its error page, its pages and the scripts they load.
package ui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/hypermedia"
)

//go:embed static
var static embed.FS

//go:embed templates
var templates embed.FS

const csp = "default-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

var (
	scriptHash = hypermedia.PathParam[string]("hash")
	scriptName = hypermedia.PathParam[string]("script")
	scriptPath = hypermedia.Root().Lit("scripts").Param(scriptHash).Rest(scriptName)
)

type UI struct {
	files   fs.FS
	hashes  map[string]string
	pages   map[string]*template.Template
}

func New() (*UI, error) {
	files, err := fs.Sub(static, "static")
	if err != nil {
		return nil, err
	}
	return build(files, templates)
}

func build(files, tmpls fs.FS) (*UI, error) {
	ui := &UI{files: files, hashes: make(map[string]string), pages: make(map[string]*template.Template)}
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		site.ServeHTTP(w, r)
	}), nil
}

func (ui *UI) routes() []hypermedia.Route {
	return []hypermedia.Route{
		hypermedia.NewRoute(http.MethodGet, scriptPath, ui.script),
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
	t, ok := ui.pages[page]
	if !ok {
		return fmt.Errorf("%q isn't a page", page)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := b.WriteTo(w)
	return err
}

type errorView struct {
	Title   string
	Message string
}

var messages = map[int]string{
	http.StatusBadRequest:            "The request couldn't be read.",
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
