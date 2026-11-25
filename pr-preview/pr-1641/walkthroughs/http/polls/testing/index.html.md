# Testing

## The Site's unit tests

The Site's tests serve it on the humustest Host, with a Store of their own:

```go
	p, status := httpservertest.Start(t, h.Host(), func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = "polls.internal:8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		u, err := ui.New(polls, polls, settings.WriteTimeout)
		if err != nil {
			return nil, err
		}
		site, err := u.Site(s)
		if err != nil {
			return nil, err
		}
		svc, err := httpserver.NewService(ctx, s, h, settings, site)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(append([]humus.Option{}, opts...)...))
```

Each test runs inside `synctest`, and checks the HTML a request gets:

```go
		fragment := vote(t, b, p, choose(p.options[1]), sameOrigin)
		require.Equal(t, http.StatusOK, fragment.status)
		require.Equal(t, "text/html; charset=utf-8", fragment.header.Get("Content-Type"))
		require.Equal(t, csp, fragment.header.Get("Content-Security-Policy"))
		require.Equal(t, []string{"HX-Request"}, fragment.header.Values("Vary"))
		require.Equal(t, "<div id=\"results\">\n<ul id=\"options\" sse-swap=\"results\">\n"+
			"<li><span class=\"option\">Tabs</span> <span class=\"votes\">0</span></li>\n"+
			"<li><span class=\"option\">Spaces</span> <span class=\"votes\">1</span></li>\n</ul>\n"+
			"<p class=\"note\">Your vote is counted.</p>\n</div>", fragment.body)
```

## The in-memory Store

`storetest` is the tests' Store. It keeps every rule the PostgreSQL Store
keeps, so a test that passes on it passes on PostgreSQL:

```go
func (s *database) vote(pollID uuid.UUID, optionID int64, voter string) error {
	p, ok := s.polls[pollID]
	if !ok {
		return store.ErrNoSuchPoll
	}
	if p.voters[voter] {
		return store.ErrAlreadyVoted
	}
	i := slices.IndexFunc(p.options, func(o store.Option) bool { return o.ID == optionID })
	if i < 0 {
		return store.ErrNotAnOption
	}
	p.voters[voter] = true
	p.options[i].Votes++
	return nil
}
```

It is the listener too. A vote sends the poll's results to each of its
subscribers, keeping only the newest for one that doesn't keep up:

```go
func (s *database) publish(pollID uuid.UUID) {
	for ch := range s.subs[pollID] {
		select {
		case <-ch:
		default:
		}
		ch <- s.polls[pollID].results(pollID)
	}
}
```

Two Sites on one `storetest.Store` are two replicas, so a feed on one gets a
vote through the other:

```go
		polls := storetest.New()
		p := newPoll(t, polls, "Tabs or spaces?", "Tabs", "Spaces")
		other := newPoll(t, polls, "Vim or Emacs?", "Vim", "Emacs")
		watching, stopWatching := start(t, polls)
		voting, stopVoting := start(t, polls)

		f := open(t, t.Context(), watching, p.feedURL())
		require.Equal(t, results("Tabs=0", "Spaces=0"), <-f.events)

		voter := browser(t, voting)
		require.Equal(t, http.StatusOK, vote(t, voter, p, choose(p.options[1]), sameOrigin).status)
		require.Equal(t, results("Tabs=0", "Spaces=1"), <-f.events, "a vote through another replica")
```

`storetest`'s own tests check the same rules as the PostgreSQL Store's.

## Init's unit tests

`Init`'s tests give `initWith` a fake database, around a `storetest.Store`:

```go
func (db *fakeDatabase) open(_ context.Context, s *humus.Setup, _ humus.Host, _ *pgxpool.Config) (backend, error) {
	return backend{
		store:     fakeStore{db: db, Store: db.WithFaults(s.Faults)},
		results:   db.Store,
		listen:    db.listen,
		reached:   db.ping,
		listening: db.listening,
	}, nil
}

func (db *fakeDatabase) init(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
	return initWith(ctx, s, h, db.open)
}
```

So a test holds a vote in flight, sends SIGTERM, and checks that the vote
counts and the open feed ends cleanly as `Shutdown` begins:

```go
		end := <-fd.ended
		require.NoError(t, end.err, "a clean end, which htmx's SSE extension reconnects after")
		require.Equal(t, 5*time.Second, end.at.Sub(sigterm), "the feed ends as Shutdown begins, after the drain")
		s.exited(t)
		require.Equal(t, []int64{0, 1}, counts(t, db.Store, p.id), "the vote in flight at SIGTERM counts")
```

## The Store's unit tests

The Store's unit tests dial a database that isn't there, through the
humustest Host, so every query fails as `ErrUnavailable`:

```go
	h := humustest.NewHost(t)
	cfg, err := pgxpool.ParseConfig("postgres://polls:" + rand.Text() + "@db.internal:5432/polls?sslmode=disable")
	require.NoError(t, err)
	cfg.ConnConfig.DialFunc = h.DialTCP
```

Behind the `testcontainers` tag, the Store and the Listener run against
PostgreSQL in a container, with `schema.sql` applied.

Run the unit tests with `go test`:

```sh
go -C example/http/polls test ./...
```

## The shared cases

The cases are the package `cases`, which imports neither the App nor
humustest. A `Target` says how to reach two or more replicas:

```go
type Target struct {
	Replicas       []*url.URL
	Browser        func() *http.Client
	TrustedBrowser func() *http.Client
	ScenarioKey    string
	Timeout        time.Duration
}
```

| Field | What it is |
|---|---|
| `Replicas` | each replica's URL |
| `Browser` | a new browser: a client with a cookie jar of its own, which trusts the replicas' certificate |
| `TrustedBrowser` | the same, presenting the cases' client certificate, so a replica trusts its scenarios |
| `ScenarioKey` | the baggage member the service decodes a scenario from |
| `Timeout` | how long a case may take |

Every case runs on its own polls, so they all run at once:

```go
type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var Cases = []Case{
	{Name: "APollsLife", Run: aPollsLife},
	{Name: "AFormWithProblems", Run: aFormWithProblems},
	{Name: "OncePerBrowser", Run: oncePerBrowser},
	{Name: "CrossSite", Run: crossSite},
	{Name: "NotFoundAndWrongMethods", Run: notFoundAndWrongMethods},
	{Name: "EveryReplica", Run: everyReplica},
	{Name: "Headers", Run: headers},
	{Name: "TheDatabaseDown", Run: theDatabaseDown},
	{Name: "ASlowDatabase", Run: aSlowDatabase},
	{Name: "AnUntrustedScenario", Run: anUntrustedScenario},
}
```

| Case | What it shows |
|---|---|
| `APollsLife` | a poll is created, and two browsers, on two replicas, each vote once |
| `AFormWithProblems` | each problem with the form is a 422 that shows it in place, and creates nothing |
| `OncePerBrowser` | a browser's second vote is a 409, and two sent at once count once |
| `CrossSite` | a vote from another site is the 403 page, and counts nothing |
| `NotFoundAndWrongMethods` | the 404 and 405 pages, and the scripts at their URLs |
| `EveryReplica` | a feed on every replica gets each vote, whichever replica took it |
| `Headers` | the voter cookie's attributes, and every page's `Content-Type` and CSP |

The rest rehearse failures, as [The failure injection cases](#the-failure-injection-cases)
shows.

A browser is on one replica, so a case reaches every replica by opening
browsers on each:

```go
func everyReplica(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := newBrowser(t, target, 0).createPoll(ctx, t, "Tabs", "Spaces")
	first := newBrowser(t, target, 0)
	first.vote(ctx, t, p, first.open(ctx, t, p), "Tabs").html(t, http.StatusOK)

	var feeds []*feed
	for i := range target.Replicas {
		f := newBrowser(t, target, i).openFeed(ctx, t, p)
		require.Equal(t, tally("Tabs=1", "Spaces=0"), f.next(t), "%s starts with the current results", f.request)
		feeds = append(feeds, f)
	}

	for i, want := range [][]count{tally("Tabs=1", "Spaces=1"), tally("Tabs=1", "Spaces=2")} {
		voter := newBrowser(t, target, i*last(target))
		voter.vote(ctx, t, p, voter.open(ctx, t, p), "Spaces").html(t, http.StatusOK)
		for _, f := range feeds {
			f.until(t, want)
		}
	}
}
```

## The cases, inside synctest

Two replicas run on one humustest Host, each at an address of its own, and
share one fake database:

```go
var replicaAddrs = []string{addr, "polls.internal:9443"}
```

```go
	for _, a := range replicaAddrs {
		r := f
		r.env = maps.Clone(f.env)
		r.env["ADDR"] = a
		p, status := r.start(t, db.init)
		select {
		case <-f.h.Listening(a):
		case st := <-status:
			t.Fatalf("the replica at %s exited %d: %v", a, st.Code(), st.Cause())
		}
		running = append(running, &service{f: r, p: p, status: status})
		replicas = append(replicas, &url.URL{Scheme: "https", Host: a})
	}
```

Each case runs in a bubble of its own, on replicas of its own, so a delay
takes no real time:

```go
func TestInit_passes_the_shared_cases_on_two_replicas_sharing_a_Store(t *testing.T) {
	for _, c := range cases.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				target, stop := startReplicas(t)
				c.Run(t, target)
				stop()
			})
		})
	}
}
```

They run with the unit tests:

```sh
go -C example/http/polls test ./app/
```

## The cases, against PostgreSQL

Behind the `testcontainers` tag, two replicas run on `humus.OSHost()`, each
on a port of its own, against PostgreSQL in a container with `schema.sql`
applied:

```go
	for range 2 {
		a := freeAddr(t)
		env := map[string]string{"ADDR": a}
		for name, file := range files {
			env[name] = file
		}
		p, status := humustest.Start(t, humus.OSHost(), Init,
			humustest.WithApp(httpserver.Defaults()...), humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder), humus.WithFaults(Faults)),
			humustest.WithEnv(env),
		)
		running = append(running, replica{addr: a, p: p, status: status})
		replicas = append(replicas, &url.URL{Scheme: "https", Host: a})
	}
	for _, r := range running {
		r.waitReady(t, client)
	}
```

The listener on each replica LISTENs on PostgreSQL, so `EveryReplica` runs
on the real notification here. Every case runs against the two replicas,
which then have to exit 0 on SIGTERM:

```go
	t.Run("cases", func(t *testing.T) {
		for _, c := range cases.Cases {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})

	for _, r := range running {
		r.stop(t)
	}
```

```sh
go -C example/http/polls test -tags testcontainers ./...
```

## The cases, on kind

Behind the `functional` tag, the same cases run against the kind overlay
that `up.sh` brings up, which [Deploying](/humus/pr-preview/pr-1641/walkthroughs/http/polls/deploying/)
covers. Each replica is a ready pod, reached through a `kubectl
port-forward` of its own, so the cases reach both pods and not one Service:

```go
	var replicas []*url.URL
	for _, pod := range readyPods(t) {
		replicas = append(replicas, &url.URL{Scheme: "https", Host: forward(t, "pod/"+pod, port)})
	}

	return cases.Target{
		Replicas:       replicas,
		Browser:        browser(t, newTransport(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})),
		TrustedBrowser: browser(t, newTransport(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})),
		ScenarioKey:    scenarioKey,
		Timeout:        timeout,
	}
```

The CA and the cases' certificate come from the `polls-cases` Secret that
`up.sh` makes:

```go
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(secret(t, "polls-cases", "ca.crt")), "secret polls-cases holds no CA certificate")
	certificate, err := tls.X509KeyPair(secret(t, "polls-cases", "tls.crt"), secret(t, "polls-cases", "tls.key"))
	require.NoError(t, err, "secret polls-cases' tls.crt and tls.key")
```

```sh
go -C example/http/polls test -tags functional ./functional/
```

## The failure injection cases

A case rehearses a failure from a trusted browser, with a scenario in its
requests' `Baggage` header:

```go
func (b browser) rehearsing(t testing.TB, rules ...fit.Rule) browser {
	t.Helper()
	require.NotEmpty(t, b.key, "Target.ScenarioKey")
	codec, err := jsonscenario.New(b.key)
	require.NoError(t, err, "Target.ScenarioKey")
	bag, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	b.scenario = bag.String()
	return b
}
```

| Case | Its scenario | What it shows |
|---|---|---|
| `TheDatabaseDown` | aborts `postgresql:polls`, from a trusted browser | the 503 page, and a vote that counts nothing, then one that counts |
| `ASlowDatabase` | delays `postgresql:polls` 2s, from a trusted browser | the vote waits out the delay, and counts |
| `AnUntrustedScenario` | aborts `postgresql:polls`, from a browser without the cases' certificate | the scenario is ignored, and the vote counts |

```go
func theDatabaseDown(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := newBrowser(t, target, 0).createPoll(ctx, t, "Tabs", "Spaces")
	b := newTrustedBrowser(t, target, last(target))
	seen := b.open(ctx, t, p)

	down := b.rehearsing(t, abort(databasePoint))
	r := down.send(ctx, t, http.MethodPost, p.votes(), url.Values{"option": {seen.option(t, "Tabs")}}, down.fromHTMX(p.path, resultsTarget))
	r.errorPage(t, http.StatusServiceUnavailable, "Service Unavailable", unreachable)
	require.NotContains(t, r.body, counted)
	require.Equal(t, tally("Tabs=0", "Spaces=0"), b.open(ctx, t, p).options, "a vote the database didn't take counts nothing")

	voted := b.vote(ctx, t, p, seen, "Tabs")
	voted.html(t, http.StatusOK)
	require.Equal(t, counted, voted.note)
	require.Equal(t, tally("Tabs=1", "Spaces=0"), voted.options)
}
```

More run inside synctest only, beside the shared cases: a delay is capped
at `FAULT_MAX_DELAY`, and without a `FAULT_TRUST_CA_FILE` no scenario is
trusted, even from the cases' certificate:

```go
				sent := time.Now()
				page := send(t, s.trusted(t), http.MethodGet, p.url(""), nil, rehearsing(t, nil, slowQueries))
				require.Equal(t, http.StatusOK, page.status, "%s", page.body)
				require.Equal(t, tc.want, time.Since(sent), "the page's one query waits out the delay, clamped")
```

## The browser cases, on kind

Only a browser runs the UI's htmx, so these cases drive headless Chrome
with chromedp. They run only on kind, since a container's browser can't
reach humustest's in-memory Host.

Chrome is the overlay's headless-shell, reached through a `kubectl
port-forward` to its DevTools port. Each browser is a browser context of
its own, with its own cookies:

```go
	alloc, cancel := remote.NewAllocator(context.Background(), "ws://"+forward(t, "deployment/"+headlessShell, devtoolsPort)+"/")
	t.Cleanup(cancel)
```

```go
func newChrome(t *testing.T, alloc context.Context) *chrome {
	t.Helper()
	b := &chrome{}
	ctx, cancel := chromedp.NewContext(alloc, chromedp.WithNewBrowserContext(), chromedp.WithListener(b.record))
	t.Cleanup(cancel)
	b.ctx = ctx
	require.NoError(t, chromedp.Do(ctx), "opening a browser context in headless-shell")
	return b
}
```

Chrome reaches the site by its Service's name:

```go
	site          = "https://polls:8443"
```

| Case | What it shows |
|---|---|
| `AVoteReachesAnotherBrowser` | browser A votes, and browser B's results show it from its feed alone, with no reload |
| `AFormWithProblemsInPlace` | each problem shows in place, from the 422 htmx swaps in, and fixing them creates the poll |
| `ARolloutWithAPageOpen` | with B's page open, a `kubectl rollout restart` completes, and A's vote then reaches B, with no reload |

```go
	t.Run("AVoteReachesAnotherBrowser", func(t *testing.T) {
		a, b := newChrome(t, alloc), newChrome(t, alloc)
		poll := a.createPoll(t, "Which colour?", "Red", "Blue")
		b.open(t, poll)

		a.vote(t, "Red")
		a.waitNote(t, "Your vote is counted.")
		a.waitVotes(t, stepLimit, "Red", "1")
		b.waitVotes(t, stepLimit, "Red", "1")

		require.Empty(t, b.requests(http.MethodPost), "browser B sent no POST")
		require.Equal(t, []string{poll}, b.navigations(), "browser B stays on the poll's page")
		a.clean(t)
		b.clean(t)
	})
```

In every case, no page throws, and the browser reports no CSP violation:

```go
func (b *chrome) clean(t *testing.T) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	require.Empty(t, b.exceptions, "the page threw")
	require.Empty(t, b.security, "the browser reported a security problem, such as a CSP violation")
}
```

They run with the shared cases on kind:

```sh
go -C example/http/polls test -tags functional ./functional/
```

Next: [Deploying](/humus/pr-preview/pr-1641/walkthroughs/http/polls/deploying/)
