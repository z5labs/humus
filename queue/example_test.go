// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package queue_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/z5labs/humus/queue"
)

type Order struct{ ID string }

// store saves orders by ID, so saving one twice leaves one order.
type store struct {
	mu     sync.Mutex
	orders map[string]Order
}

func (s *store) Save(ctx context.Context, o Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[o.ID] = o
	return nil
}

// A Processor under AtLeastOnce must be idempotent: the same message may
// be delivered again, and processing it twice must leave the same result.
func ExampleProcessorFunc() {
	s := &store{orders: map[string]Order{}}

	var p queue.Processor[Order] = queue.ProcessorFunc[Order](func(ctx context.Context, o Order) error {
		return s.Save(ctx, o) // idempotent: under queue.AtLeastOnce, o may come again
	})

	ctx := context.Background()
	_ = p.Process(ctx, Order{ID: "42"})
	_ = p.Process(ctx, Order{ID: "42"}) // delivered again

	fmt.Println(len(s.orders))
	// Output: 1
}

func ExampleCheckDelivery() {
	fmt.Println(queue.CheckDelivery(queue.AtLeastOnce))
	fmt.Println(queue.CheckDelivery("exactly-once"))
	// Output:
	// <nil>
	// "exactly-once" is not one of at-least-once or at-most-once
}
