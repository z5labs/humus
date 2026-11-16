// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/inlineedit/contacts"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/hypermedia"
	"github.com/z5labs/humus/sse"
)

//go:embed static
var files embed.FS

//go:embed templates
var templates embed.FS

var tmpl = template.Must(template.New("datastar").Funcs(template.FuncMap{
	"asset": func(name string) string {
		return contacts.Static.URL(hypermedia.Arg(contacts.Asset, name)).String()
	},
}).ParseFS(templates, "templates/*.html"))

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(files, "static")
	if err != nil {
		return nil, err
	}
	ui := UI{Store: contacts.NewStore(), Static: static, WriteTimeout: settings.WriteTimeout}
	site, err := hypermedia.NewSite(s, contacts.ErrorPage, ui.Routes())
	if err != nil {
		return nil, err
	}
	svc, err := httpserver.NewService(s, settings, http.NewCrossOriginProtection().Handler(site))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

func readSettings(ctx context.Context, s *humus.Setup) (httpserver.Settings, error) {
	d := httpserver.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	return config.Struct(func(r *config.Resolver) httpserver.Settings {
		return httpserver.Settings{
			Addr:              r.Get(env("ADDR").Validate(httpserver.CheckAddr)),
			TLS:               r.Get(env("TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httpserver.CheckTLS)),
			HTTP2Only:         r.Get(env("HTTP2_ONLY").Parse(strconv.ParseBool).Default(d.HTTP2Only)),
			ReadHeaderTimeout: r.Get(env("READ_HEADER_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadHeaderTimeout).Validate(httpserver.CheckReadHeaderTimeout)),
			ReadTimeout:       r.Get(env("READ_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadTimeout).Validate(httpserver.CheckReadTimeout)),
			WriteTimeout:      r.Get(env("WRITE_TIMEOUT").Parse(time.ParseDuration).Default(d.WriteTimeout).Validate(httpserver.CheckWriteTimeout)),
			IdleTimeout:       r.Get(env("IDLE_TIMEOUT").Parse(time.ParseDuration).Default(d.IdleTimeout).Validate(httpserver.CheckIdleTimeout)),
			MaxHeaderBytes:    r.Get(env("MAX_HEADER_BYTES").Parse(strconv.Atoi).Default(d.MaxHeaderBytes).Validate(httpserver.CheckMaxHeaderBytes)),
		}
	}).Read(ctx)
}

func readFile(fsys humus.FS) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}

func serverTLS(pem []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(pem, pem)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

type UI struct {
	Store        *contacts.Store
	Static       fs.FS
	WriteTimeout time.Duration
}

func (ui UI) Routes() []hypermedia.Route {
	return []hypermedia.Route{
		hypermedia.NewRoute(http.MethodGet, contacts.Static, contacts.Assets(ui.Static)),
		hypermedia.NewRoute(http.MethodGet, contacts.List, ui.list),
		hypermedia.NewRoute(http.MethodGet, contacts.Edit, ui.edit),
		hypermedia.NewRoute(http.MethodPut, contacts.One, ui.save),
		hypermedia.NewRoute(http.MethodGet, contacts.Changes, ui.changes),
	}
}

type row struct {
	contacts.Contact
	SaveURL  string
	EditURL  string
	Editing  bool
	Problems contacts.Problems
}

func viewRow(c contacts.Contact) row {
	id := hypermedia.Arg(contacts.ContactID, c.ID)
	return row{
		Contact: c,
		SaveURL: contacts.One.URL(id).String(),
		EditURL: contacts.Edit.URL(id).String(),
	}
}

func editRow(c contacts.Contact, name, email string, problems contacts.Problems) row {
	r := viewRow(c)
	r.Name, r.Email = name, email
	r.Editing = true
	r.Problems = problems
	return r
}

type page struct {
	FeedURL string
	Rows    []row
}

func newPage(rows ...row) page {
	return page{FeedURL: contacts.Changes.URL().String(), Rows: rows}
}

func render(w http.ResponseWriter, name string, data any) error {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return err
	}
	contacts.HTML(w, http.StatusOK)
	_, err := b.WriteTo(w)
	return err
}

func patchEvent(name string, data any) (sse.Event, error) {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return sse.Event{}, err
	}
	return sse.Event{Name: "datastar-patch-elements", Data: elements(b.String())}, nil
}

var newlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

func elements(html string) string {
	lines := strings.Split(newlines.Replace(html), "\n")
	for i, line := range lines {
		lines[i] = "elements " + line
	}
	return strings.Join(lines, "\n")
}

func (ui UI) patch(w http.ResponseWriter, name string, data any) error {
	e, err := patchEvent(name, data)
	if err != nil {
		return err
	}
	stream, err := sse.NewStream(w, ui.WriteTimeout)
	if err != nil {
		return err
	}
	return stream.Send(e)
}

func isDatastar(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "Datastar-Request")
	return r.Header.Get("Datastar-Request") == "true"
}

func (ui UI) list(w http.ResponseWriter, r *http.Request) error {
	var rows []row
	for _, c := range ui.Store.All() {
		rows = append(rows, viewRow(c))
	}
	return render(w, "page", newPage(rows...))
}

func (ui UI) edit(w http.ResponseWriter, r *http.Request) error {
	c, err := ui.Store.Get(hypermedia.ParamFrom(r, contacts.ContactID))
	if err != nil {
		return err
	}
	form := editRow(c, c.Name, c.Email, nil)
	if isDatastar(w, r) {
		return ui.patch(w, "edit", form)
	}
	return render(w, "page", newPage(form))
}

func (ui UI) save(w http.ResponseWriter, r *http.Request) error {
	c, err := ui.Store.Get(hypermedia.ParamFrom(r, contacts.ContactID))
	if err != nil {
		return err
	}
	form, err := hypermedia.ReadForm(r, contacts.Name, contacts.Email)
	if err != nil {
		return err
	}
	if !form.Valid() {
		return ui.patch(w, "edit", editRow(c, form.Raw(contacts.Name), form.Raw(contacts.Email), contacts.ProblemsIn(form)))
	}
	c.Name = hypermedia.FieldFrom(form, contacts.Name)
	c.Email = hypermedia.FieldFrom(form, contacts.Email)
	ui.Store.Put(c)
	return ui.patch(w, "row", viewRow(c))
}

func (ui UI) changes(w http.ResponseWriter, r *http.Request) error {
	changed, unsubscribe := ui.Store.Subscribe()
	defer unsubscribe()
	w.Header().Set("Cache-Control", "no-store")
	stream, err := sse.NewStream(w, ui.WriteTimeout)
	if err != nil {
		return err
	}
	stop := httpserver.ShuttingDown(r.Context())
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-stop:
			panic(http.ErrAbortHandler)
		case <-r.Context().Done():
			return nil
		case c := <-changed:
			e, err := patchEvent("row", viewRow(c))
			if err != nil {
				return err
			}
			if err := stream.Send(e); err != nil {
				return err
			}
		case <-keepAlive.C:
			if err := stream.Comment("keep-alive"); err != nil {
				return err
			}
		}
	}
}
