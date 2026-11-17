// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package ui

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/hypermedia"
)

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

type homeView struct {
	Title string
	Form  pollForm
}

type pollView struct {
	Title    string
	Question string
	Options  []store.Option
	HomeURL  string
}

func isHTMX(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "HX-Request")
	return r.Header.Get("HX-Request") == "true"
}

func (ui *UI) home(w http.ResponseWriter, r *http.Request) error {
	return ui.render(w, http.StatusOK, "home.html", homeView{Title: "Polls", Form: newPollForm()})
}

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

func (ui *UI) poll(w http.ResponseWriter, r *http.Request) error {
	id, err := uuid.Parse(hypermedia.ParamFrom(r, pollID))
	if err != nil {
		return &hypermedia.RequestError{Status: http.StatusNotFound, Param: "pollId", Err: err}
	}
	p, err := ui.polls.Poll(r.Context(), id)
	if errors.Is(err, store.ErrNoSuchPoll) {
		return &hypermedia.RequestError{Status: http.StatusNotFound, Param: "pollId", Err: err}
	}
	if err != nil {
		return err
	}
	return ui.render(w, http.StatusOK, "poll.html", pollView{
		Title:    p.Question,
		Question: p.Question,
		Options:  p.Options,
		HomeURL:  homePath.URL().String(),
	})
}
