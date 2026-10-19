// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package projector folds an order's events into its status and produces its changes.
package projector

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/z5labs/humus/example/orderstatus/store"
)

const (
	Placed    = "placed"
	Paid      = "paid"
	Shipped   = "shipped"
	Delivered = "delivered"
	Cancelled = "cancelled"
)

var transitions = map[string][]string{
	Placed:    {""},
	Paid:      {Placed},
	Shipped:   {Paid},
	Delivered: {Shipped},
	Cancelled: {Placed, Paid},
}

type Change struct {
	Status   string
	Previous string
	At       time.Time
}

type TransitionError struct {
	Event    store.Event
	Previous string
}

func (e *TransitionError) Error() string {
	ev := e.Event
	if e.Previous == "" {
		return fmt.Sprintf("order %s: %s at partition %d, offset %d (%s) can't come first",
			ev.OrderID, ev.Type, ev.Partition, ev.Offset, ev.At.Format(time.RFC3339Nano))
	}
	return fmt.Sprintf("order %s: %s at partition %d, offset %d (%s) can't follow %s",
		ev.OrderID, ev.Type, ev.Partition, ev.Offset, ev.At.Format(time.RFC3339Nano), e.Previous)
}

func Fold(history []store.Event, e store.Event) (store.Status, []Change, error) {
	type entry struct {
		store.Event
		isNew bool
	}
	events := make([]entry, 0, len(history)+1)
	for _, ev := range history {
		events = append(events, entry{Event: ev})
	}
	events = append(events, entry{Event: e, isNew: true})
	slices.SortStableFunc(events, func(a, b entry) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Offset, b.Offset))
	})

	var status store.Status
	var changes []Change
	reached := false
	for _, ev := range events {
		reached = reached || ev.isNew
		if status.Status != "" && ev.Type == status.Status {
			continue
		}
		if !slices.Contains(transitions[ev.Type], status.Status) {
			return store.Status{}, nil, &TransitionError{Event: ev.Event, Previous: status.Status}
		}
		if reached {
			changes = append(changes, Change{Status: ev.Type, Previous: status.Status, At: ev.At})
		}
		status = store.Status{Status: ev.Type, At: ev.At}
	}
	return status, changes, nil
}
