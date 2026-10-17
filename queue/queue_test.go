// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package queue_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/queue"
)

func TestDelivery_values(t *testing.T) {
	t.Parallel()

	// A Delivery sourced from a variable reads as the constant's value.
	require.Equal(t, queue.Delivery("at-least-once"), queue.AtLeastOnce)
	require.Equal(t, queue.Delivery("at-most-once"), queue.AtMostOnce)
}

func TestCheckDelivery(t *testing.T) {
	t.Parallel()

	t.Run("accepts", func(t *testing.T) {
		t.Parallel()

		for _, d := range []queue.Delivery{queue.AtLeastOnce, queue.AtMostOnce} {
			require.NoError(t, queue.CheckDelivery(d), "CheckDelivery(%q)", d)
		}
	})

	t.Run("rejects", func(t *testing.T) {
		t.Parallel()

		for _, d := range []queue.Delivery{"", "exactly-once", "AT-LEAST-ONCE"} {
			t.Run(string(d), func(t *testing.T) {
				t.Parallel()

				err := queue.CheckDelivery(d)
				require.Error(t, err)

				msg := err.Error()
				require.Contains(t, msg, `"`+string(d)+`"`, "the error quotes the value it rejected")
				require.Contains(t, msg, string(queue.AtLeastOnce))
				require.Contains(t, msg, string(queue.AtMostOnce))
				require.NotContains(t, strings.ToLower(msg), "delivery", "the error names no field")
			})
		}
	})
}

func TestProcessorFunc_Process(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}
	type order struct{ ID string }

	t.Run("passes its context and message through", func(t *testing.T) {
		t.Parallel()

		ctx := context.WithValue(context.Background(), ctxKey{}, "value")
		var (
			gotCtx context.Context
			gotMsg order
			calls  int
		)
		var p queue.Processor[order] = queue.ProcessorFunc[order](func(ctx context.Context, o order) error {
			gotCtx, gotMsg = ctx, o
			calls++
			return nil
		})

		require.NoError(t, p.Process(ctx, order{ID: "42"}))
		require.Equal(t, 1, calls)
		require.Equal(t, order{ID: "42"}, gotMsg)
		require.Equal(t, "value", gotCtx.Value(ctxKey{}))
	})

	t.Run("returns the function's error", func(t *testing.T) {
		t.Parallel()

		want := errors.New("store unavailable")
		p := queue.ProcessorFunc[order](func(context.Context, order) error {
			return want
		})

		require.ErrorIs(t, p.Process(context.Background(), order{ID: "42"}), want)
	})
}
