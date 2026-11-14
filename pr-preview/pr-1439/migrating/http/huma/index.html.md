# Migrating from Huma

This guide moves a small [Huma](https://huma.rocks) v2 API to humus. Its
first step runs the Huma API on humus, so the process, settings, TLS,
telemetry, probes and shutdown are humus's from then on. Its next steps move
the operations onto `openapi/v3` a few at a time, while Huma serves the rest,
until the API no longer requires Huma and one OpenAPI document, `openapi/v3`'s,
describes it.

Huma and `openapi/v3` both declare typed operations and write the document
from them, so most of the move translates declarations: Huma's input and
output structs, and their tags, become typed paths, parameters and codecs.
Two things let the operations move a few at a time. A Huma API on its
`humago` adapter is served by an `http.ServeMux`, which
`httpserver.NewService` serves. And an `openapi.API` hands every request no
operation serves to its `Unmatched` handlers, which can be that ServeMux.

## Huma and humus

| Huma | humus |
|---|---|
| `humacli.New`, and its `hooks.OnStart` | `httpserver.Main`, and an `Init` that returns `httpserver.NewService`: [Serving HTTP](/humus/pr-preview/pr-1439/services/http/serving/#a-service) |
| the CLI's `Options`, from `--port` or `SERVICE_PORT` | `httpserver.Settings`, sourced and checked with `config.Struct`: [Configuring a humus service](/humus/pr-preview/pr-1439/fundamentals/configuration/) |
| `ListenAndServe`, with TLS left to a proxy | `Settings.TLS`, which is required: [TLS](/humus/pr-preview/pr-1439/services/http/serving/#tls) |
| `hooks.OnStop` and `Shutdown` on SIGTERM, within 5s | a 5s drain, then a 10s grace period: [The defaults](/humus/pr-preview/pr-1439/services/http/serving/#the-defaults) |
| `slog` on stdout, in `main` | for the API's own records, the Setup's `LoggerProvider`, through the OpenTelemetry Logs API: [Telemetry](/humus/pr-preview/pr-1439/fundamentals/telemetry/#where-the-providers-come-from) |
| `humago.New` and `huma.DefaultConfig` | `openapi.NewAPI` and `openapi.Info`: [An API](/humus/pr-preview/pr-1439/services/http/openapi/#an-api) |
| `huma.NewGroup` | a `Path` that the paths under it share: [Paths](/humus/pr-preview/pr-1439/services/http/openapi/#paths) |
| `UseMiddleware`, for the request log | while Huma serves operations, the log on the Process's stdout; then none: each request is a span, with its metrics: [Telemetry](/humus/pr-preview/pr-1439/services/http/serving/#telemetry) |
| `huma.Register` and `huma.Operation`, with its `OperationID`, `Summary` and `DefaultStatus` | `openapi.Get`, `Post`, `Put` and `Delete`, each with a typed handler, and `OperationID`, `Summary` and `Status`: [Operations](/humus/pr-preview/pr-1439/services/http/openapi/#operations) |
| `path`, `query` and `header` fields, with `doc`, `default` and their constraints | `openapi.PathParam`, `QueryParam` and `HeaderParam`, with `Description`, `Default` and their constraints, and `openapi.ParamFrom`: [Parameters](/humus/pr-preview/pr-1439/services/http/openapi/#parameters) |
| `Body` fields, with `doc`, `example` and `format` tags | the operation's decoder and encoder, `jsoncodec`, with `description`, `example`, `format` and `required` tags: [jsoncodec](/humus/pr-preview/pr-1439/services/http/openapi/#jsoncodec) |
| the body checked against its schema, such as `minLength` and `format` | none: the handler checks its body, and returns an error an `openapi.Error` answers: [Error](/humus/pr-preview/pr-1439/services/http/openapi/#error) |
| `huma.Error404NotFound` | `openapi.Error`, with `problem.Details`: [Problem details](/humus/pr-preview/pr-1439/services/http/openapi/#problem-details) |
| Huma's 400, 413, 415 and 422 for a request it rejects | `RequestErrors`, with `problem.FromRequestError`: [RequestErrors](/humus/pr-preview/pr-1439/services/http/openapi/#requesterrors) |
| Huma's 500 for an error without a status | `CatchAll`: [CatchAll](/humus/pr-preview/pr-1439/services/http/openapi/#catchall) |
| `MaxBodyBytes`, 1 MiB by default | none: `http.MaxBytesHandler` around the API, and the decoder answers 413: [Operations](/humus/pr-preview/pr-1439/services/http/openapi/#operations) |
| the ServeMux's 404 and 405 | `Unmatched`: [Unmatched](/humus/pr-preview/pr-1439/services/http/openapi/#unmatched) |
| the ServeMux's `HEAD` for every `GET` | none: the API answers `HEAD` with a 405, so clients send `GET`: [Unmatched](/humus/pr-preview/pr-1439/services/http/openapi/#unmatched) |
| `/openapi.json` | `openapi.Document`: [The document](/humus/pr-preview/pr-1439/services/http/openapi/#the-document) |
| `/openapi.yaml`, and the OpenAPI 3.0 documents | none: the document is JSON, in OpenAPI 3.2.0, so a tool that reads only YAML or 3.0 reads a copy converted from it: [The document](/humus/pr-preview/pr-1439/services/http/openapi/#the-document) |
| the docs page, `/docs` | none: a page of your own, such as one that loads Stoplight Elements, reads `/openapi.json`: [The document](/humus/pr-preview/pr-1439/services/http/openapi/#the-document) |
| `/schemas`, each answer's `$schema` and its `Link` header | none: the document holds every schema inline: [The document](/humus/pr-preview/pr-1439/services/http/openapi/#the-document) |
| no probes | `httphealth.NewHandler` and a health Service: [Health probes](/humus/pr-preview/pr-1439/services/http/serving/#health-probes) |
| `httptest.NewServer` | `httpservertest.Start`, inside `testing/synctest`: [Testing](/humus/pr-preview/pr-1439/services/http/serving/#testing) |

## The Huma API

The guide's code is under
[`example/http/huma`](https://github.com/z5labs/humus/tree/main/example/http/huma),
a module for each step:

```text
example/http/huma/
├── cases/    the black-box test cases every step passes
├── start/    the Huma API the guide starts from
├── step1/    the Huma API, run on humus
├── step2/    /v1/users/{id} on openapi/v3, the rest on Huma
└── step3/    every operation on openapi/v3, without Huma
```

The API is one of users, kept in memory, built as Huma's
[tutorial](https://huma.rocks/tutorial/your-first-api/) builds one, on Go's
`http.ServeMux`: operations registered with `huma.Register`, in groups, with
middleware:

```go
func newServer(store *store, logger *slog.Logger) *http.ServeMux {
	router := http.NewServeMux()
	api := humago.New(router, huma.DefaultConfig("Users", "1.0.0"))

	v1 := huma.NewGroup(api, "/v1")
	v1.UseMiddleware(logRequests(logger))
	users := huma.NewGroup(v1, "/users")

	h := &handlers{store: store}
	huma.Register(users, huma.Operation{
		OperationID: "listUsers",
		Method:      http.MethodGet,
		Path:        "",
		Summary:     "List the users",
	}, h.listUsers)
	huma.Register(users, huma.Operation{
		OperationID:   "createUser",
		Method:        http.MethodPost,
		Path:          "",
		Summary:       "Create a user",
		DefaultStatus: http.StatusCreated,
	}, h.createUser)
	huma.Register(users, huma.Operation{
		OperationID: "getUser",
		Method:      http.MethodGet,
		Path:        "/{id}",
		Summary:     "Get a user",
	}, h.getUser)
	huma.Register(users, huma.Operation{
		OperationID: "updateUser",
		Method:      http.MethodPut,
		Path:        "/{id}",
		Summary:     "Update a user",
	}, h.updateUser)
	huma.Register(users, huma.Operation{
		OperationID:   "deleteUser",
		Method:        http.MethodDelete,
		Path:          "/{id}",
		Summary:       "Delete a user",
		DefaultStatus: http.StatusNoContent,
	}, h.deleteUser)
	return router
}
```

```go
func logRequests(logger *slog.Logger) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		next(ctx)
		logger.InfoContext(ctx.Context(), "request",
			"method", ctx.Method(),
			"path", ctx.URL().Path,
			"status", ctx.Status(),
		)
	}
}
```

Its inputs and outputs are structs. Their fields say where each value comes
from, and their tags what it must be and how the document describes it:

```go
type userInput struct {
	Name  string `json:"name" minLength:"1" doc:"The user's name." example:"Ada"`
	Email string `json:"email" format:"email" doc:"The user's email address." example:"ada@example.com"`
}

type listUsersInput struct {
	Limit  int `query:"limit" minimum:"1" maximum:"100" default:"20" doc:"The most users to list."`
	Offset int `query:"offset" minimum:"0" doc:"How many users to skip."`
}

type createUserInput struct {
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"64" doc:"Sent again with the same key, the request creates no second user."`
	Body           userInput
}
```

Huma checks a request against them before the handler runs, and answers one
it rejects with a problem detail. A handler returns its output or one of
Huma's errors:

```go
func (h *handlers) updateUser(_ context.Context, in *updateUserInput) (*userOutput, error) {
	updated, ok := h.store.update(in.ID, in.Body)
	if !ok {
		return nil, huma.Error404NotFound(fmt.Sprintf("There is no user %d.", in.ID))
	}
	return &userOutput{Body: updated}, nil
}
```

Huma serves the document at `/openapi.json` and `/openapi.yaml`, a docs page
at `/docs`, and each schema under `/schemas`.

`main` is a `humacli` CLI. It reads the port from `--port` or
`SERVICE_PORT`, serves plain HTTP, and shuts down gracefully on SIGINT or
SIGTERM:

```go
type Options struct {
	Port int `help:"Port to listen on" short:"p" default:"8888"`
}

func main() {
	cli := humacli.New(func(hooks humacli.Hooks, options *Options) {
		logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
		server := &http.Server{
			Addr:    fmt.Sprintf(":%d", options.Port),
			Handler: newServer(newStore(), logger),
		}

		hooks.OnStart(func() {
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("failed to start server", "error", err)
			}
		})

		hooks.OnStop(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(ctx); err != nil {
				logger.Error("failed to shut down server", "error", err)
			}
		})
	})

	cli.Run()
}
```

## The test cases

The `cases` module holds black-box cases, written once. Each takes the API's
URL and a client:

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
	{Name: "Retries", Run: Retries},
	{Name: "BadParameters", Run: BadParameters},
	{Name: "BadBodies", Run: BadBodies},
	{Name: "Unmatched", Run: Unmatched},
}
```

They check what every step keeps. Where a step changes what clients see, the
step names the change, and a case takes both answers: a parameter rejected
with Huma's 422 or `openapi/v3`'s 400, and a problem detail with no `type`,
as Huma writes it, or `about:blank`, which RFC 9457 makes the same:

```go
func (a answer) rejected(t testing.TB) {
	t.Helper()
	require.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, a.code, "%s: %s", a.request, a.body)
	a.problem(t, a.code)
}
```

Every step runs them all, each against an API of its own. The Huma API runs
them over `httptest`:

```go
func TestServer_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(newServer(newStore(), slog.New(slog.DiscardHandler)))
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

`Init` serves the Huma API's ServeMux, its operations unchanged, with
`httpserver.NewService`, next to the probes:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	logger := slog.New(slog.NewJSONHandler(s.Process.Stdout, nil))

	hs := health.NewService()
	mux := http.NewServeMux()
	mux.Handle("/", newServer(newStore(), logger))
	mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))

	svc, err := httpserver.NewService(s, settings, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, hs}, nil
}
```

From here on, these are humus's:

- **The process and its shutdown.** The request log writes to the Process's
  stdout, as it wrote to the real one. On SIGTERM the API keeps serving
  through a 5s drain, then waits up to 10s for requests in flight, as
  [The defaults](/humus/pr-preview/pr-1439/services/http/serving/#the-defaults)
  describes, so `humacli` and its hooks go.
- **The settings.** A missing or bad one makes the API exit 78, naming its
  variable. `ADDR` replaces `--port` and `SERVICE_PORT`, and has no default.
- **TLS.** `serverTLS` reads the certificate and its key from the files
  `TLS_CERT_FILE` and `TLS_KEY_FILE` name, through the Host's `FS`. A proxy
  in front connects over TLS, as
  [Behind a proxy](/humus/pr-preview/pr-1439/services/http/serving/#behind-a-proxy)
  describes.
- **Telemetry.** `buildTelemetry` sources the OpenTelemetry variables and
  names the service `users`, as
  [Writing the Builder](/humus/pr-preview/pr-1439/fundamentals/telemetry/#writing-the-builder)
  shows. Each request is a span, with its metrics, named after the pattern
  Huma's ServeMux matched, such as `GET /v1/users/{id}`.
- **The probes.** The kubelet probes `/health/liveness` and
  `/health/readiness` on the API's port, with `scheme: HTTPS`, as
  [Health probes](/humus/pr-preview/pr-1439/services/http/serving/#health-probes)
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

The cases run against the whole API, with `httpservertest.Start`, inside
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

- **HTTPS.** The API serves TLS alone, so clients, and any proxy in front,
  use `https`. A client that offers HTTP/2 gets it.
- **The probes.** `/health/liveness` and `/health/readiness` answer, where
  the ServeMux answered 404.

## Step 2: move /v1/users/{id}

The next steps move the operations onto `openapi/v3` a path at a time. The
groups become a path that the paths under it share, and the `path` field a
typed parameter:

```go
var userID = openapi.PathParam[int]("id").Description("The user's ID.")

var (
	v1        = openapi.Root().Lit("v1")
	usersPath = v1.Lit("users")
	userPath  = usersPath.Param(userID)
)
```

Huma's errors become problem details of the API's own, each given to the
operations that return it:

```go
type notFound struct{ problem.Details }

type unprocessable struct{ problem.Details }

var (
	userNotFoundError = openapi.Error[notFound](http.StatusNotFound, jsoncodec.Problem)
	invalidUserError  = openapi.Error[unprocessable](http.StatusUnprocessableEntity, jsoncodec.Problem)
)
```

The three operations on `/v1/users/{id}` keep their IDs and summaries:

```go
func (u *users) operations() []openapi.Operation {
	return []openapi.Operation{
		openapi.Get(userPath, u.getUser, jsoncodec.Encoder,
			openapi.OperationID("getUser"),
			openapi.Summary("Get a user"),
			userNotFoundError,
		),
		openapi.Put(userPath, u.updateUser, jsoncodec.Decoder, jsoncodec.Encoder,
			openapi.OperationID("updateUser"),
			openapi.Summary("Update a user"),
			userNotFoundError,
			invalidUserError,
		),
		openapi.Delete(userPath, u.deleteUser, openapi.EmptyEncoder,
			openapi.OperationID("deleteUser"),
			openapi.Summary("Delete a user"),
			userNotFoundError,
		),
	}
}
```

A handler gets its parameter and its decoded body, and returns its result or
an error. `jsoncodec` checks no tags, so the handler checks the body, as
`minLength` and `format` had Huma check it:

```go
func (u *users) updateUser(_ context.Context, req openapi.Request[userInput]) (user, error) {
	id := openapi.ParamFrom(req, userID)
	in := req.Body()
	if err := validate(in); err != nil {
		return user{}, err
	}
	updated, ok := u.store.update(id, in)
	if !ok {
		return user{}, userNotFound(id)
	}
	return updated, nil
}
```

```go
func validate(in userInput) error {
	if in.Name == "" {
		return invalidUser("The name is empty.")
	}
	if addr, err := mail.ParseAddress(in.Email); err != nil || addr.Address != in.Email {
		return invalidUser(fmt.Sprintf("%q isn't an email address.", in.Email))
	}
	return nil
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
`CatchAll` any other error, which the API logs. Its answer says nothing of
the failure:

```go
func serverError(error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
	}
}
```

`Init` serves the API, which hands Huma's ServeMux every request no
operation serves. Both share the store, and `http.MaxBytesHandler` limits
the API's bodies to 1 MiB, as `MaxBodyBytes` limits Huma's:

```go
store := newStore()
logger := slog.New(slog.NewJSONHandler(s.Process.Stdout, nil))

api, err := openapi.NewAPI(s,
	openapi.Info{Title: "Users", Version: "1.0.0"},
	openapi.Unmatched{
		NotFound:         newServer(store, logger),
		MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
	},
	openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
	openapi.CatchAll(http.StatusInternalServerError, serverError, jsoncodec.Problem),
	(&users{store: store}).operations(),
)
if err != nil {
	return nil, err
}

hs := health.NewService()
mux := http.NewServeMux()
mux.Handle("/", http.MaxBytesHandler(api, maxBodyBytes))
mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))
```

Huma keeps the moved operations registered, though no request reaches them
now. So while the operations are split, clients get Huma's document at
`/openapi.json`, which still lists every operation, as Huma served it. The
last step replaces it with `openapi/v3`'s.

What clients see change on an operation that moves:

- **A parameter the API rejects** is a 400, where Huma answered 422.
- **A problem detail** has the `type` `about:blank`, and no `$schema` or
  `errors`. Its `detail` names the first failure, such as
  `path parameter "id": "ada" isn't an integer of 64 bits`, where Huma's
  said `validation failed` and listed each failure in `errors`.
- **A user** has no `$schema`, and the answer no `Link` header.
- **A body without a `Content-Type`** is a 415, where Huma read it as JSON.
- **A property `userInput` doesn't have** is ignored, where Huma answered
  422.
- **A `HEAD` request** is a 405, and `Allow` doesn't list `HEAD`, where the
  ServeMux served it as a `GET`.
- **A method `/v1/users/{id}` doesn't serve** is a 405 with a problem
  detail, where the ServeMux answered in plain text.

## Step 3: move /v1/users, and drop Huma

The two operations left take their query and header parameters, with their
constraints and default:

```go
var (
	pageLimit  = openapi.QueryParam[int]("limit").Description("The most users to list.").Minimum(1).Maximum(100).Default(20)
	pageOffset = openapi.QueryParam[int]("offset").Description("How many users to skip.").Minimum(0)
)

var idempotencyKey = openapi.HeaderParam[string]("Idempotency-Key").
	Description("Sent again with the same key, the request creates no second user.").
	MaxLength(64)
```

```go
openapi.Get(usersPath, u.listUsers, jsoncodec.Encoder,
	openapi.OperationID("listUsers"),
	openapi.Summary("List the users"),
	openapi.Query(pageLimit),
	openapi.Query(pageOffset),
),
openapi.Post(usersPath, u.createUser, jsoncodec.Decoder, jsoncodec.Encoder,
	openapi.OperationID("createUser"),
	openapi.Summary("Create a user"),
	openapi.Status(http.StatusCreated),
	openapi.Header(idempotencyKey),
	invalidUserError,
),
```

```go
func (u *users) createUser(_ context.Context, req openapi.Request[userInput]) (user, error) {
	in := req.Body()
	if err := validate(in); err != nil {
		return user{}, err
	}
	return u.store.create(openapi.ParamFrom(req, idempotencyKey), in), nil
}
```

With no operation left on Huma, the API answers unmatched requests itself,
and serves its document at `/openapi.json`, where Huma served its own:

```go
api, err := openapi.NewAPI(s,
	openapi.Info{Title: "Users", Version: "1.0.0"},
	openapi.Unmatched{
		NotFound:         problem.Handler(http.StatusNotFound, jsoncodec.Problem),
		MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
	},
	openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
	openapi.CatchAll(http.StatusInternalServerError, serverError, jsoncodec.Problem),
	append((&users{store: newStore()}).operations(),
		openapi.Document(openapi.Root().Lit("openapi.json")),
	),
)
```

Huma's code goes, `newServer`, `logRequests`, the handlers and their input
and output structs, and the module's
[`go.mod`](https://github.com/z5labs/humus/blob/main/example/http/huma/step3/go.mod)
no longer requires Huma. The bodies' tags describe them in the document:
`doc` becomes `description`, and a field Huma required by default is
`required:"true"`:

```go
type user struct {
	ID    int    `json:"id" required:"true" description:"The user's ID." example:"1"`
	Name  string `json:"name" required:"true" description:"The user's name." example:"Ada"`
	Email string `json:"email" required:"true" format:"email" description:"The user's email address." example:"ada@example.com"`
}

type userInput struct {
	Name  string `json:"name" required:"true" minLength:"1" description:"The user's name." example:"Ada"`
	Email string `json:"email" required:"true" format:"email" description:"The user's email address." example:"ada@example.com"`
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

- **A query parameter given twice, or given empty,** is a 400, where Huma
  read the first value, or the default.
- **The document** is OpenAPI 3.2.0, where Huma's was 3.1, with its schemas
  inline rather than under `components`, and its `Content-Type` is
  `application/json`, where Huma's was `application/openapi+json`.
- **`/openapi.yaml`, the OpenAPI 3.0 documents, `/docs` and `/schemas`** are
  404s.
- **A path no operation serves** is a 404 with a problem detail, where the
  ServeMux answered in plain text.
