// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestHeaderCarrier(t *testing.T) {
	t.Parallel()

	t.Run("Get returns the value of the first header with the key", func(t *testing.T) {
		t.Parallel()
		c := headerCarrier{
			{Key: "a", Value: []byte("1")},
			{Key: "traceparent", Value: []byte("first")},
			{Key: "traceparent", Value: []byte("second")},
		}
		require.Equal(t, "first", c.Get("traceparent"))
		require.Equal(t, "1", c.Get("a"))
		require.Empty(t, c.Get("missing"))
	})

	t.Run("Get on no headers returns empty", func(t *testing.T) {
		t.Parallel()
		var c headerCarrier
		require.Empty(t, c.Get("traceparent"))
		require.Empty(t, c.Keys())
	})

	t.Run("Keys returns the headers' keys in their order", func(t *testing.T) {
		t.Parallel()
		c := headerCarrier{
			{Key: "b"},
			{Key: "a"},
			{Key: "b"},
		}
		require.Equal(t, []string{"b", "a", "b"}, c.Keys())
	})

	t.Run("Set appends a header to the record", func(t *testing.T) {
		t.Parallel()
		r := &kgo.Record{Headers: []kgo.RecordHeader{{Key: "traceparent", Value: []byte("old")}}}
		c := (*headerCarrier)(&r.Headers)
		c.Set("traceparent", "new")
		c.Set("baggage", "k=v")
		require.Equal(t, []kgo.RecordHeader{
			{Key: "traceparent", Value: []byte("old")},
			{Key: "traceparent", Value: []byte("new")},
			{Key: "baggage", Value: []byte("k=v")},
		}, r.Headers)
		require.Equal(t, "old", c.Get("traceparent"), "Get still returns the first")
	})
}
