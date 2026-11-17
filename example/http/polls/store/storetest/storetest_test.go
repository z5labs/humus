// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package storetest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/example/http/polls/store/storetest"
)

func create(t *testing.T, s *storetest.Store, question string, options ...string) store.Poll {
	t.Helper()
	id, err := s.CreatePoll(t.Context(), question, options)
	require.NoError(t, err)
	poll, err := s.Poll(t.Context(), id)
	require.NoError(t, err)
	return poll
}

func counts(t *testing.T, s *storetest.Store, id uuid.UUID) []int64 {
	t.Helper()
	poll, err := s.Poll(t.Context(), id)
	require.NoError(t, err)
	counts := make([]int64, len(poll.Options))
	for i, o := range poll.Options {
		counts[i] = o.Votes
	}
	return counts
}

func TestStore_reads_back_a_poll_it_creates_with_no_votes(t *testing.T) {
	s := storetest.New()
	ctx := t.Context()

	id, err := s.CreatePoll(ctx, "Tabs or spaces?", []string{"Tabs", "Spaces", "Both"})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, id)

	poll, err := s.Poll(ctx, id)
	require.NoError(t, err)
	require.Equal(t, id, poll.ID)
	require.Equal(t, "Tabs or spaces?", poll.Question)
	require.Len(t, poll.Options, 3)
	for i, text := range []string{"Tabs", "Spaces", "Both"} {
		require.Equal(t, text, poll.Options[i].Text, "the options keep their order")
		require.NotZero(t, poll.Options[i].ID)
		require.Zero(t, poll.Options[i].Votes)
	}

	_, err = s.Poll(ctx, uuid.New())
	require.ErrorIs(t, err, store.ErrNoSuchPoll)
	require.NotErrorIs(t, err, store.ErrUnavailable)
}

func TestStore_refuses_a_poll_that_breaks_its_rules_and_creates_nothing(t *testing.T) {
	testCases := []struct {
		name     string
		question string
		options  []string
	}{
		{name: "no question", question: "", options: []string{"Tabs", "Spaces"}},
		{name: "no options", question: "Tabs or spaces?"},
		{name: "one option", question: "Tabs or spaces?", options: []string{"Tabs"}},
		{name: "an option given twice", question: "Tabs or spaces?", options: []string{"Tabs", "Spaces", "Tabs"}},
		{name: "an option with no text", question: "Tabs or spaces?", options: []string{"Tabs", ""}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s := storetest.New()
			id, err := s.CreatePoll(t.Context(), tc.question, tc.options)
			require.ErrorIs(t, err, store.ErrInvalidPoll)
			require.NotErrorIs(t, err, store.ErrUnavailable)
			require.Equal(t, uuid.Nil, id)

			poll := create(t, s, "Vim or Emacs?", "Vim", "Emacs")
			require.Equal(t, int64(1), poll.Options[0].ID, "the refused poll took no option IDs")
		})
	}
}

func TestStore_counts_a_vote_once_and_refuses_a_second_from_the_same_voter(t *testing.T) {
	s := storetest.New()
	ctx := t.Context()
	poll := create(t, s, "Tabs or spaces?", "Tabs", "Spaces")
	tabs, spaces := poll.Options[0].ID, poll.Options[1].ID

	require.NoError(t, s.Vote(ctx, poll.ID, tabs, "alice"))
	require.Equal(t, []int64{1, 0}, counts(t, s, poll.ID))

	for _, option := range []int64{tabs, spaces} {
		err := s.Vote(ctx, poll.ID, option, "alice")
		require.ErrorIs(t, err, store.ErrAlreadyVoted)
		require.NotErrorIs(t, err, store.ErrUnavailable)
	}
	require.Equal(t, []int64{1, 0}, counts(t, s, poll.ID))

	require.NoError(t, s.Vote(ctx, poll.ID, spaces, "bob"))
	require.Equal(t, []int64{1, 1}, counts(t, s, poll.ID))

	other := create(t, s, "Vim or Emacs?", "Vim", "Emacs")
	require.NoError(t, s.Vote(ctx, other.ID, other.Options[1].ID, "alice"), "a voter votes once on each poll")
	require.Equal(t, []int64{0, 1}, counts(t, s, other.ID))
}

func TestStore_counts_concurrent_votes_from_one_voter_once(t *testing.T) {
	s := storetest.New()
	poll := create(t, s, "Tabs or spaces?", "Tabs", "Spaces")

	const sends = 20
	errs := make(chan error, sends)
	for i := range sends {
		option := poll.Options[i%2].ID
		go func() {
			errs <- s.Vote(context.Background(), poll.ID, option, "alice")
		}()
	}

	counted := 0
	for range sends {
		err := <-errs
		if err == nil {
			counted++
			continue
		}
		require.ErrorIs(t, err, store.ErrAlreadyVoted)
	}
	require.Equal(t, 1, counted)
	c := counts(t, s, poll.ID)
	require.EqualValues(t, 1, c[0]+c[1])
}

func TestStore_counts_every_one_of_concurrent_votes_from_many_voters(t *testing.T) {
	s := storetest.New()
	poll := create(t, s, "Tabs or spaces?", "Tabs", "Spaces", "Both")

	const voters = 30
	errs := make(chan error, voters)
	for i := range voters {
		option := poll.Options[i%3].ID
		voter := fmt.Sprintf("voter-%d", i)
		go func() {
			errs <- s.Vote(context.Background(), poll.ID, option, voter)
		}()
	}
	for range voters {
		require.NoError(t, <-errs)
	}
	require.Equal(t, []int64{10, 10, 10}, counts(t, s, poll.ID))
}

func TestStore_refuses_a_vote_for_another_polls_option_and_counts_nothing(t *testing.T) {
	s := storetest.New()
	ctx := t.Context()
	poll := create(t, s, "Tabs or spaces?", "Tabs", "Spaces")
	other := create(t, s, "Vim or Emacs?", "Vim", "Emacs")

	err := s.Vote(ctx, poll.ID, other.Options[0].ID, "alice")
	require.ErrorIs(t, err, store.ErrNotAnOption)
	require.NotErrorIs(t, err, store.ErrNoSuchPoll)
	require.NotErrorIs(t, err, store.ErrUnavailable)

	err = s.Vote(ctx, poll.ID, 1<<40, "alice")
	require.ErrorIs(t, err, store.ErrNotAnOption)

	err = s.Vote(ctx, uuid.New(), poll.Options[0].ID, "alice")
	require.ErrorIs(t, err, store.ErrNoSuchPoll)
	require.NotErrorIs(t, err, store.ErrNotAnOption)

	require.Equal(t, []int64{0, 0}, counts(t, s, poll.ID))
	require.Equal(t, []int64{0, 0}, counts(t, s, other.ID))

	require.NoError(t, s.Vote(ctx, poll.ID, poll.Options[0].ID, "alice"), "a refused vote isn't a vote")
}

func TestStore_a_second_vote_is_refused_before_its_option_is_checked(t *testing.T) {
	s := storetest.New()
	ctx := t.Context()
	poll := create(t, s, "Tabs or spaces?", "Tabs", "Spaces")
	other := create(t, s, "Vim or Emacs?", "Vim", "Emacs")

	require.NoError(t, s.Vote(ctx, poll.ID, poll.Options[0].ID, "alice"))
	err := s.Vote(ctx, poll.ID, other.Options[0].ID, "alice")
	require.ErrorIs(t, err, store.ErrAlreadyVoted, "as PostgreSQL checks the primary key before the foreign key")
	require.NotErrorIs(t, err, store.ErrNotAnOption)
}

func TestStore_a_poll_read_back_is_a_copy(t *testing.T) {
	s := storetest.New()
	poll := create(t, s, "Tabs or spaces?", "Tabs", "Spaces")
	poll.Options[0].Votes = 99
	poll.Options[0].Text = "Neither"

	again, err := s.Poll(t.Context(), poll.ID)
	require.NoError(t, err)
	require.Equal(t, "Tabs", again.Options[0].Text)
	require.Zero(t, again.Options[0].Votes)
}
