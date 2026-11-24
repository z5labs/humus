# Live results

## On the page

The poll's page connects to its feed with htmx's SSE extension, from the
`div` around its results:

```html
<div hx-ext="sse" sse-connect="{{.FeedURL}}">
{{template "results" .Results}}
</div>
```

Each `results` event replaces the options' list with the `options`
fragment it carries:

```html
<ul id="options" sse-swap="results">
{{template "options" .Options}}</ul>
```

## The feed

The feed subscribes to the poll before it reads it, so a vote between the
two isn't missed. It sends the poll's results first, then each change:

```go
func (ui *UI) feed(w http.ResponseWriter, r *http.Request) error {
	id, err := pollFrom(r)
	if err != nil {
		return err
	}
	changed, unsubscribe := ui.results.Subscribe(id)
	defer unsubscribe()
	p, err := ui.readPoll(r, id)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	stream, err := sse.NewStream(w, ui.writeTimeout)
	if err != nil {
		return err
	}
	if r.Method == http.MethodHead {
		return nil
	}
	if err := ui.sendResults(stream, p); err != nil {
		return err
	}
	stop := httpserver.ShuttingDown(r.Context())
	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-r.Context().Done():
			return nil
		case p := <-changed:
			if err := ui.sendResults(stream, p); err != nil {
				return err
			}
			keepAlive.Reset(keepAliveInterval)
		case <-keepAlive.C:
			if err := stream.Comment("keep-alive"); err != nil {
				return err
			}
		}
	}
}
```

A poll that doesn't exist gets the 404 page, since nothing has been written
yet. A HEAD gets the stream's headers and nothing else.

## Its events

An event is the `options` fragment, rendered into a buffer first:

```go
const (
	resultsEvent      = "results"
	keepAliveInterval = 15 * time.Second
)
```

```go
func (ui *UI) sendResults(stream *sse.Stream, p store.Poll) error {
	b, err := ui.execute("poll.html", "options", p.Options)
	if err != nil {
		return err
	}
	return stream.Send(sse.Event{Name: resultsEvent, Data: strings.TrimSuffix(b.String(), "\n")})
}
```

`sse.NewStream` takes the server's `WriteTimeout`, and sets a write
deadline around each event, as
[Event streams](/humus/pr-preview/pr-1613/services/http/serving/#event-streams)
shows. `UI` gets it from `Init`.

## Its keep-alives

With no change for 15 seconds, the feed writes a comment, so a proxy
doesn't close the connection as idle. An event resets the ticker.

## At Shutdown

`httpserver.ShuttingDown` closes when the Service's `Shutdown` begins, and
the feed returns, so `Shutdown` doesn't wait on it to the end of the grace
period. Returning ends the stream cleanly, and htmx's SSE extension
connects again, to a replica that is still serving, and gets the poll's
results afresh.

A browser that leaves ends the request's context, and the feed returns
too. Either way, `unsubscribe` takes it off the listener.

Next: [Init](/humus/pr-preview/pr-1613/walkthroughs/http/polls/init/)
