# Migrating from Gin

This guide moves a small [Gin](https://gin-gonic.com/en/docs/) v1 service to
humus. Its first step runs the Gin engine on humus, so the process,
settings, TLS, telemetry, probes and shutdown are humus's from then on. Its
next steps move the routes onto `openapi/v3` a few at a time, while Gin
serves the rest, until the service no longer requires Gin.

Two things make that work. A Gin engine is an `http.Handler`, which
`httpserver.NewService` serves. And an `openapi.API` hands every request no
operation serves to its `Unmatched` handlers, which can be the Gin engine.

## Gin and humus

| Gin | humus |
|---|---|
| an `http.Server` and its `ListenAndServe` | `httpserver.Main`, and an `Init` that returns `httpserver.NewService`: [Serving HTTP](/humus/pr-preview/pr-1432/services/http/serving/#a-service) |
| the address, from `os.Getenv` | `httpserver.Settings`, sourced and checked with `config.Struct`: [Configuring a humus service](/humus/pr-preview/pr-1432/fundamentals/configuration/) |
| `ListenAndServeTLS`, or TLS left to a proxy | `Settings.TLS`, which is required: [TLS](/humus/pr-preview/pr-1432/services/http/serving/#tls) |
| `Shutdown` on SIGTERM, within a timeout | a 5s drain, then a 10s grace period: [The defaults](/humus/pr-preview/pr-1432/services/http/serving/#the-defaults) |
| `log`, in `main` | for the service's own records, the Setup's `LoggerProvider`, through the OpenTelemetry Logs API: [Telemetry](/humus/pr-preview/pr-1432/fundamentals/telemetry/#where-the-providers-come-from) |
| `gin.Logger()` | while Gin serves routes, `gin.LoggerWithWriter` on the Process's stdout; then none: each request is a span, with its metrics: [Telemetry](/humus/pr-preview/pr-1432/services/http/serving/#telemetry) |
| `gin.CustomRecovery` | while Gin serves routes, `gin.CustomRecoveryWithWriter` on the Process's stderr; then the API's `CatchAll` answers a panic: [Panics](/humus/pr-preview/pr-1432/services/http/openapi/#panics) |
| `GIN_MODE` | none: Gin reads it from the real environment, and in debug mode writes its debug lines to the real stdout, so the deployment sets `GIN_MODE=release` |
| a body limit on `r.Use`, with `http.MaxBytesReader` | none: `http.MaxBytesHandler` around the API, and the decoder answers 413: [Operations](/humus/pr-preview/pr-1432/services/http/openapi/#operations) |
| `r.Group` | a `Path` that the paths under it share: [Paths](/humus/pr-preview/pr-1432/services/http/openapi/#paths) |
| `GET`, `POST`, `PUT` and `DELETE` on a group | `openapi.Get`, `Post`, `Put` and `Delete`, each with a typed handler: [Operations](/humus/pr-preview/pr-1432/services/http/openapi/#operations) |
| `c.ShouldBindUri` and `c.ShouldBindQuery`, with `uri`, `form` and `binding` tags | `openapi.PathParam` and `openapi.QueryParam`, with their constraints and default, and `openapi.ParamFrom`: [Parameters](/humus/pr-preview/pr-1432/services/http/openapi/#parameters) |
| `c.ShouldBindJSON` | the operation's decoder, `jsoncodec.Decoder`: [Codecs](/humus/pr-preview/pr-1432/services/http/openapi/#codecs) |
| `binding` tags on a body | none: the handler validates its body, with the same tags, and returns an error an `openapi.Error` answers: [Error](/humus/pr-preview/pr-1432/services/http/openapi/#error) |
| `c.JSON` and `c.Status` | the handler's result, which its encoder writes, `jsoncodec.Encoder` or `openapi.EmptyEncoder`: [Operations](/humus/pr-preview/pr-1432/services/http/openapi/#operations) |
| `c.AbortWithStatusJSON` | an error the handler returns, which `openapi.Error`, `RequestErrors` or `CatchAll` answers, with `problem`: [Errors](/humus/pr-preview/pr-1432/services/http/openapi/#errors) |
| `r.NoRoute`, and `r.NoMethod` with `HandleMethodNotAllowed` | `openapi.Unmatched`: [Unmatched](/humus/pr-preview/pr-1432/services/http/openapi/#unmatched) |
| `RedirectTrailingSlash`, on by default | none: the API serves a path as written, so clients drop the trailing slash: [Paths](/humus/pr-preview/pr-1432/services/http/openapi/#paths) |
| no probes | `httphealth.NewHandler` and a health Service: [Health probes](/humus/pr-preview/pr-1432/services/http/serving/#health-probes) |
| `httptest.NewServer` | `httpservertest.Start`, inside `testing/synctest`: [Testing](/humus/pr-preview/pr-1432/services/http/serving/#testing) |

## The Gin service

The guide's code is under
[`example/http/gin`](https://github.com/z5labs/humus/tree/main/example/http/gin),
a module for each step:

```text
example/http/gin/
├── cases/    the black-box test cases every step passes
├── start/    the Gin service the guide starts from
├── step1/    its Gin engine, run on humus
├── step2/    /v1/users/{id} on openapi/v3, the rest on Gin
└── step3/    every route on openapi/v3, without Gin
```

The service is an API of users, kept in memory, built as Gin's own
documentation builds one: routes in groups, middleware, and handlers for
the requests no route serves:

```go
func newServer(store *store) *gin.Engine {
	r := gin.New()
	r.HandleMethodNotAllowed = true

	r.Use(gin.Logger())
	r.Use(gin.CustomRecovery(handlePanic))
	r.Use(limitBody)

	r.NoRoute(noRoute)
	r.NoMethod(noMethod)

	v1 := r.Group("/v1")
	users := v1.Group("/users")

	h := &handlers{store: store}
	users.GET("", h.listUsers)
	users.POST("", h.createUser)
	users.GET("/:id", h.getUser)
	users.PUT("/:id", h.updateUser)
	users.DELETE("/:id", h.deleteUser)
	return r
}
```

`limitBody` is middleware of its own:

```go
func limitBody(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	c.Next()
}
```

Its handlers bind parameters and JSON bodies, which Gin checks against their
`binding` tags:

```go
type userInput struct {
	Name  string `json:"name" binding:"required"`
	Email string `json:"email" binding:"required,email"`
}

type userURI struct {
	ID int `uri:"id"`
}

type page struct {
	Limit  int `form:"limit,default=20" binding:"min=1,max=100"`
	Offset int `form:"offset" binding:"min=0"`
}
```

```go
func (h *handlers) listUsers(c *gin.Context) {
	var p page
	if err := c.ShouldBindQuery(&p); err != nil {
		abort(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, h.store.list(p.Offset, p.Limit))
}
```

```go
func (h *handlers) updateUser(c *gin.Context) {
	var uri userURI
	if err := c.ShouldBindUri(&uri); err != nil {
		abort(c, http.StatusBadRequest, err.Error())
		return
	}
	var in userInput
	if err := c.ShouldBindJSON(&in); err != nil {
		abortBinding(c, err)
		return
	}
	u, ok := h.store.update(uri.ID, in)
	if !ok {
		abortUserNotFound(c, uri.ID)
		return
	}
	c.JSON(http.StatusOK, u)
}
```

A body that breaks its tags is a 422, and one past the limit a 413:

```go
func abortBinding(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	var invalid validator.ValidationErrors
	switch {
	case errors.As(err, &tooLarge):
		abort(c, http.StatusRequestEntityTooLarge, err.Error())
	case errors.As(err, &invalid):
		abort(c, http.StatusUnprocessableEntity, err.Error())
	default:
		abort(c, http.StatusBadRequest, err.Error())
	}
}
```

Every error aborts the request with an RFC 9457 problem detail, and so do a
panic and a request no route serves:

```go
type problemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func abort(c *gin.Context, status int, detail string) {
	c.Header("Content-Type", "application/problem+json")
	c.AbortWithStatusJSON(status, problemDetails{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	})
}
```

```go
func handlePanic(c *gin.Context, _ any) {
	abort(c, http.StatusInternalServerError, "")
}
```

`main` serves plain HTTP on an `http.Server` until SIGINT or SIGTERM, then
shuts it down gracefully, as Gin's documentation shows:

```go
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: newServer(newStore()).Handler(),
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	<-ctx.Done()
	stop()
	log.Println("shutting down gracefully, press Ctrl+C again to force")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown: ", err)
	}
	log.Println("Server exiting")
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

Every step runs them all, each against a service of its own. The Gin
service runs them over `httptest`:

```go
func TestServer_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(newServer(newStore()))
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

`Init` serves the Gin engine, its routes and handlers unchanged, with
`httpserver.NewService`, next to the probes:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	r := newServer(newStore(), s.Process.Stdout, s.Process.Stderr)

	hs := health.NewService()
	mux := http.NewServeMux()
	mux.Handle("/", r)
	mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))

	svc, err := httpserver.NewService(s, settings, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, hs}, nil
}
```

Gin's middleware takes its writer, and the routes take the middleware, when
they're added. So `newServer` takes the Process's stdout and stderr, and adds
`route` before the routes:

```go
func newServer(store *store, stdout, stderr io.Writer) *gin.Engine {
	r := gin.New()
	r.HandleMethodNotAllowed = true

	r.Use(gin.LoggerWithWriter(stdout))
	r.Use(gin.CustomRecoveryWithWriter(stderr, handlePanic))
	r.Use(limitBody)
	r.Use(route)
```

From here on, these are humus's:

- **The process and its shutdown.** Gin's request log goes to the Process's
  stdout, and a panic's stack to its stderr, as they went to the real ones.
  On SIGTERM the service keeps serving through a 5s drain, then waits up to
  10s for requests in flight, as
  [The defaults](/humus/pr-preview/pr-1432/services/http/serving/#the-defaults)
  describes, so the `http.Server` and its `Shutdown` go.
- **The settings.** A missing or bad one makes the service exit 78, naming
  its variable. `ADDR` has no default now.
- **TLS.** `serverTLS` reads the certificate and its key from the files
  `TLS_CERT_FILE` and `TLS_KEY_FILE` name, through the Host's `FS`. A proxy
  in front connects over TLS, as
  [Behind a proxy](/humus/pr-preview/pr-1432/services/http/serving/#behind-a-proxy)
  describes.
- **Telemetry.** `buildTelemetry` sources the OpenTelemetry variables and
  names the service `users`, as
  [Writing the Builder](/humus/pr-preview/pr-1432/fundamentals/telemetry/#writing-the-builder)
  shows. Each request is a span, with its metrics.
- **The probes.** The kubelet probes `/health/liveness` and
  `/health/readiness` on the service's port, with `scheme: HTTPS`, as
  [Health probes](/humus/pr-preview/pr-1432/services/http/serving/#health-probes)
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

`httpserver` names each span after the request's `Pattern`, which Gin
doesn't set. `route` sets it to Gin's route, such as `/v1/users/:id`:

```go
func route(c *gin.Context) {
	c.Request.Pattern = c.FullPath()
	c.Next()
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
  Gin answered 404.
- **A path that isn't clean,** such as `/v1//users`, redirects to the clean
  one, as `http.ServeMux` does, where Gin answered 404.

## Step 2: move /v1/users/{id}

The next steps move the routes onto `openapi/v3` a path at a time. The Gin
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

The problems `abort` answered with become problem details of the service's
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

A handler gets its parameter and its decoded body, checks the body against
its `binding` tags, and returns its result or an error:

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

The validator is the library Gin validates with, reading the tags Gin
reads:

```go
func newValidator() *validator.Validate {
	v := validator.New()
	v.SetTagName("binding")
	return v
}
```

`RequestErrors` answers a parameter or a body the API rejects, and
`CatchAll` anything else, such as a panic, which the API logs. Like
`handlePanic`'s, its answer says nothing of the failure:

```go
func serverError(error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
	}
}
```

`Init` serves the API, which hands Gin every request no operation serves.
Both share the store, and `http.MaxBytesHandler` limits the API's bodies as
`limitBody` limits Gin's:

```go
store := newStore()
r := newServer(store, s.Process.Stdout, s.Process.Stderr)

api, err := openapi.NewAPI(s,
	openapi.Info{Title: "Users", Version: "1.0.0"},
	openapi.Unmatched{
		NotFound:         r,
		MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
	},
	openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
	openapi.CatchAll(http.StatusInternalServerError, serverError, jsoncodec.Problem),
	(&users{store: store, validate: newValidator()}).operations(),
)
if err != nil {
	return nil, err
}

hs := health.NewService()
mux := http.NewServeMux()
mux.Handle("/", http.MaxBytesHandler(api, maxBodyBytes))
mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))
```

A method `/v1/users/{id}` doesn't serve is the API's 405 now, since Gin
serves nothing at that path. Gin keeps the two routes on `/v1/users`:

```go
h := &handlers{store: store}
users.GET("", h.listUsers)
users.POST("", h.createUser)
return r
```

What clients see change on a route that moves:

- **The detail of a 400 or a 413** is `openapi`'s own, such as
  `path parameter "id": "ada" isn't an integer of 64 bits`.
- **A body that isn't JSON,** by its `Content-Type` or for want of one, is a
  415, where Gin read any body as JSON.
- **Data after the JSON value** is a 400, where Gin ignored it.
- **A trailing slash,** as in `/v1/users/1/`, is a 404, where Gin redirected
  to the path without it.
- **A JSON answer's `Content-Type`** is `application/json`, without Gin's
  `; charset=utf-8`.

## Step 3: move /v1/users, and drop Gin

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

With no route left on Gin, the API answers unmatched requests itself, and
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
	append((&users{store: newStore(), validate: newValidator()}).operations(),
		openapi.Document(openapi.Root().Lit("openapi.json")),
	),
)
```

Gin's code goes, `newServer`, its middleware, `abort` and `route`, and the
module's
[`go.mod`](https://github.com/z5labs/humus/blob/main/example/http/gin/step3/go.mod)
no longer requires Gin. The `binding` tags stay for `newValidator`, and tags
beside them describe the bodies in the document:

```go
type userInput struct {
	Name  string `json:"name" binding:"required" required:"true" minLength:"1"`
	Email string `json:"email" binding:"required,email" required:"true" format:"email"`
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

- **A query parameter given twice, or given empty,** is a 400, where Gin
  read the first value, or the default.
- **The document** is served at `/openapi.json`.
