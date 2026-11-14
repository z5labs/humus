# Migrating from Fiber

This guide moves a small [Fiber](https://docs.gofiber.io) v3 service to
humus. Its first step runs the Fiber app on humus, so the process, settings,
TLS, telemetry, probes and shutdown are humus's from then on. Its next steps
move the routes onto `openapi/v3` a few at a time, while Fiber serves the
rest, until the service requires neither Fiber nor fasthttp.

Fiber runs on fasthttp, not `net/http`, so each handler is rewritten when its
route moves. Two things still let the service move a few routes at a time.
Fiber's [adaptor](https://docs.gofiber.io/middleware/adaptor) middleware
serves a Fiber app as an `http.HandlerFunc`, which `httpserver.NewService`
serves. And an `openapi.API` hands every request no operation serves to its
`Unmatched` handlers, which can be the adapted app.

## Fiber and humus

| Fiber | humus |
|---|---|
| `app.Listen` | `httpserver.Main`, and an `Init` that returns `httpserver.NewService`: [Serving HTTP](/humus/pr-preview/pr-1436/services/http/serving/#a-service) |
| fasthttp's settings in `fiber.Config`, such as `ReadTimeout` | `httpserver.Settings`, such as `ReadTimeout`: [Settings](/humus/pr-preview/pr-1436/services/http/serving/#settings) |
| the address, from `os.Getenv` | `httpserver.Settings`, sourced and checked with `config.Struct`: [Configuring a humus service](/humus/pr-preview/pr-1436/fundamentals/configuration/) |
| TLS on `ListenConfig`, or TLS left to a proxy | `Settings.TLS`, which is required: [TLS](/humus/pr-preview/pr-1436/services/http/serving/#tls) |
| `GracefulContext` on SIGTERM, within `ShutdownTimeout` | a 5s drain, then a 10s grace period: [The defaults](/humus/pr-preview/pr-1436/services/http/serving/#the-defaults) |
| `log`, in `main` | for the service's own records, the Setup's `LoggerProvider`, through the OpenTelemetry Logs API: [Telemetry](/humus/pr-preview/pr-1436/fundamentals/telemetry/#where-the-providers-come-from) |
| `logger.New()` | while Fiber serves routes, `logger.New` on the Process's stdout; then none: each request is a span, with its metrics: [Telemetry](/humus/pr-preview/pr-1436/services/http/serving/#telemetry) |
| `recover.New()` | while Fiber serves routes, `recover.New`; then the API's `CatchAll` answers a panic: [Panics](/humus/pr-preview/pr-1436/services/http/openapi/#panics) |
| `BodyLimit` | while Fiber serves routes, `BodyLimit`, which the adaptor enforces; then none: `http.MaxBytesHandler` around the API, and the decoder answers 413: [Operations](/humus/pr-preview/pr-1436/services/http/openapi/#operations) |
| `app.Group` | a `Path` that the paths under it share: [Paths](/humus/pr-preview/pr-1436/services/http/openapi/#paths) |
| `Get`, `Post`, `Put` and `Delete` on a group | `openapi.Get`, `Post`, `Put` and `Delete`, each with a typed handler: [Operations](/humus/pr-preview/pr-1436/services/http/openapi/#operations) |
| `c.Bind().URI` and `c.Bind().Query`, with `uri` and `query` tags and a `default` | `openapi.PathParam` and `openapi.QueryParam`, with their constraints and default, and `openapi.ParamFrom`: [Parameters](/humus/pr-preview/pr-1436/services/http/openapi/#parameters) |
| `c.Bind().JSON` | the operation's decoder, `jsoncodec.Decoder`: [Codecs](/humus/pr-preview/pr-1436/services/http/openapi/#codecs) |
| `StructValidator` | none: the handler validates its body, with the same validator, and returns an error an `openapi.Error` answers: [Error](/humus/pr-preview/pr-1436/services/http/openapi/#error) |
| `c.JSON` and `c.SendStatus` | the handler's result, which its encoder writes, `jsoncodec.Encoder` or `openapi.EmptyEncoder`: [Operations](/humus/pr-preview/pr-1436/services/http/openapi/#operations) |
| `ErrorHandler` and `fiber.NewError` | `openapi.Error`, `RequestErrors`, `CatchAll` and `Unmatched`, with `problem`: [Errors](/humus/pr-preview/pr-1436/services/http/openapi/#errors) |
| routes matched in any case, and with a trailing slash | none: the API serves a path only as written, so clients send it in its case, without a trailing slash: [Paths](/humus/pr-preview/pr-1436/services/http/openapi/#paths) |
| a `HEAD` route for every `GET` route | none: the API answers `HEAD` with a 405, so clients send `GET`: [Unmatched](/humus/pr-preview/pr-1436/services/http/openapi/#unmatched) |
| no probes | `httphealth.NewHandler` and a health Service: [Health probes](/humus/pr-preview/pr-1436/services/http/serving/#health-probes) |
| `app.Listener` on a loopback port, in tests | `httpservertest.Start`, inside `testing/synctest`: [Testing](/humus/pr-preview/pr-1436/services/http/serving/#testing) |

## The Fiber service

The guide's code is under
[`example/http/fiber`](https://github.com/z5labs/humus/tree/main/example/http/fiber),
a module for each step:

```text
example/http/fiber/
├── cases/    the black-box test cases every step passes
├── start/    the Fiber service the guide starts from
├── step1/    its Fiber app, run on humus
├── step2/    /v1/users/{id} on openapi/v3, the rest on Fiber
└── step3/    every route on openapi/v3, without Fiber
```

The service is an API of users, kept in memory, built as Fiber's own
documentation builds one: routes in groups, middleware, a struct validator
and an error handler of its own:

```go
func newServer(store *store) *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit:       maxBodyBytes,
		ErrorHandler:    handleError,
		StructValidator: &structValidator{validate: validator.New()},
	})

	app.Use(logger.New())
	app.Use(recover.New())

	v1 := app.Group("/v1")
	users := v1.Group("/users")

	h := &handlers{store: store}
	users.Get("", h.listUsers)
	users.Post("", h.createUser)
	users.Get("/:id", h.getUser)
	users.Put("/:id", h.updateUser)
	users.Delete("/:id", h.deleteUser)
	return app
}
```

```go
func (v *structValidator) Validate(out any) error {
	return v.validate.Struct(out)
}
```

Its handlers bind parameters and JSON bodies, which the validator checks
against their `validate` tags:

```go
type userInput struct {
	Name  string `json:"name" validate:"required"`
	Email string `json:"email" validate:"required,email"`
}

type userURI struct {
	ID int `uri:"id"`
}

type page struct {
	Limit  int `query:"limit,default:20" validate:"min=1,max=100"`
	Offset int `query:"offset" validate:"min=0"`
}
```

```go
func (h *handlers) listUsers(c fiber.Ctx) error {
	var p page
	if err := c.Bind().Query(&p); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(h.store.list(p.Offset, p.Limit))
}
```

```go
func (h *handlers) updateUser(c fiber.Ctx) error {
	var uri userURI
	if err := c.Bind().URI(&uri); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	var in userInput
	if err := c.Bind().JSON(&in); err != nil {
		return bodyError(err)
	}
	u, ok := h.store.update(uri.ID, in)
	if !ok {
		return userNotFound(uri.ID)
	}
	return c.JSON(u)
}
```

A body that breaks its tags is a 422:

```go
func bodyError(err error) error {
	var invalid validator.ValidationErrors
	if errors.As(err, &invalid) {
		return fiber.NewError(fiber.StatusUnprocessableEntity, err.Error())
	}
	return fiber.NewError(fiber.StatusBadRequest, err.Error())
}
```

Its error handler answers every error with an RFC 9457 problem detail: a
request no route serves, a body past `BodyLimit`, a recovered panic, and the
errors its handlers return:

```go
type problemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func handleError(c fiber.Ctx, err error) error {
	p := problemDetails{Type: "about:blank", Status: fiber.StatusInternalServerError}
	var e *fiber.Error
	if errors.As(err, &e) {
		p.Status, p.Detail = e.Code, e.Message
	}
	p.Title = http.StatusText(p.Status)
	return c.Status(p.Status).JSON(p, "application/problem+json")
}
```

`main` serves plain HTTP until SIGINT or SIGTERM, then shuts down
gracefully, as Fiber's documentation shows:

```go
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":3000"
	}

	app := newServer(newStore())
	if err := app.Listen(addr, fiber.ListenConfig{GracefulContext: ctx}); err != nil {
		log.Fatal(err)
	}
}
```

## The test cases

The `cases` module holds black-box cases, written once. Each takes the
service's URL and a client:

```go
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
	{Name: "BadParameters", Run: BadParameters},
	{Name: "BadBodies", Run: BadBodies},
	{Name: "Unmatched", Run: Unmatched},
}
```

Every step runs them all, each against a service of its own. The Fiber
service runs them on Fiber's own server, on a loopback port:

```go
func TestServer_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			u := serve(t, newServer(newStore()))

			c.Run(t, cases.Target{URL: u, Client: &http.Client{}})
		})
	}
}

func serve(t *testing.T, app *fiber.App) *url.URL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() {
		require.NoError(t, app.Shutdown())
		require.NoError(t, <-served)
	})
	return &url.URL{Scheme: "http", Host: ln.Addr().String()}
}
```

## Step 1: run it on humus

`main` becomes one call to `httpserver.Main`:

```go
httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
```

`Init` serves the Fiber app, its routes and handlers unchanged, with
`httpserver.NewService`, next to the probes:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	stopped := make(chan struct{})
	s.OnStop(func(context.Context) error {
		close(stopped)
		return nil
	})
	app := newServer(newStore(), s.Process.Stdout, stopped)

	hs := health.NewService()
	mux := http.NewServeMux()
	mux.Handle("/", route(app))
	mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))

	svc, err := httpserver.NewService(s, settings, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, hs}, nil
}
```

Served over `net/http`, the app no longer runs on fasthttp's server: the
adaptor copies each request into a fasthttp one and the response back, so the
server settings in `fiber.Config`, such as `ReadTimeout`, no longer apply.

Fiber's logger takes its writer when it's made, and keeps a goroutine that
refreshes its timestamps until `TimeDone` is closed. So `newServer` takes the
Process's stdout, and a channel `Init` closes once the service has stopped:

```go
func newServer(store *store, stdout io.Writer, stopped <-chan struct{}) *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit:       maxBodyBytes,
		ErrorHandler:    handleError,
		StructValidator: &structValidator{validate: validator.New()},
	})

	app.Use(logger.New(logger.Config{Stream: stdout, TimeDone: stopped}))
```

From here on, these are humus's:

- **The process and its shutdown.** Fiber's request log goes to the
  Process's stdout, as it went to the real one. On SIGTERM the service keeps
  serving through a 5s drain, then waits up to 10s for requests in flight, as
  [The defaults](/humus/pr-preview/pr-1436/services/http/serving/#the-defaults)
  describes, so `GracefulContext` goes.
- **The settings.** A missing or bad one makes the service exit 78, naming
  its variable. `ADDR` has no default now.
- **TLS.** `serverTLS` reads the certificate and its key from the files
  `TLS_CERT_FILE` and `TLS_KEY_FILE` name, through the Host's `FS`. A proxy
  in front connects over TLS, as
  [Behind a proxy](/humus/pr-preview/pr-1436/services/http/serving/#behind-a-proxy)
  describes.
- **Telemetry.** `buildTelemetry` sources the OpenTelemetry variables and
  names the service `users`, as
  [Writing the Builder](/humus/pr-preview/pr-1436/fundamentals/telemetry/#writing-the-builder)
  shows. Each request is a span, with its metrics.
- **The probes.** The kubelet probes `/health/liveness` and
  `/health/readiness` on the service's port, with `scheme: HTTPS`, as
  [Health probes](/humus/pr-preview/pr-1436/services/http/serving/#health-probes)
  describes.

`readSettings` sources the address and the certificate, the two settings
`httpserver` requires:

```go
func readSettings(ctx context.Context, s *humus.Setup) (httpserver.Settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	return config.Struct(func(r *config.Resolver) httpserver.Settings {
		settings := httpserver.DefaultSettings()
		settings.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		settings.TLS = r.Get(env("TLS_CERT_FILE").FlatMap(func(certFile string) config.Value[*tls.Config] {
			return env("TLS_KEY_FILE").Parse(serverTLS(s.Host.FS, certFile))
		}).Validate(httpserver.CheckTLS))
		return settings
	}).Read(ctx)
}
```

`httpserver` names each span after the request's `Pattern`, which a Fiber
handler can't reach. `route` serves the app through the adaptor, and sets
the `Pattern` to the Fiber route that matches, such as `/v1/users/:id`:

```go
func route(app *fiber.App) http.Handler {
	serve := adaptor.FiberApp(app)
	cfg := app.Config()
	routes := app.GetRoutes(true)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, rt := range routes {
			if rt.Method == r.Method && fiber.RoutePatternMatch(r.URL.Path, rt.Path, cfg) {
				r.Pattern = rt.Path
				break
			}
		}
		serve(w, r)
	})
}
```

The cases run against the whole service, with `httpservertest.Start`, inside
`synctest`. `start` gives it a certificate from a CA of the test's own. The
adaptor looks up a client address that isn't an IP, as a connection on the
test's Host has, so `withClientAddr` gives each connection one:

```go
func start(t *testing.T, tel telemetry.Builder) service {
	t.Helper()
	ca := humustest.NewCA(t)
	certPEM, keyPEM := pemPair(t, ca.Issue(t, "localhost"))
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"tls/tls.crt": certPEM,
		"tls/tls.key": keyPEM,
	}))
	host := h.Host()
	host.Listen = withClientAddr(host.Listen)
	p, status := httpservertest.Start(t, Init,
		humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(tel)),
		humustest.WithEnv(map[string]string{
			"ADDR":          ":8443",
			"TLS_CERT_FILE": "tls/tls.crt",
			"TLS_KEY_FILE":  "tls/tls.key",
		}),
	)
	select {
	case <-h.Listening(addr):
	case st := <-status:
		t.Fatalf("the service exited %d: %v", st.Code(), st.Cause())
	}
	return service{
		target: cases.Target{URL: &url.URL{Scheme: "https", Host: addr}, Client: httpservertest.NewClient(h, ca.Pool())},
		p:      p,
		status: status,
	}
}
```

```go
func TestInit_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := start(t, telemetry.NoopBuilder)
				c.Run(t, s.target)
				s.stop(t)
			})
		})
	}
}
```

What clients see change:

- **HTTPS.** The service serves TLS alone, so clients, and any proxy in
  front, use `https`. A client that offers HTTP/2 gets it.
- **The probes.** `/health/liveness` and `/health/readiness` answer, where
  Fiber answered 404.
- **A path that isn't clean,** such as `/v1//users`, redirects to the clean
  one, as `http.ServeMux` does, where Fiber answered 404.
- **A body past `BodyLimit`** is the adaptor's 413, in plain text. Fiber
  answered it with `handleError`, then closed the connection without reading
  the body, so a client still sending it could see a reset instead.
- **A 204** has the `Content-Type` `text/plain; charset=utf-8`, which the
  adaptor copies from fasthttp.

## Step 2: move /v1/users/{id}

The next steps move the routes onto `openapi/v3` a path at a time. The Fiber
group becomes a path that the paths under it share, and `:id` a typed
parameter:

```go
var userID = openapi.PathParam[int]("id").Description("The user's ID.")

var (
	v1        = openapi.Root().Lit("v1")
	usersPath = v1.Lit("users")
	userPath  = usersPath.Param(userID)
)
```

The errors `handleError` answered become problem details of the service's
own, each given to the operations that return it:

```go
type notFound struct{ problem.Details }

type unprocessable struct{ problem.Details }

var (
	userNotFoundError = openapi.Error[notFound](http.StatusNotFound, jsoncodec.Problem)
	invalidUserError  = openapi.Error[unprocessable](http.StatusUnprocessableEntity, jsoncodec.Problem)
)
```

The three routes on `/v1/users/:id` become operations:

```go
func (u *users) operations() []openapi.Operation {
	return []openapi.Operation{
		openapi.Get(userPath, u.getUser, jsoncodec.Encoder,
			openapi.OperationID("getUser"),
			userNotFoundError,
		),
		openapi.Put(userPath, u.updateUser, jsoncodec.Decoder, jsoncodec.Encoder,
			openapi.OperationID("updateUser"),
			userNotFoundError,
			invalidUserError,
		),
		openapi.Delete(userPath, u.deleteUser, openapi.EmptyEncoder,
			openapi.OperationID("deleteUser"),
			userNotFoundError,
		),
	}
}
```

Each handler is rewritten, since a Fiber handler takes a `fiber.Ctx`. It
gets its parameter and its decoded body, validates the body with the same
validator, and returns its result or an error:

```go
func (u *users) updateUser(_ context.Context, req openapi.Request[userInput]) (user, error) {
	id := openapi.ParamFrom(req, userID)
	in := req.Body()
	if err := u.validate.Struct(in); err != nil {
		return user{}, invalidUser(err)
	}
	updated, ok := u.store.update(id, in)
	if !ok {
		return user{}, userNotFound(id)
	}
	return updated, nil
}
```

```go
func userNotFound(id int) error {
	return notFound{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusNotFound),
		Status: http.StatusNotFound,
		Detail: fmt.Sprintf("There is no user %d.", id),
	}}
}
```

`RequestErrors` answers a parameter or a body the API rejects, and
`CatchAll` anything else, such as a panic, which the API logs. Like
`handleError`'s, its answer says nothing of the failure:

```go
func serverError(error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
	}
}
```

`Init` serves the API, which hands the adapted app every request no
operation serves. Both share the store, and `http.MaxBytesHandler` limits the
API's bodies as `BodyLimit` limits Fiber's:

```go
store := newStore()
app := newServer(store, s.Process.Stdout, stopped)

api, err := openapi.NewAPI(s,
	openapi.Info{Title: "Users", Version: "1.0.0"},
	openapi.Unmatched{
		NotFound:         route(app),
		MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
	},
	openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
	openapi.CatchAll(http.StatusInternalServerError, serverError, jsoncodec.Problem),
	(&users{store: store, validate: validator.New()}).operations(),
)
if err != nil {
	return nil, err
}

hs := health.NewService()
mux := http.NewServeMux()
mux.Handle("/", http.MaxBytesHandler(api, maxBodyBytes))
mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))
```

A method `/v1/users/{id}` doesn't serve is the API's 405 now, since Fiber
serves nothing at that path. Fiber keeps the two routes on `/v1/users`:

```go
h := &handlers{store: store}
users.Get("", h.listUsers)
users.Post("", h.createUser)
return app
```

What clients see change on a route that moves:

- **The detail of a 400** is `openapi`'s own, such as
  `path parameter "id": "ada" isn't an integer of 64 bits`, and a 405 has
  none.
- **A body past the limit** is a 413 with a problem detail again.
- **A body that isn't JSON,** by its `Content-Type` or for want of one, is a
  415, where Fiber read any body as JSON.
- **A path in another case, or with a trailing slash,** such as
  `/V1/Users/1` or `/v1/users/1/`, is a 404, where Fiber served it as
  `/v1/users/1`.
- **A `HEAD` request** is a 405, and `Allow` doesn't list `HEAD`, where
  Fiber answered it as a `GET` without the body.
- **A JSON answer's `Content-Type`** is `application/json`, without Fiber's
  `; charset=utf-8`.

On the routes Fiber still serves, a body sent without a `Content-Length`
that runs past the limit is a 500: the adaptor reads it through
`http.MaxBytesHandler`'s reader, and answers any body it fails to read with
a 500.

## Step 3: move /v1/users, and drop Fiber

The two routes left become operations, and their query parameters take
their range and default with them:

```go
var (
	pageLimit  = openapi.QueryParam[int]("limit").Description("The most users to list.").Minimum(1).Maximum(100).Default(20)
	pageOffset = openapi.QueryParam[int]("offset").Description("How many users to skip.").Minimum(0)
)
```

```go
openapi.Get(usersPath, u.listUsers, jsoncodec.Encoder,
	openapi.OperationID("listUsers"),
	openapi.Query(pageLimit),
	openapi.Query(pageOffset),
),
openapi.Post(usersPath, u.createUser, jsoncodec.Decoder, jsoncodec.Encoder,
	openapi.OperationID("createUser"),
	openapi.Status(http.StatusCreated),
	invalidUserError,
),
```

```go
func (u *users) listUsers(_ context.Context, req openapi.Request[openapi.EmptyBody]) ([]user, error) {
	return u.store.list(openapi.ParamFrom(req, pageOffset), openapi.ParamFrom(req, pageLimit)), nil
}
```

With no route left on Fiber, the API answers unmatched requests itself, and
serves its OpenAPI document:

```go
api, err := openapi.NewAPI(s,
	openapi.Info{Title: "Users", Version: "1.0.0"},
	openapi.Unmatched{
		NotFound:         problem.Handler(http.StatusNotFound, jsoncodec.Problem),
		MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
	},
	openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
	openapi.CatchAll(http.StatusInternalServerError, serverError, jsoncodec.Problem),
	append((&users{store: newStore(), validate: validator.New()}).operations(),
		openapi.Document(openapi.Root().Lit("openapi.json")),
	),
)
```

Fiber's code goes, `newServer`, its validator, `handleError` and `route`,
and the module's
[`go.mod`](https://github.com/z5labs/humus/blob/main/example/http/fiber/step3/go.mod)
requires neither Fiber nor fasthttp. The `validate` tags stay for the
validator, and tags beside them describe the bodies in the document:

```go
type userInput struct {
	Name  string `json:"name" validate:"required" required:"true" minLength:"1"`
	Email string `json:"email" validate:"required,email" required:"true" format:"email"`
}
```

The test of the document compares it with a golden file, so a change to it
shows up in review:

```go
var indented bytes.Buffer
require.NoError(t, json.Indent(&indented, doc, "", "  "))
indented.WriteByte('\n')
want, err := os.ReadFile(filepath.Join("testdata", "openapi.json"))
require.NoError(t, err)
require.Equal(t, string(want), indented.String())
```

What clients see change, besides step 2's changes on `/v1/users`:

- **A query parameter given twice,** or `offset` given empty, is a 400,
  where Fiber read the last value, or 0.
- **The document** is served at `/openapi.json`.
