# Voting

## The poll's page

A poll's page shows its results, and a button for each option. The
`results` fragment is what a vote swaps:

```html
{{define "main"}}<h1>{{.Question}}</h1>
<div hx-ext="sse" sse-connect="{{.FeedURL}}">
{{template "results" .Results}}
</div>
<p><a href="{{.HomeURL}}">Create another poll</a></p>{{end}}{{define "results"}}<div id="results">
<ul id="options" sse-swap="results">
{{template "options" .Options}}</ul>
{{with .Note}}<p class="note">{{.}}</p>
{{end}}{{with .Action}}<form method="post" action="{{.}}" hx-post="{{.}}" hx-target="#results" hx-swap="outerHTML">
{{range $.Options}}<button type="submit" name="option" value="{{.ID}}">{{.Text}}</button>
{{end}}</form>
{{end}}</div>{{end}}{{define "options"}}{{range .}}<li><span class="option">{{.Text}}</span> <span class="votes">{{.Votes}}</span></li>
{{end}}{{end}}{{template "layout" .}}
```

```go
type pollView struct {
	Title    string
	Question string
	Results  resultsView
	FeedURL  string
	HomeURL  string
}
```

```go
type resultsView struct {
	Options []store.Option
	Note    string
	Action  string
}

func votesURL(id uuid.UUID) string {
	return votesPath.URL(hypermedia.Arg(pollID, id.String())).String()
}
```

The handler reads the poll, and gives the browser its voter cookie before
it votes:

```go
func pollFrom(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(hypermedia.ParamFrom(r, pollID))
	if err != nil {
		return uuid.Nil, &hypermedia.RequestError{Status: http.StatusNotFound, Param: "pollId", Err: err}
	}
	return id, nil
}

func (ui *UI) readPoll(r *http.Request, id uuid.UUID) (store.Poll, error) {
	p, err := ui.polls.Poll(r.Context(), id)
	if errors.Is(err, store.ErrNoSuchPoll) {
		return store.Poll{}, &hypermedia.RequestError{Status: http.StatusNotFound, Param: "pollId", Err: err}
	}
	return p, err
}

func (ui *UI) poll(w http.ResponseWriter, r *http.Request) error {
	id, err := pollFrom(r)
	if err != nil {
		return err
	}
	p, err := ui.readPoll(r, id)
	if err != nil {
		return err
	}
	voter(w, r)
	return ui.render(w, http.StatusOK, "poll.html", newPollView(p, resultsView{Options: p.Options, Action: votesURL(id)}))
}

func newPollView(p store.Poll, results resultsView) pollView {
	return pollView{
		Title:    p.Question,
		Question: p.Question,
		Results:  results,
		FeedURL:  feedPath.URL(hypermedia.Arg(pollID, p.ID.String())).String(),
		HomeURL:  homePath.URL().String(),
	}
}
```

## The voter cookie

A browser is a random id in a `__Host-` cookie. A browser takes a
`__Host-` cookie only when it is `Secure`, on the path `/`, and has no
`Domain`, so no other host can set it:

```go
const (
	voterCookie = "__Host-voter"
	voterMaxAge = 400 * 24 * 60 * 60
)
```

```go
func voter(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(voterCookie); err == nil && voterID.MatchString(c.Value) {
		return c.Value
	}
	id := rand.Text()
	http.SetCookie(w, &http.Cookie{
		Name:     voterCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   voterMaxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return id
}
```

A cookie whose value isn't in the form `rand.Text` returns is replaced with a new one:

```go
var (
	voterID     = regexp.MustCompile(`^[A-Z2-7]{26}$`)
	optionField = hypermedia.Field[int64]("option").Required()
)
```

## One vote per browser

`vote` reads the option, and the store's errors decide the answer:

- **counted:** a 200;
- **voted already:** a 409, with no buttons left;
- **no option, or another poll's:** a 422, with the buttons again;
- **no such poll:** the 404 page.

```go
const (
	counted        = "Your vote is counted."
	alreadyVoted   = "This browser has voted on this poll already."
	chooseAnOption = "Choose one of the poll's options."
)
```

```go
func (ui *UI) vote(w http.ResponseWriter, r *http.Request) error {
	id, err := pollFrom(r)
	if err != nil {
		return err
	}
	form, err := hypermedia.ReadForm(r, optionField)
	if err != nil {
		return err
	}
	status, note, action := http.StatusOK, counted, ""
	if form.Problem(optionField) != nil {
		status, note, action = http.StatusUnprocessableEntity, chooseAnOption, votesURL(id)
	} else {
		err := ui.polls.Vote(r.Context(), id, hypermedia.FieldFrom(form, optionField), voter(w, r))
		switch {
		case errors.Is(err, store.ErrNoSuchPoll):
			return &hypermedia.RequestError{Status: http.StatusNotFound, Param: "pollId", Err: err}
		case errors.Is(err, store.ErrNotAnOption):
			status, note, action = http.StatusUnprocessableEntity, chooseAnOption, votesURL(id)
		case errors.Is(err, store.ErrAlreadyVoted):
			status, note = http.StatusConflict, alreadyVoted
		case err != nil:
			return err
		}
	}
	p, err := ui.readPoll(r, id)
	if err != nil {
		return err
	}
	results := resultsView{Options: p.Options, Note: note, Action: action}
	if isHTMX(w, r) {
		return ui.renderTemplate(w, status, "poll.html", "results", results)
	}
	return ui.render(w, status, "poll.html", newPollView(p, results))
}
```

htmx gets the `results` fragment, and the layout's `responseHandling` swaps
the 409 and the 422 as it does the 200. A browser without htmx gets the
whole page.

## Cross-origin requests

The cookie is the vote's only credential, so `http.CrossOriginProtection`
goes around the Site, and a POST from another origin gets a 403. Its deny
handler is the error page, so the 403 is a page too:

```go
protection := http.NewCrossOriginProtection()
protection.SetDenyHandler(http.HandlerFunc(ui.crossOrigin))
protected := protection.Handler(http.MaxBytesHandler(site, maxBodyBytes))
```

```go
func (ui *UI) crossOrigin(w http.ResponseWriter, r *http.Request) {
	err := ui.ErrorPage(w, r, &hypermedia.RequestError{Status: http.StatusForbidden, Err: errors.New("a cross-origin request")})
	if err != nil {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	}
}
```

It needs no token, so the pages carry none. A GET is never checked, so
every vote is a POST.

Next: [Live results](/humus/pr-preview/pr-1605/walkthroughs/http/polls/live-results/)
