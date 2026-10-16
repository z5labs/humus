// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package telemetry sets up OpenTelemetry for a humus service.
//
// An App's OpenTelemetry providers are one [SDK] value, handed to its
// Services, so nothing reads or sets OpenTelemetry's globals. [New] builds
// one from a plain [Settings] value, which the end user's [Builder] sources
// however it likes, starting from [DefaultSettings]:
//
//	func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
//		s := telemetry.DefaultSettings()
//		s.ServiceName = "carts"
//		return telemetry.New(ctx, s, dial, p.Stdout)
//	}
//
// The package reads no environment variables. With the defaults, log
// records are written to stdout in the format below, spans are not exported
// but still get real IDs, so the logs carry them, and metrics are a no-op.
//
// # Console log format
//
// When a service's logs are written to the console, each log record is
// written as one line of JSON, terminated by a newline:
//
//	{"timestamp":"2026-10-16T06:56:32.447829601Z","severity_text":"INFO","severity_number":9,"body":"cart created","trace_id":"e2f668b6c06f2de82e61e2517c2d7a07","span_id":"ff25eb972a5557d5","trace_flags":"01","scope":"carts","attributes":{"cart.id":"c-123"}}
//
// This format is part of humus's API. Changing it, by renaming, reordering,
// adding or removing a key or by encoding a value differently, is a breaking
// change. It does not depend on the format of any OpenTelemetry exporter, so
// upgrading OpenTelemetry does not change it.
//
// The keys of a line are always written in the order below, and each one
// only when the record has what it holds:
//
//   - timestamp: the record's timestamp or, when that is unset, its observed
//     timestamp, in UTC, as RFC 3339 with exactly nine fractional digits:
//     2026-10-16T06:56:32.447829601Z. Always present.
//   - severity_text: the record's severity text or, when that is empty, the
//     short name of its severity number: TRACE, TRACE2, ..., INFO, INFO2,
//     ..., FATAL4. Present when the record's severity number is set, that is
//     when it is one of OpenTelemetry's severities 1 to 24.
//   - severity_number: the record's OpenTelemetry severity number, 1 to 24.
//     Present under the same condition as severity_text.
//   - event_name: the record's event name. Present when it is not empty.
//   - body: the record's body, encoded as described below. Present when the
//     body is set.
//   - trace_id, span_id and trace_flags: the trace context of the span the
//     record was logged in, as lowercase hex: 32, 16 and 2 digits. Present
//     only when the record has a valid span context, one whose trace ID and
//     span ID are both non-zero; a record logged outside a span has none of
//     the three, rather than zeros. These keys follow OpenTelemetry's stable
//     specification for trace context in non-OTLP log formats,
//     https://opentelemetry.io/docs/specs/otel/compatibility/logging_trace_context/.
//   - scope: the name of the record's instrumentation scope, the name its
//     logger was created with. Always present, and empty when that name is.
//   - attributes: the record's attributes, as an object whose keys are the
//     attribute keys, in the record's order. Present when the record has at
//     least one attribute. An attribute whose value is empty is written with
//     the value null. A key the record holds more than once, which happens
//     only when its logger provider allows duplicate keys, is written once
//     for each value it holds.
//
// The record's resource and its count of dropped attributes are never
// written.
//
// A body and an attribute value are encoded the same way, according to
// their type:
//
//   - a string as a JSON string;
//   - a bool as true or false;
//   - an integer as a JSON number;
//   - a finite float as a JSON number, and NaN, positive infinity and
//     negative infinity as the strings "NaN", "+Inf" and "-Inf", so that every
//     line is valid JSON;
//   - bytes as a string holding their standard, padded base64 encoding;
//   - a slice, the typed bool, integer, float and string slices included, as
//     an array of its elements, each encoded by these rules;
//   - a map as an object, its keys sorted by their bytes and its values
//     encoded by these rules; a key the map holds more than once is written
//     once for each value it holds;
//   - an empty value as null, as an attribute's value or inside a slice or
//     map. An empty body is not written at all.
//
// Strings are escaped as JSON requires and no further: the characters <, >
// and & are written as they are. Invalid UTF-8 is replaced by U+FFFD.
package telemetry
