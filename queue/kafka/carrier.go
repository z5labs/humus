// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/propagation"
)

// headerCarrier carries a propagator's fields in a record's headers. The
// Service extracts the producer's trace context and baggage through it, and
// a producer injects through it.
type headerCarrier []kgo.RecordHeader

var _ propagation.TextMapCarrier = (*headerCarrier)(nil)

// Get returns the value of the first header with key, or "" when there is
// none.
func (c *headerCarrier) Get(key string) string {
	for _, h := range *c {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// Set appends a header with key and value. A header already carrying key is
// kept.
func (c *headerCarrier) Set(key, value string) {
	*c = append(*c, kgo.RecordHeader{Key: key, Value: []byte(value)})
}

// Keys returns the headers' keys, in the headers' order. A key that repeats
// is returned each time.
func (c *headerCarrier) Keys() []string {
	keys := make([]string, len(*c))
	for i, h := range *c {
		keys[i] = h.Key
	}
	return keys
}
