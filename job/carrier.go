// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package job

import (
	"strings"

	"github.com/z5labs/bedrock"
)

// envCarrier is a propagation.TextMapCarrier over a Process's environment.
// It reads each key under its name as an environment-variable carrier, and
// writes nothing. The name is the one OpenTelemetry's environment-variable
// carriers give the key, https://opentelemetry.io/docs/specs/otel/context/env-carriers/:
// traceparent is read as TRACEPARENT and x-b3-traceid as X_B3_TRACEID.
type envCarrier struct{ env bedrock.Environment }

// Get returns the value of key's variable, or "" when it is not set. It
// never reads a variable under the key's own spelling. A nil environment
// has nothing set.
func (c envCarrier) Get(key string) string {
	if c.env == nil {
		return ""
	}
	v, err := c.env.Value(envName(key))
	if err != nil {
		return ""
	}
	return v
}

// Set does nothing: Extract never calls it, and the environment is read-only.
func (envCarrier) Set(key, value string) {}

// Keys returns nil, since a bedrock.Environment can't list its variables.
// The specification lets it return only names that are already normalized,
// and W3C trace context and baggage never call it.
func (envCarrier) Keys() []string { return nil }

// envName returns the name of key's variable, as the environment-variable
// carriers specification normalizes it: ASCII letters uppercased, every
// character but an ASCII letter, digit or '_' replaced with '_', "_" for an
// empty key, and a '_' before a leading digit. A name that already matches
// ^[A-Z_][A-Z0-9_]*$ is unchanged.
func envName(key string) string {
	if key == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(key) + 1)
	if key[0] >= '0' && key[0] <= '9' {
		b.WriteByte('_')
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
