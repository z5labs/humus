# Creating a poll

## The form

The home page is the form. It posts as a plain form, and htmx posts it
with `hx-post` and swaps the answer in its place:

```html
{{define "main"}}<h1>Polls</h1>
{{template "form" .Form}}{{end}}{{define "form"}}<form id="new-poll" method="post" action="{{.Action}}" hx-post="{{.Action}}" hx-swap="outerHTML">
{{template "field" .Question}}
{{range .Options}}{{template "field" .}}
{{end}}<button type="submit">Create the poll</button>
</form>{{end}}{{define "field"}}<p><label>{{.Label}} <input name="{{.Name}}" value="{{.Value}}" maxlength="{{.MaxLength}}"{{if .Required}} required{{end}}></label>{{with .Problem}} <span class="problem">{{.}}</span>{{end}}</p>{{end}}{{template "layout" .}}
```

Its view holds each field's value and problem, to render the form again:

```go
type field struct {
	Name      string
	Label     string
	Value     string
	Problem   string
	MaxLength int
	Required  bool
}

type pollForm struct {
	Action   string
	Question field
	Options  []field
}

func newPollForm() pollForm {
	f := pollForm{
		Action:   pollsPath.URL().String(),
		Question: field{Name: "question", Label: "Question", MaxLength: questionMaxLength, Required: true},
	}
	for i := range optionCount {
		f.Options = append(f.Options, field{
			Name:      fmt.Sprintf("option%d", i+1),
			Label:     fmt.Sprintf("Option %d", i+1),
			MaxLength: optionMaxLength,
			Required:  i < requiredOptions,
		})
	}
	return f
}
```

```go
type homeView struct {
	Title string
	Form  pollForm
}
```

```go
func (ui *UI) home(w http.ResponseWriter, r *http.Request) error {
	return ui.render(w, http.StatusOK, "home.html", homeView{Title: "Polls", Form: newPollForm()})
}
```

## The fields

The fields are declared once, with their rules: a question, and five
options, the first two required. A value of only spaces breaks `notBlank`:

```go
const (
	questionMaxLength = 200
	optionMaxLength   = 100
	optionCount       = 5
	requiredOptions   = 2
)

var (
	notBlank      = regexp.MustCompile(`\S`)
	questionField = hypermedia.Field[string]("question").Required().Pattern(notBlank).MaxLength(questionMaxLength)
	optionFields  = newOptionFields()
)

func newOptionFields() []hypermedia.FormField[string] {
	fields := make([]hypermedia.FormField[string], optionCount)
	for i := range fields {
		f := hypermedia.Field[string](fmt.Sprintf("option%d", i+1)).Pattern(notBlank).MaxLength(optionMaxLength)
		if i < requiredOptions {
			f = f.Required()
		}
		fields[i] = f
	}
	return fields
}
```

## Reading the form

`create` reads the body with `ReadForm`. A body it can't read, too large or
not a form, is a `RequestError`, which the error page answers:

```go
func (ui *UI) create(w http.ResponseWriter, r *http.Request) error {
	fields := make([]hypermedia.AnyField, 0, 1+len(optionFields))
	fields = append(fields, questionField)
	for _, of := range optionFields {
		fields = append(fields, of)
	}
	form, err := hypermedia.ReadForm(r, fields...)
	if err != nil {
		return err
	}
	f, question, options := readPollForm(form)
	htmx := isHTMX(w, r)
	if !f.valid() {
		if htmx {
			return ui.renderTemplate(w, http.StatusUnprocessableEntity, "home.html", "form", f)
		}
		return ui.render(w, http.StatusUnprocessableEntity, "home.html", homeView{Title: "Polls", Form: f})
	}
	id, err := ui.polls.CreatePoll(r.Context(), question, options)
	if err != nil {
		return err
	}
	target := pollPath.URL(hypermedia.Arg(pollID, id.String())).String()
	if htmx {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
	return nil
}
```

`readPollForm` keeps what was sent, gives each problem its message, and
skips empty options. An option given twice is a problem too:

```go
func readPollForm(form *hypermedia.Form) (f pollForm, question string, options []string) {
	f = newPollForm()
	f.Question.Value = form.Raw(questionField)
	if p := form.Problem(questionField); p != nil {
		f.Question.Problem = questionProblem(p)
	}
	given := make(map[string]bool, optionCount)
	for i, of := range optionFields {
		o := &f.Options[i]
		o.Value = form.Raw(of)
		if p := form.Problem(of); p != nil {
			o.Problem = optionProblem(p, o.Required)
			continue
		}
		text := hypermedia.FieldFrom(form, of)
		switch {
		case text == "":
		case given[text]:
			o.Problem = "This option is given already."
		default:
			given[text] = true
			options = append(options, text)
		}
	}
	return f, hypermedia.FieldFrom(form, questionField), options
}

func (f pollForm) valid() bool {
	if f.Question.Problem != "" {
		return false
	}
	for _, o := range f.Options {
		if o.Problem != "" {
			return false
		}
	}
	return true
}
```

## Each problem's message

Each rule a field breaks has a message, written for the person filling the
form in:

```go
func questionProblem(p *hypermedia.FieldError) string {
	switch p.Rule {
	case hypermedia.Repeated:
		return "Send one question."
	case hypermedia.TooLong:
		return fmt.Sprintf("Keep the question to %d characters.", p.Limit)
	default:
		return "Write the poll's question."
	}
}

func optionProblem(p *hypermedia.FieldError, required bool) string {
	switch {
	case p.Rule == hypermedia.Repeated:
		return "Send this option once."
	case p.Rule == hypermedia.TooLong:
		return fmt.Sprintf("Keep each option to %d characters.", p.Limit)
	case required:
		return fmt.Sprintf("Write this option: a poll needs at least %d.", requiredOptions)
	default:
		return "Write this option, or leave it empty."
	}
}
```

## The 422

htmx sends `HX-Request: true`, and the answer varies on it, so a cache
keeps the page and the fragment apart:

```go
func isHTMX(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "HX-Request")
	return r.Header.Get("HX-Request") == "true"
}
```

A form with problems is a 422: htmx gets the `form` fragment, which the
layout's `responseHandling` swaps in place of the form, and a browser
without htmx gets the whole page.

## The new poll

A created poll is a redirect to its page: an `HX-Redirect` and a 204 for
htmx, and a 303 for the browser.

Next: [Voting](/humus/pr-preview/pr-1562/walkthroughs/http/polls/voting/)
