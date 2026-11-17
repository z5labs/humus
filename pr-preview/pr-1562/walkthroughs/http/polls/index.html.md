# Polls

The polls service is a hypermedia UI, served by `httpserver` with the
`hypermedia` and `sse` packages, and driven by htmx in the browser:

- **a poll** is a question and two to five options, made with a form;
- **a vote** is one per browser on each poll;
- **the results** update live on every page open on the poll.

It runs as two or more replicas, and a page's feed is often on a different
replica from the one its vote reaches. One vote reaches every open page
this way:

1. the browser posts the vote to `/polls/{pollId}/votes`, on any replica;
2. that replica inserts it into PostgreSQL, and a trigger, in the same
   transaction, notifies the `votes` channel with the poll's id;
3. once the vote commits, PostgreSQL delivers the notification to every
   replica's listener;
4. each listener reads the poll's results once, and hands them to each of
   its feeds of that poll;
5. each feed sends them to its page as an event, and htmx swaps them in.

```text
browser ──POST vote──▶ replica A ──INSERT──▶ PostgreSQL: polls
                                                 │ NOTIFY votes, on commit
                                   ┌─────────────┴─────────────┐
                                   ▼                           ▼
                         replica A's listener        replica B's listener
                                   │                           │
                                   ▼                           ▼
                      its feeds ──SSE──▶ pages      its feeds ──SSE──▶ pages
```

## The files it ends with

Its code is a module of its own, at
[`example/http/polls`](https://github.com/z5labs/humus/tree/main/example/http/polls):

```text
example/http/polls/
├── main.go             httpserver.Main(app.Init, ...)
├── app/                Init, settings, the listener's Service, readiness, BuildTelemetry and Faults
├── store/              the Store and the Listener on PostgreSQL, and schema.sql
│   └── storetest/      the tests' in-memory Store, which keeps the same rules
├── ui/                 the Site: its paths, error page, forms, votes and feed
│   ├── templates/      the layout, the pages and their fragments
│   └── static/         htmx and its SSE extension, vendored
├── vendored_test.go    the version each vendored script is, checked
├── internal/certfile/  the certificate served from its files, read again when renewed
├── internal/latch/     a monitor that is healthy for good once its check first succeeds
├── cases/              the test cases the in-process tests, the container tests and kind share
├── functional/         the cases against kind, and in headless Chrome
├── deploy/             the base, the kind overlay and up.sh, the alerts and the dashboard
└── Dockerfile          the image
```

These pages build the schema and the store, the listener that reaches every
replica, and the Site, then the pages that create a poll, vote, and show
live results, then `Init` and `main`. Then they test it, deploy it, and
observe it, and end with the checklist it meets.

Next: [The schema and the store](/humus/pr-preview/pr-1562/walkthroughs/http/polls/schema/)
