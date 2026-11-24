# The Site

## The paths

Every page, action and script is a typed path, and the same value routes a
request and builds its links. A poll's id must look like a UUID, so any
other path is a 404:

```go
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
```

## The routes

One route for each method and path:

```go
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
```

## The UI

The handlers are methods of a `UI`, on the store and the listener, through
interfaces of its own:

```go
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
```

The scripts and the templates are embedded in the binary:

```go
//go:embed static
var static embed.FS

//go:embed templates
var templates embed.FS
```

```go
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
```

`build` hashes each script, and parses each page with the layout:

```go
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
```

## The scripts

`ui/static` holds htmx and its SSE extension, vendored.
[`vendored_test.go`](https://github.com/z5labs/humus/blob/main/example/http/polls/vendored_test.go)
names the version each one is, and checks that the file is that version's.

A script's URL holds its hash, `/scripts/{hash}/{script...}`, so a new
version is a new URL:

```go
func (ui *UI) scriptURL(name string) (string, error) {
	hash, ok := ui.hashes[name]
	if !ok {
		return "", fmt.Errorf("%q isn't a script", name)
	}
	return scriptPath.URL(hypermedia.Arg(scriptHash, hash), hypermedia.Arg(scriptName, name)).String(), nil
}
```

So the handler serves a script only at its current hash, and lets a browser
cache it for a year:

```go
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
```

## The layout

Every page is `layout.html` around its own `main`. It loads both scripts
from their hashed URLs:

```html
{{define "layout"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Title}}</title>
<meta name="htmx-config" content='{"allowEval":false,"includeIndicatorStyles":false,"responseHandling":[{"code":"204","swap":false},{"code":"[23]..","swap":true},{"code":"409","swap":true},{"code":"422","swap":true},{"code":"[45]..","swap":false,"error":true}]}'>
<script src="{{script "htmx.min.js"}}"></script>
<script src="{{script "sse.min.js"}}"></script>
</head>
<body>
{{template "main" .}}
</body>
</html>
{{end}}
```

The `htmx-config`:

- **`allowEval` and `includeIndicatorStyles` are off,** because the CSP
  below allows neither `eval` nor an inline style;
- **`responseHandling` swaps a 409 and a 422,** the answers to a second vote
  and to a form with problems, and swaps no other 4xx or 5xx;
- **a 204 swaps nothing,** so a created poll's `HX-Redirect` is all it does.

## Rendering

A page renders into a buffer first, so a template's error comes before the
first write and gets the error page:

```go
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
```

`renderTemplate` renders one fragment of a page, such as its form, for
htmx to swap.

## The error page

The error page answers a `RequestError` with its status, an unavailable
store with a 503, and any other error with a 500. Each status has a
message of its own:

```go
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
```

```go
type errorView struct {
	Title   string
	Message string
}
```

```html
{{define "main"}}<h1>{{.Title}}</h1>
<p>{{.Message}}</p>{{end}}{{template "layout" .}}
```

## The Site and its CSP

`Site` builds the `hypermedia.Site` from the error page and the routes. A
middleware above it sets the `Content-Security-Policy`, so the error page
gets it too:

```go
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
```

The policy allows the site's own scripts and nothing inline, and a form to
post only to the site:

```go
const (
	csp          = "default-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	maxBodyBytes = 16 << 10
)
```

A body over `maxBodyBytes` gets the 413 page.
[Voting](/humus/pr-preview/pr-1613/walkthroughs/http/polls/voting/#cross-origin-requests)
covers `http.CrossOriginProtection`.

Next: [Creating a poll](/humus/pr-preview/pr-1613/walkthroughs/http/polls/creating/)
