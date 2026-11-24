# Serving a hypermedia UI

The [`hypermedia`](https://pkg.go.dev/github.com/z5labs/humus/hypermedia)
package serves the pages, fragments and actions of a hypermedia-driven UI, as
the `http.Handler` that [`httpserver`](/humus/pr-preview/pr-1629/services/http/serving/)
serves. The [`sse`](https://pkg.go.dev/github.com/z5labs/humus/sse) package
writes its event streams. Neither knows a client library: the headers,
attributes, events and status codes a library reads are the end user's, and
[Your library decides](#your-library-decides) shows them.

The code on this page is
[`example/http/inlineedit`](https://github.com/z5labs/humus/tree/main/example/http/inlineedit),
a list of contacts edited in place, with a live feed of every saved row. It
serves the same UI twice: `htmxui` with htmx, and `datastarui` with Datastar.
Both share the
[`contacts`](https://github.com/z5labs/humus/blob/main/example/http/inlineedit/contacts/contacts.go)
package: the paths, the form's fields, the store, the error page and the
assets.

The design is in
[Proposal: Hypermedia UIs](https://github.com/z5labs/humus/discussions/1454),
with its [revision 2](https://github.com/z5labs/humus/discussions/1454#discussioncomment-19621172)
and [revision 3](https://github.com/z5labs/humus/discussions/1454#discussioncomment-19621325).

The [polls walkthrough](/humus/pr-preview/pr-1629/walkthroughs/http/polls/) builds a
production-ready hypermedia UI on both packages, step by step, with htmx,
forms, and live results on every replica, and its code is
[`example/http/polls`](https://github.com/z5labs/humus/tree/main/example/http/polls).

## The Site

The end user's `Init` builds a
[`Site`](https://pkg.go.dev/github.com/z5labs/humus/hypermedia#Site) from its
error page and its routes, and serves it with `httpserver.NewService`:

```go
ui := UI{Store: contacts.NewStore(), Static: static, WriteTimeout: settings.WriteTimeout}
site, err := hypermedia.NewSite(s, contacts.ErrorPage, ui.Routes())
if err != nil {
	return nil, err
}
svc, err := httpserver.NewService(ctx, s, h, settings, http.NewCrossOriginProtection().Handler(site))
```

A route is one method on one path, and its handler:

```go
func (ui UI) Routes() []hypermedia.Route {
	return []hypermedia.Route{
		hypermedia.NewRoute(http.MethodGet, contacts.Static, contacts.Assets(ui.Static)),
		hypermedia.NewRoute(http.MethodGet, contacts.List, ui.list),
		hypermedia.NewRoute(http.MethodGet, contacts.One, ui.show),
		hypermedia.NewRoute(http.MethodGet, contacts.Edit, ui.edit),
		hypermedia.NewRoute(http.MethodPut, contacts.One, ui.save),
		hypermedia.NewRoute(http.MethodGet, contacts.Changes, ui.changes),
	}
}
```

`NewSite` checks every path and route first, and returns one error that
names every mistake. A GET route also serves HEAD, and a method may be any
token, such as QUERY. A handler gets `w` and `r` and returns an error: there
is no render interface, so html/template, templ, gomponents and a library's
own helpers all write to `w` as they would on net/http.

**A browser never sees ServeMux's plain-text 404 or 405.** The end user's
`ErrorPage` answers them, with a
[`*hypermedia.RequestError`](https://pkg.go.dev/github.com/z5labs/humus/hypermedia#RequestError):

- a path no route serves, or whose value doesn't parse or breaks a
  constraint, is a 404, as no such page exists: `/contacts/0` under
  `Minimum(1)` is a 404, not a 400;
- a method its path doesn't have is a 405, with `Allow` already set on `w`.
  `Allow` lists the methods of the routes that match the path and whose
  values parse, so `/contacts/changes` allows only `GET, HEAD`, although
  `/contacts/{contactId}` beside it allows `GET, HEAD, PUT`.

A handler returns a `RequestError` for the client's own mistake, such as a
contact that doesn't exist, and the error page answers it in the same way:

```go
c, ok := s.contacts[id]
if !ok {
	return Contact{}, &hypermedia.RequestError{Status: http.StatusNotFound, Param: "contactId", Err: fmt.Errorf("no contact has the id %d", id)}
}
```

The error page is the end user's, and answers whatever status the error
asks for:

```go
func ErrorPage(w http.ResponseWriter, r *http.Request, err error) error {
	status := http.StatusInternalServerError
	if re, ok := errors.AsType[*hypermedia.RequestError](err); ok {
		status = re.Status
	}
	HTML(w, status)
	return errorTmpl.Execute(w, http.StatusText(status))
}
```

**The Site records by error, not by status.** Every error but a
`RequestError` is recorded, whatever status the page writes, and every panic
but `http.ErrAbortHandler`: as an exception event on the request's span, its
status set to Error, and one ERROR record through the logger
`github.com/z5labs/humus/hypermedia`. An error that comes once the client has
gone away, its request's context done or a write failing on its lost
connection, goes on the span alone, with no record, so a closed tab isn't
logged as the server's failure. A write the server itself refused is logged
whatever the context, and so is every panic. A 404 or a 422 the end user
answers on purpose is an answer, not a failure, and records nothing. The
error page's
own error or panic is recorded as `error page failed`, and the Site then
answers a bare 500 if the page wrote nothing. The Site adds no instrument or
attribute of its own.

**The route names the span,** such as `PUT /contacts/{contactId}`, whatever
the handlers above the Site pass on a copy of the request. So latency alerts
and dashboards group by route rather than by URL.

## Links

A [`Path`](https://pkg.go.dev/github.com/z5labs/humus/hypermedia#Path) is a
Go value built from `Root` one segment at a time, not a pattern string. **The
same value routes a request and builds its links,** so every `href`, form
action and `Location` comes from the path that serves it:

```go
var (
	ContactID = hypermedia.PathParam[ID]("contactId").Minimum(1)
	List      = hypermedia.Root().Lit("contacts")
	One       = List.Param(ContactID)
	Edit      = One.Lit("edit")
	Changes   = List.Lit("changes")
	Asset     = hypermedia.PathParam[string]("file")
	Static    = hypermedia.Root().Lit("static").Rest(Asset)
)
```

`Path.URL` builds a link with one `Arg` for each of the path's parameters, in
any order. An `Arg` has its parameter's type, so an ID of the wrong type
doesn't compile:

```go
func viewRow(c contacts.Contact) row {
	id := hypermedia.Arg(contacts.ContactID, c.ID)
	return row{
		Contact: c,
		ShowURL: contacts.One.URL(id).String(),
		EditURL: contacts.Edit.URL(id).String(),
	}
}
```

A handler reads the same parameter back, parsed and checked, with
`hypermedia.ParamFrom(r, contacts.ContactID)`.

- **A missing, repeated or extra `Arg` panics,** naming the path and the
  parameter, so the first test that renders the link finds it.
- **A link out of bounds is built, and leads to the 404 page.** An `Arg` isn't
  checked against its parameter's constraints, so `contacts.One` with an ID
  of 0 builds `/contacts/0`, which the error page answers as a 404.
- **Each value is escaped as one path segment,** so a `/` in it is `%2F`. A
  `Rest` value keeps its slashes, and each segment between them is escaped.
- **`With` and `QueryFrom` share a query parameter.** A
  `hypermedia.QueryParam[int]("page")` adds `?page=2` to a link with
  `URL.With`, escaped, and the handler reads it back as an `int` with
  `hypermedia.QueryFrom`. It gets the zero value when the query doesn't have
  it, and a `RequestError` of Status 400, which the error page answers, when
  it doesn't parse or appears twice.

## Forms

**A form's fields are declared once,** as
[`hypermedia.Field`](https://pkg.go.dev/github.com/z5labs/humus/hypermedia#Field)
values with `Required` and the constraints the form's rules need, so both
UIs read the same form:

```go
var (
	Name  = hypermedia.Field[string]("name").Required().Pattern(regexp.MustCompile(`\S`))
	Email = hypermedia.Field[string]("email").Required().Pattern(regexp.MustCompile(`^[^@\s]+@[^@\s]+$`))
)
```

A handler reads the body with
[`ReadForm`](https://pkg.go.dev/github.com/z5labs/humus/hypermedia#ReadForm),
and each value with `FieldFrom`, parsed and checked. htmxui's `save`:

```go
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
		return render(w, http.StatusUnprocessableEntity, "edit", editRow(c, form.Raw(contacts.Name), form.Raw(contacts.Email), contacts.ProblemsIn(form)))
	}
	c.Name = hypermedia.FieldFrom(form, contacts.Name)
	c.Email = hypermedia.FieldFrom(form, contacts.Email)
	ui.Store.Put(c)
	w.Header().Set("HX-Trigger", "contactSaved")
	return render(w, http.StatusOK, "row", viewRow(c))
}
```

**A problem is the form's, not an error.** A field that breaks a rule, such
as `Missing` or `PatternMismatch`, has a `Problem`, whose `Rule` gets a
message the end user writes. `Raw` gives the value the client sent, to
re-render:

```go
"name": {
	hypermedia.Repeated:        "Enter one name.",
	hypermedia.Missing:         "Enter a name.",
	hypermedia.PatternMismatch: "Enter a name.",
},
```

```go
func ProblemsIn(form *hypermedia.Form) Problems {
	problems := make(Problems)
	for _, field := range []hypermedia.AnyField{Name, Email} {
		if p := form.Problem(field); p != nil {
			problems[p.Field] = messages[p.Field][p.Rule]
		}
	}
	return problems
}
```

The status a form with problems gets is the library's: a 422 for htmx, and a
200 patch for Datastar, as [Your library decides](#your-library-decides)
shows.

- **Any method's form body is read,** a PUT's or a QUERY's as a POST's,
  urlencoded or multipart. A form that method-override middleware already
  parsed, as [Unpoly's method override](#unpolys-method-override) does, is
  read from `r.PostForm`.
- **A checkbox is a bool field,** true when present.
- **File parts are skipped.** Nothing is written to disk, so an upload is
  streamed with `r.MultipartReader()` and never spilled outside `humus.Host`.
- **A body ReadForm can't read gets the error page.** One over
  `http.MaxBytesHandler`'s limit is a 413, and one that isn't a form a 415.
  Each is a `RequestError`, so `save` returns it and nothing is recorded.
- **A field sent twice is `Repeated`.** A checkbox group's or a
  `<select multiple>`'s values are read from `r.PostForm[name]`.

## When the error page answers

A handler's `w` has net/http's semantics: each `WriteHeader`, `Write` and
`Flush` goes straight to the response, and nothing is held. The Site watches
only whether the response has started:

- **Before it starts,** a handler's error or panic gets the error page,
  without the headers and cookies the handler set, so a library header or a
  cookie meant for a success doesn't ride on it. Headers set above the Site,
  such as a middleware's `Vary`, are kept.
- **After it starts, the response is the handler's.** An error ends it as
  written, and is recorded unless it is a `RequestError`, which the Site
  never records. A panic aborts it, so the client sees a network error rather
  than a page that looks whole.

**A render that can fail halfway renders into a buffer first.** Then a
template's error comes before the first write, and gets the error page, and
the page that is sent is whole:

```go
func render(w http.ResponseWriter, status int, name string, data any) error {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return err
	}
	contacts.HTML(w, status)
	_, err := b.WriteTo(w)
	return err
}
```

Streaming the render instead is the end user's choice, or a framework's: a
long page reaches the browser sooner, and a failure halfway leaves it cut
short.

**A body with no Content-Type is sniffed,** as for any net/http handler, so
`<!DOCTYPE html>`, `<div>`, `<p>` and `<table>` go out as `text/html`. A
fragment that opens with any other tag, such as a `<tr>`, an `<li>`, a
`<span>` or the `<form>` of inlineedit's edit row, goes out as `text/plain`,
and a library that swaps only HTML ignores it. So every response sets its
Content-Type:

```go
func HTML(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
}
```

## Streams

A feed is a handler that writes an event stream with
[`sse`](https://pkg.go.dev/github.com/z5labs/humus/sse) and watches
`httpserver.ShuttingDown`, so the Service's `Shutdown` doesn't wait on it to
the end of the grace period. htmxui's feed sends each saved row:

```go
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
			return nil
		case <-r.Context().Done():
			return nil
		case c := <-changed:
			oob := viewRow(c)
			oob.OOB = true
			var b bytes.Buffer
			if err := tmpl.ExecuteTemplate(&b, "row", oob); err != nil {
				return err
			}
			if err := stream.Send(sse.Event{Name: "changed", Data: b.String()}); err != nil {
				return err
			}
		case <-keepAlive.C:
			if err := stream.Comment("keep-alive"); err != nil {
				return err
			}
		}
	}
}
```

**How the stream ends is the handler's.** Returning ends it cleanly, and
htmx's SSE extension, like the browser's `EventSource`, reconnects after a
clean end. Datastar's client reconnects after a network error instead, so
datastarui's feed aborts it:

```go
case <-stop:
	panic(http.ErrAbortHandler)
```

The Site passes `http.ErrAbortHandler` through without recording it, so the
abort is an ending, not a failure. The stream has started by then, so neither
ending gets the error page.

[Event streams](/humus/pr-preview/pr-1629/services/http/serving/#event-streams), in
Serving HTTP, covers the rest: the write deadline that `sse.NewStream` sets
and clears around each write, why it takes the Settings' `WriteTimeout`, and
the span and the metrics a stream records.

## CSRF and security headers

**`http.CrossOriginProtection` goes around the Site,** as inlineedit's
`Init` above puts it. A request whose method isn't GET, HEAD or OPTIONS gets
a 403 when its `Sec-Fetch-Site` or its `Origin` says it came from another
origin. It needs no token, so the pages carry none, and a cookie-authenticated
action can't be forged from another site. A GET is never checked, so every
change of state is a PUT, a POST or a DELETE. Its 403 is net/http's plain
text: it is outside the Site, so the error page doesn't answer it.

A browser signed in with a session cookie is
[Browser sessions](/humus/pr-preview/pr-1629/hardening/auth/#browser-sessions), with
`auth.APIKeyCookie` behind the same `http.CrossOriginProtection`.

**Security headers are the end user's.** humus sets no
`Content-Security-Policy`, `Strict-Transport-Security`,
`X-Content-Type-Options`, `Referrer-Policy` or `frame-ancestors`, because each
one depends on the pages: a CSP that allows the end user's scripts, styles
and inline handlers, and only those, is a property of the UI and of its
library. A middleware around the Site sets them, and since the Site keeps
headers set above it, they reach the error page too. Some libraries evaluate
attribute expressions, and need a CSP that allows it; their docs say which.

## Static assets

A path can end in a `Rest` segment, which takes the rest of the path, so the
assets are one more route: `contacts.Static` is `/static/{file...}`, with
`contacts.Asset` the rest:

```go
Asset     = hypermedia.PathParam[string]("file")
Static    = hypermedia.Root().Lit("static").Rest(Asset)
```

Its handler serves the file with `http.ServeFileFS`, from the `embed.FS` the
example builds into its binary:

```go
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
```

The `fs.Stat` is the end user's choice: a missing asset gets the error page
rather than `ServeFileFS`'s plain-text 404, and a directory isn't listed.
`ServeFileFS` answers `Range` and sets the Content-Type from the file's
extension.

**Caching, fingerprinting and compression are the end user's, or a
framework's.** humus has no asset handler. An `embed.FS` file has no
modification time, so `ServeFileFS` sends no `Last-Modified` and a browser
has nothing to revalidate with. A `Cache-Control` header, an `ETag`, or a
fingerprinted name such as `app.3f2a1c.js` with a long `max-age`, is a
policy the end user picks for their own assets.

## Compression's traps

humus compresses nothing. A compressing middleware around the Site is the
end user's, and has four traps:

- **An event stream stops arriving.** A gzip writer holds its output until
  its buffer fills, so events sit in it, and keep-alive comments too, until
  the connection is cut. A middleware flushes the gzip writer on every
  `Flush`, or leaves `text/event-stream` responses alone.
- **A wrapper hides the connection.** `sse.NewStream` sets its write deadline
  through `http.ResponseController`, which finds the connection by calling
  `Unwrap` on each wrapper. A wrapper with no `Unwrap` makes `NewStream` fail
  with `http.ErrNotSupported`, and one with no `Flush` delays every event.
- **A compressed response that reflects a secret leaks it.** BREACH guesses a
  secret, such as a token on the page, from the compressed size of responses
  that also reflect the attacker's input. `http.CrossOriginProtection` needs
  no token, so the commonest secret isn't on the page, but any other secret
  in a compressed page beside reflected input still is.
- **A cache serves the wrong encoding** unless the response adds
  `Vary: Accept-Encoding`, and a `Range` request of a file compressed on the
  fly gets bytes of the compressed body. Compressing assets ahead of time,
  and serving the compressed file to a client that accepts it, avoids both,
  and the CPU.

## Your library decides

Everything a client library needs from the server is the end user's code,
not humus's: how to tell its requests from a browser's, the headers it reads,
the status codes it swaps, and the events it listens for. inlineedit's two
UIs show the same contact list each way.

### htmx

The same URL serves a whole page to the browser and a fragment to htmx,
which sends `HX-Request: true`. The response varies on that header, so a
cache keeps the two apart:

```go
func isHTMX(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "HX-Request")
	return r.Header.Get("HX-Request") == "true"
}
```

A saved row answers with its fragment, and an `HX-Trigger` header for any
listener on the page, as htmxui's `save` in [Forms](#forms) does. A form with
problems answers again with a 422.

htmx swaps no 4xx by default, so the page's `responseHandling` swaps a 422,
and the rows swap themselves in place:

```html
<meta name="htmx-config" content='{"responseHandling":[{"code":"204","swap":false},{"code":"[23]..","swap":true},{"code":"422","swap":true},{"code":"[45]..","swap":false,"error":true}]}'>
```

```html
{{define "row"}}<div id="contact-{{.ID}}"{{if .OOB}} hx-swap-oob="true"{{end}}><span class="text">{{.Name}} {{.Email}}</span> <button hx-get="{{.EditURL}}" hx-target="closest div" hx-swap="outerHTML">Edit</button></div>{{end}}
```

The feed's row is the same template with `hx-swap-oob`, so htmx's SSE
extension swaps it in wherever that row is on the page.

### Datastar

Datastar sends `Datastar-Request: true` with its actions, and processes only
a 200. An action's answer is an event stream of patches, even for one patch,
so a form with problems is a 200 too:

```go
func isDatastar(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "Datastar-Request")
	return r.Header.Get("Datastar-Request") == "true"
}
```

```go
func patchEvent(name string, data any) (sse.Event, error) {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return sse.Event{}, err
	}
	return sse.Event{Name: "datastar-patch-elements", Data: elements(b.String())}, nil
}
```

```go
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
```

A form with problems is a 200 patch of the form, in datastarui's `save`:

```go
if !form.Valid() {
	return ui.patch(w, "edit", editRow(c, form.Raw(contacts.Name), form.Raw(contacts.Email), contacts.ProblemsIn(form)))
}
```

The patch renders before the stream starts, so a template's error still gets
the error page. Each line of the patch is a data line beginning `elements `:

```go
func elements(html string) string {
	lines := strings.Split(newlines.Replace(html), "\n")
	for i, line := range lines {
		lines[i] = "elements " + line
	}
	return strings.Join(lines, "\n")
}
```

Datastar's Go SDK, datastar-go, writes the same events with its own stream
writer, and works on a Site as on net/http. That writer never sets the write
deadline, so a long-lived stream of it needs the line in
[Event streams](/humus/pr-preview/pr-1629/services/http/serving/#event-streams) that
clears the deadline once.

### Unpoly's method override

Unpoly sends a PUT, a PATCH or a DELETE as a POST, with the method in a
`_method` form field. A middleware in front of the Site turns it back, so
the Site routes it to its PUT route, and names its span after it:

```go
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
```

```go
handler := http.NewCrossOriginProtection().Handler(contacts.MethodOverride(site))
```

It goes inside `http.CrossOriginProtection`, which checks the POST the
browser sent. It turns a POST into no method but these three, so a form
can't make itself a GET. The form it parsed is the handler's, in
`r.PostForm`.

### The libraries' docs

Each library documents what it sends and what it expects back:

- [htmx](https://htmx.org/docs/), and its
  [SSE extension](https://htmx.org/extensions/sse/)
- [Datastar](https://data-star.dev/guide/getting_started)
- [Turbo](https://turbo.hotwired.dev/handbook/introduction)
- [Unpoly](https://unpoly.com/up.protocol)
- [Alpine AJAX](https://alpine-ajax.js.org/)
- [fixi](https://github.com/bigskysoftware/fixi)

The example vendors its scripts, and the versions are pinned in
[`vendored_test.go`](https://github.com/z5labs/humus/blob/main/example/http/inlineedit/vendored_test.go),
which checks each script against its release, and the Chrome image its
browser test runs in
[`chrometest.go`](https://github.com/z5labs/humus/blob/main/example/http/inlineedit/internal/chrometest/chrometest.go).

Next: [gRPC](/humus/pr-preview/pr-1629/services/grpc/)
