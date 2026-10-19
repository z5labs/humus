// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package projector_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/orderstatus/projector"
	"github.com/z5labs/humus/example/orderstatus/store"
)

var start = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func event(offset int64, typ string, minute int) store.Event {
	return store.Event{
		Partition: 3,
		Offset:    offset,
		OrderID:   "order-1",
		Type:      typ,
		At:        start.Add(time.Duration(minute) * time.Minute),
	}
}

func history(types ...string) []store.Event {
	events := make([]store.Event, 0, len(types))
	for i, typ := range types {
		events = append(events, event(int64(i), typ, i))
	}
	return events
}

func TestFold_AllowedTransitions(t *testing.T) {
	testCases := []struct {
		name    string
		history []string
		next    string
	}{
		{name: "placed first", next: projector.Placed},
		{name: "paid after placed", history: []string{projector.Placed}, next: projector.Paid},
		{name: "shipped after paid", history: []string{projector.Placed, projector.Paid}, next: projector.Shipped},
		{name: "delivered after shipped", history: []string{projector.Placed, projector.Paid, projector.Shipped}, next: projector.Delivered},
		{name: "cancelled after placed", history: []string{projector.Placed}, next: projector.Cancelled},
		{name: "cancelled after paid", history: []string{projector.Placed, projector.Paid}, next: projector.Cancelled},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := history(tc.history...)
			e := event(int64(len(h)), tc.next, len(h))
			previous := ""
			if len(h) > 0 {
				previous = h[len(h)-1].Type
			}

			status, changes, err := projector.Fold(h, e)

			require.NoError(t, err)
			require.Equal(t, store.Status{Status: tc.next, At: e.At}, status)
			require.Equal(t, []projector.Change{{Status: tc.next, Previous: previous, At: e.At}}, changes)
		})
	}
}

func TestFold_RefusedTransitions(t *testing.T) {
	testCases := []struct {
		name     string
		history  []string
		next     string
		previous string
	}{
		{name: "paid first", next: projector.Paid},
		{name: "shipped first", next: projector.Shipped},
		{name: "delivered first", next: projector.Delivered},
		{name: "cancelled first", next: projector.Cancelled},
		{name: "unknown type first", next: "lost"},
		{name: "placed after placed and paid", history: []string{projector.Placed, projector.Paid}, next: projector.Placed, previous: projector.Paid},
		{name: "shipped after placed", history: []string{projector.Placed}, next: projector.Shipped, previous: projector.Placed},
		{name: "delivered after placed", history: []string{projector.Placed}, next: projector.Delivered, previous: projector.Placed},
		{name: "delivered after paid", history: []string{projector.Placed, projector.Paid}, next: projector.Delivered, previous: projector.Paid},
		{name: "cancelled after shipped", history: []string{projector.Placed, projector.Paid, projector.Shipped}, next: projector.Cancelled, previous: projector.Shipped},
		{name: "cancelled after delivered", history: []string{projector.Placed, projector.Paid, projector.Shipped, projector.Delivered}, next: projector.Cancelled, previous: projector.Delivered},
		{name: "paid after cancelled", history: []string{projector.Placed, projector.Cancelled}, next: projector.Paid, previous: projector.Cancelled},
		{name: "paid after shipped", history: []string{projector.Placed, projector.Paid, projector.Shipped}, next: projector.Paid, previous: projector.Shipped},
		{name: "shipped after delivered", history: []string{projector.Placed, projector.Paid, projector.Shipped, projector.Delivered}, next: projector.Shipped, previous: projector.Delivered},
		{name: "unknown type after placed", history: []string{projector.Placed}, next: "lost", previous: projector.Placed},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := history(tc.history...)
			e := event(int64(len(h)), tc.next, len(h))

			status, changes, err := projector.Fold(h, e)

			var terr *projector.TransitionError
			require.ErrorAs(t, err, &terr)
			require.Equal(t, e, terr.Event)
			require.Equal(t, tc.previous, terr.Previous)
			if tc.previous == "" {
				require.ErrorContains(t, err, "can't come first")
			} else {
				require.ErrorContains(t, err, "can't follow "+tc.previous)
			}
			require.Zero(t, status)
			require.Nil(t, changes)
		})
	}
}

func TestFold_RepeatedStatusChangesNothing(t *testing.T) {
	h := history(projector.Placed, projector.Paid)
	e := event(2, projector.Paid, 2)

	status, changes, err := projector.Fold(h, e)

	require.NoError(t, err)
	require.Equal(t, store.Status{Status: projector.Paid, At: h[1].At}, status)
	require.Empty(t, changes)
}

func TestFold_LatePaidAfterCancelled(t *testing.T) {
	placed := event(0, projector.Placed, 0)
	cancelled := event(1, projector.Cancelled, 10)
	paid := event(2, projector.Paid, 5)

	status, changes, err := projector.Fold([]store.Event{placed, cancelled}, paid)

	require.NoError(t, err)
	require.Equal(t, store.Status{Status: projector.Cancelled, At: cancelled.At}, status)
	require.Equal(t, []projector.Change{
		{Status: projector.Paid, Previous: projector.Placed, At: paid.At},
		{Status: projector.Cancelled, Previous: projector.Paid, At: cancelled.At},
	}, changes)
}

func TestFold_ShippedWithoutPaidIsRefused(t *testing.T) {
	placed := event(0, projector.Placed, 0)
	shipped := event(1, projector.Shipped, 1)

	_, _, err := projector.Fold([]store.Event{placed}, shipped)

	var terr *projector.TransitionError
	require.ErrorAs(t, err, &terr)
	require.Equal(t, shipped, terr.Event)
	require.Equal(t, projector.Placed, terr.Previous)
	require.ErrorContains(t, err, "order order-1: shipped at partition 3, offset 1")
	require.ErrorContains(t, err, "can't follow placed")
}

func TestFold_LateEventThatBreaksALaterOneNamesTheLaterOne(t *testing.T) {
	placed := event(0, projector.Placed, 0)
	paid := event(1, projector.Paid, 10)
	cancelled := event(2, projector.Cancelled, 5)

	_, _, err := projector.Fold([]store.Event{placed, paid}, cancelled)

	var terr *projector.TransitionError
	require.ErrorAs(t, err, &terr)
	require.Equal(t, paid, terr.Event)
	require.Equal(t, projector.Cancelled, terr.Previous)
}

func TestFold_SameAtOrderedByOffset(t *testing.T) {
	placed := event(0, projector.Placed, 0)
	paid := event(5, projector.Paid, 1)
	shipped := event(4, projector.Shipped, 1)

	t.Run("the lower offset first", func(t *testing.T) {
		_, _, err := projector.Fold([]store.Event{placed, shipped}, paid)

		var terr *projector.TransitionError
		require.ErrorAs(t, err, &terr)
		require.Equal(t, shipped, terr.Event)
		require.Equal(t, projector.Placed, terr.Previous)
	})

	t.Run("whichever comes new", func(t *testing.T) {
		paid := event(4, projector.Paid, 1)
		shipped := event(5, projector.Shipped, 1)

		status, changes, err := projector.Fold([]store.Event{placed, shipped}, paid)

		require.NoError(t, err)
		require.Equal(t, store.Status{Status: projector.Shipped, At: shipped.At}, status)
		require.Equal(t, []projector.Change{
			{Status: projector.Paid, Previous: projector.Placed, At: paid.At},
			{Status: projector.Shipped, Previous: projector.Paid, At: shipped.At},
		}, changes)
	})
}

func TestFold_HistoryInAnyOrder(t *testing.T) {
	placed := event(0, projector.Placed, 0)
	paid := event(1, projector.Paid, 1)
	shipped := event(2, projector.Shipped, 2)

	status, changes, err := projector.Fold([]store.Event{paid, placed}, shipped)

	require.NoError(t, err)
	require.Equal(t, store.Status{Status: projector.Shipped, At: shipped.At}, status)
	require.Equal(t, []projector.Change{{Status: projector.Shipped, Previous: projector.Paid, At: shipped.At}}, changes)
}

func TestFold_DoesNotReorderTheHistoryItWasGiven(t *testing.T) {
	placed := event(0, projector.Placed, 0)
	paid := event(1, projector.Paid, 1)
	h := make([]store.Event, 2, 3)
	h[0], h[1] = paid, placed

	_, _, err := projector.Fold(h, event(2, projector.Shipped, 2))

	require.NoError(t, err)
	require.Equal(t, []store.Event{paid, placed}, h)
}

func TestFold_ChangesStartAtTheNewEventEvenIfTheHistoryRepeatsItsOffset(t *testing.T) {
	placed := event(0, projector.Placed, 0)
	paid := event(1, projector.Paid, 1)
	cancelled := event(2, projector.Cancelled, 2)
	again := paid
	again.At = cancelled.At.Add(time.Minute)
	again.Type = projector.Cancelled

	status, changes, err := projector.Fold([]store.Event{placed, paid, cancelled}, again)

	require.NoError(t, err)
	require.Equal(t, store.Status{Status: projector.Cancelled, At: cancelled.At}, status)
	require.Empty(t, changes)
}
