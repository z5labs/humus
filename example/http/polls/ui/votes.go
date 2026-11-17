// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package ui

import (
	"crypto/rand"
	"errors"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/hypermedia"
)

const (
	voterCookie = "__Host-voter"
	voterMaxAge = 400 * 24 * 60 * 60
)

var (
	voterID     = regexp.MustCompile(`^[A-Z2-7]{26}$`)
	optionField = hypermedia.Field[int64]("option").Required()
)

const (
	counted        = "Your vote is counted."
	alreadyVoted   = "This browser has voted on this poll already."
	chooseAnOption = "Choose one of the poll's options."
)

type resultsView struct {
	Options []store.Option
	Note    string
	Action  string
}

func votesURL(id uuid.UUID) string {
	return votesPath.URL(hypermedia.Arg(pollID, id.String())).String()
}

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
