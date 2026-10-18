// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"slices"

	"go.opentelemetry.io/otel/propagation"
)

// headerCarrier carries a propagator's fields in a message's headers. The
// Service extracts the publisher's trace context and baggage through it, and
// a publisher injects through it.
//
// A header's value is an AMQP field table's value, as amqp091 decodes it,
// and the carrier reads only the string ones: a header of any other type is
// not a field the carrier carries, whatever its key.
type headerCarrier map[string]any

var _ propagation.TextMapCarrier = (*headerCarrier)(nil)

// Get returns the value of the header with key, or "" when there is none or
// its value is not a string.
func (c *headerCarrier) Get(key string) string {
	v, _ := (*c)[key].(string)
	return v
}

// Set sets the header with key to value, replacing what it held. It makes
// the headers when there are none.
func (c *headerCarrier) Set(key, value string) {
	if *c == nil {
		*c = headerCarrier{}
	}
	(*c)[key] = value
}

// Keys returns the keys of the headers whose values are strings, sorted.
func (c *headerCarrier) Keys() []string {
	keys := make([]string, 0, len(*c))
	for k, v := range *c {
		if _, ok := v.(string); ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}
