# Migrating from Echo

This guide moves a small [Echo](https://echo.labstack.com) v5 service to
humus. Its first step runs the Echo engine on humus, so the process,
settings, TLS, telemetry, probes and shutdown are humus's from then on. Its
next steps move the routes onto `openapi/v3` a few at a time, while Echo
serves the rest, until the service no longer requires Echo.

Two things make that work. An Echo engine is an `http.Handler`, which
`httpserver.NewService` serves. And an `openapi.API` hands every request no
operation serves to its `Unmatched` handlers, which can be the Echo engine.

## Echo and humus

| Echo | humus |
|---|---|
| `echo.StartConfig` and its `Start` | `httpserver.Main`, and an `Init` that returns `httpserver.NewService`: [Serving HTTP](/humus/pr-preview/pr-1443/services/http/serving/#a-service) |
| the address, from `os.Getenv` | `httpserver.Settings`, sourced and checked with `config.Struct`: [Configuring a humus service](/humus/pr-preview/pr-1443/fundamentals/configuration/) |
| `StartTLS`, or TLS left to a proxy | `Settings.TLS`, which is required: [TLS](/humus/pr-preview/pr-1443/services/http/serving/#tls) |
| graceful shutdown on SIGTERM, within `GracefulTimeout` | a 5s drain, then a 10s grace period: [The defaults](/humus/pr-preview/pr-1443/services/http/serving/#the-defaults) |
| `e.Logger` | while Echo serves routes, Echo's logger on the Process's stdout, rather than the real one; for the service's own records, the Setup's `LoggerProvider`, through the OpenTelemetry Logs API: [Telemetry](/humus/pr-preview/pr-1443/fundamentals/telemetry/#where-the-providers-come-from) |
| `middleware.RequestLogger()` | none: each request is a span, with its metrics: [Telemetry](/humus/pr-preview/pr-1443/services/http/serving/#telemetry) |
| `middleware.Recover()` | the API's `CatchAll` answers a panic: [Panics](/humus/pr-preview/pr-1443/services/http/openapi/#panics) |
| `middleware.BodyLimit` | none: `http.MaxBytesHandler` around the API, and the decoder answers 413: [Operations](/humus/pr-preview/pr-1443/services/http/openapi/#operations) |
| `e.Group` | a `Path` that the paths under it share: [Paths](/humus/pr-preview/pr-1443/services/http/openapi/#paths) |
| `GET`, `POST`, `PUT` and `DELETE` on a group | `openapi.Get`, `Post`, `Put` and `Delete`, each with a typed handler: [Operations](/humus/pr-preview/pr-1443/services/http/openapi/#operations) |
| `echo.PathParam`, `echo.QueryParamOr` and range checks by hand | `openapi.PathParam` and `openapi.QueryParam`, with their constraints, and `openapi.ParamFrom`: [Parameters](/humus/pr-preview/pr-1443/services/http/openapi/#parameters) |
| `c.Bind` | the operation's decoder, `jsoncodec.Decoder`: [Codecs](/humus/pr-preview/pr-1443/services/http/openapi/#codecs) |
| `e.Validator` and `c.Validate` | none: the handler validates its body, with the same validator, and returns an error an `openapi.Error` answers: [Error](/humus/pr-preview/pr-1443/services/http/openapi/#error) |
| `c.JSON` and `c.NoContent` | the handler's result, which its encoder writes, `jsoncodec.Encoder` or `openapi.EmptyEncoder`: [Operations](/humus/pr-preview/pr-1443/services/http/openapi/#operations) |
| `e.HTTPErrorHandler` and `echo.NewHTTPError` | `openapi.Error`, `RequestErrors`, `CatchAll` and `Unmatched`, with `problem`: [Errors](/humus/pr-preview/pr-1443/services/http/openapi/#errors) |
| no probes | `httphealth.NewHandler` and a health Service: [Health probes](/humus/pr-preview/pr-1443/services/http/serving/#health-probes) |
| `httptest.NewServer` | `httpservertest.Start`, inside `testing/synctest`: [Testing](/humus/pr-preview/pr-1443/services/http/serving/#testing) |

## The Echo service

The guide's code is under
[`example/http/echo`](https://github.com/z5labs/humus/tree/main/example/http/echo),
a module for each step:

```text
example/http/echo/
├── cases/    the black-box test cases every step passes
├── start/    the Echo service the guide starts from
├── step1/    its Echo engine, run on humus
├── step2/    /v1/users/{id} on openapi/v3, the rest on Echo
└── step3/    every route on openapi/v3, without Echo
```

The service is an API of users, kept in memory, built as Echo's own
documentation builds one: routes in groups, middleware, and a validator and
an error handler of its own:

```go
func newServer(store *store) *echo.Echo {
	e := echo.New()
	e.Validator = &customValidator{validator: validator.New()}
	e.HTTPErrorHandler = handleError

	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit(maxBodyBytes))

	v1 := e.Group("/v1")
	users := v1.Group("/users")

	h := &handlers{store: store}
	users.GET("", h.listUsers)
	users.POST("", h.createUser)
	users.GET("/:id", h.getUser)
	users.PUT("/:id", h.updateUser)
	users.DELETE("/:id", h.deleteUser)
	return e
}
```

Its handlers read parameters, and bind and validate JSON bodies:

```go
type userInput struct {
	Name  string `json:"name" validate:"required"`
	Email string `json:"email" validate:"required,email"`
}
```

```go
func (h *handlers) listUsers(c *echo.Context) error {
	limit, err := echo.QueryParamOr(c, "limit", 20)
	if err != nil {
		return err
	}
	if limit < 1 || limit > 100 {
		return echo.NewHTTPError(http.StatusBadRequest, "limit must be from 1 to 100")
	}
	offset, err := echo.QueryParamOr(c, "offset", 0)
	if err != nil {
		return err
	}
	if offset < 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "offset must not be negative")
	}
	return c.JSON(http.StatusOK, h.store.list(offset, limit))
}
```

```go
func (h *handlers) updateUser(c *echo.Context) error {
	id, err := echo.PathParam[int](c, "id")
	if err != nil {
		return err
	}
	var in userInput
	if err := c.Bind(&in); err != nil {
		return err
	}
	if err := c.Validate(&in); err != nil {
		return err
	}
	u, ok := h.store.update(id, in)
	if !ok {
		return userNotFound(id)
	}
	return c.JSON(http.StatusOK, u)
}
```

```go
func (cv *customValidator) Validate(i any) error {
	if err := cv.validator.Struct(i); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	}
	return nil
}
```

Its error handler answers every error with an RFC 9457 problem detail, and
logs the ones no status explains:

```go
type problemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func handleError(c *echo.Context, err error) {
	if r, _ := echo.UnwrapResponse(c.Response()); r != nil && r.Committed {
		return
	}
	p := problemDetails{Type: "about:blank", Status: http.StatusInternalServerError}
	var he *echo.HTTPError
	switch {
	case errors.As(err, &he):
		p.Status, p.Detail = he.Code, he.Message
	case echo.StatusCode(err) != 0:
		p.Status = echo.StatusCode(err)
	default:
		c.Logger().ErrorContext(c.Request().Context(), "request failed", "error", err)
	}
	p.Title = http.StatusText(p.Status)
	c.Response().Header().Set(echo.HeaderContentType, "application/problem+json")
	if err := c.JSON(p.Status, p); err != nil {
		c.Logger().ErrorContext(c.Request().Context(), "writing the problem failed", "error", err)
	}
}
```

`main` serves plain HTTP until SIGINT or SIGTERM, then shuts down
gracefully:

```go
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	e := newServer(newStore())
	sc := echo.StartConfig{Address: addr, GracefulTimeout: 10 * time.Second}
	if err := sc.Start(ctx, e); err != nil {
		e.Logger.Error("failed to start server", "error", err)
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

Every step runs them all, each against a service of its own. The Echo
service runs them over `httptest`:

```go
func TestServer_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			e := newServer(newStore())
			e.Logger = slog.New(slog.DiscardHandler)
			srv := httptest.NewServer(e)
			t.Cleanup(srv.Close)
			u, err := url.Parse(srv.URL)
			require.NoError(t, err)

			c.Run(t, cases.Target{URL: u, Client: srv.Client()})
		})
	}
}
```

## Step 1: run it on humus

`main` becomes one call to `httpserver.Main`:

```go
httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
```

`Init` serves the Echo engine, its routes and handlers unchanged, with
`httpserver.NewService`, next to the probes:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	e := newServer(newStore())
	e.Logger = slog.New(slog.NewJSONHandler(s.Process.Stdout, nil))
	e.Use(route)

	hs := health.NewService()
	mux := http.NewServeMux()
	mux.Handle("/", e)
	mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))

	svc, err := httpserver.NewService(s, settings, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, hs}, nil
}
```

From here on, these are humus's:

- **The process and its shutdown.** Echo's logger writes to the Process's
  stdout, as it wrote to the real one. On SIGTERM the service keeps serving
  through a 5s drain, then waits up to 10s for requests in flight, as
  [The defaults](/humus/pr-preview/pr-1443/services/http/serving/#the-defaults)
  describes, so `GracefulTimeout` goes.
- **The settings.** A missing or bad one makes the service exit 78, naming
  its variable. `ADDR` has no default now.
- **TLS.** `serverTLS` reads the certificate and its key from the files
  `TLS_CERT_FILE` and `TLS_KEY_FILE` name, through the Host's `FS`. A proxy
  in front connects over TLS, as
  [Behind a proxy](/humus/pr-preview/pr-1443/services/http/serving/#behind-a-proxy)
  describes.
- **Telemetry.** `buildTelemetry` sources the OpenTelemetry variables and
  names the service `users`, as
  [Writing the Builder](/humus/pr-preview/pr-1443/fundamentals/telemetry/#writing-the-builder)
  shows. Each request is a span, with its metrics.
- **The probes.** The kubelet probes `/health/liveness` and
  `/health/readiness` on the service's port, with `scheme: HTTPS`, as
  [Health probes](/humus/pr-preview/pr-1443/services/http/serving/#health-probes)
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

`httpserver` names each span after the request's `Pattern`, which Echo
doesn't set. `route` sets it to Echo's route, such as `/v1/users/:id`:

```go
func route(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Request().Pattern = c.Path()
		return next(c)
	}
}
```

The cases run against the whole service, with `httpservertest.Start`, inside
`synctest`. `start` gives it a certificate from a CA of the test's own:

```go
func start(t *testing.T, tel telemetry.Builder) service {
	t.Helper()
	ca := humustest.NewCA(t)
	certPEM, keyPEM := pemPair(t, ca.Issue(t, "localhost"))
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"tls/tls.crt": certPEM,
		"tls/tls.key": keyPEM,
	}))
	p, status := httpservertest.Start(t, Init,
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel)),
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
  Echo answered 404.

## Step 2: move /v1/users/{id}

The next steps move the routes onto `openapi/v3` a path at a time. The
Echo group becomes a path that the paths under it share, and `:id` a typed
parameter:

```go
var userID = openapi.PathParam[int]("id").Description("The user's ID.")

var (
	v1        = openapi.Root().Lit("v1")
	usersPath = v1.Lit("users")
	userPath  = usersPath.Param(userID)
)
```

The error handler's answers become problem details of the service's own,
each given to the operations that return it:

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

A handler gets its parameter and its decoded body, validates the body with
the same validator, and returns its result or an error:

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
`CatchAll` anything else, which the API logs, as `handleError` did. Its
answer says nothing of the failure:

```go
func serverError(error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
	}
}
```

`Init` serves the API, which hands Echo every request no operation serves.
Both share the store, and `http.MaxBytesHandler` limits the API's bodies as
`BodyLimit` limits Echo's:

```go
store := newStore()
e := newServer(store)
e.Logger = slog.New(slog.NewJSONHandler(s.Process.Stdout, nil))
e.Use(route)

api, err := openapi.NewAPI(s,
	openapi.Info{Title: "Users", Version: "1.0.0"},
	openapi.Unmatched{
		NotFound:         e,
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

A method `/v1/users/{id}` doesn't serve is the API's 405 now, since Echo
serves nothing at that path. Echo keeps the two routes on `/v1/users`:

```go
h := &handlers{store: store}
users.GET("", h.listUsers)
users.POST("", h.createUser)
return e
```

What clients see change on a route that moves:

- **The detail of a 400, a 413 or a 415** is `openapi`'s own, such as
  `path parameter "id": "ada" isn't an integer of 64 bits`.
- **An empty body** is a 400, where Echo bound nothing and answered 422.
- **An `OPTIONS` request** is a 405, and `Allow` doesn't list `OPTIONS`.
- **A path with more after the ID,** such as `/v1/users/1/friends`, is a 404,
  where Echo answered 400.

## Step 3: move /v1/users, and drop Echo

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

With no route left on Echo, the API answers unmatched requests itself, and
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

Echo's code goes, `newServer`, its validator, its error handler and
`route`, and the module's
[`go.mod`](https://github.com/z5labs/humus/blob/main/example/http/echo/step3/go.mod)
no longer requires Echo. Tags on the bodies describe them in the document:

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

- **A query parameter given twice, or given empty,** is a 400, where Echo
  read the first value, or the default.
- **The document** is served at `/openapi.json`.
