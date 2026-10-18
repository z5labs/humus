// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package jsonscenario carries a failure scenario, a fit.Scenario, as JSON
// in one baggage member whose key the end user names.
//
// It is a ready-made codec that a service and the test harness or edge
// that drives it can share from day one. Nothing requires it: the
// fault.Settings Decoder has no default, and an end user who needs
// something else, such as a signed format or a registry of scenario IDs,
// writes a fit.Decoder of their own instead.
//
// # The member
//
// A [Codec] reads and writes one baggage member, named by the key given to
// [New]. There is no default key. Its value is the scenario as JSON,
// percent-encoded as W3C Baggage requires:
//
//	acme.fault={"rules":[{"point":"http:payments:8443","abort":true},{"point":"*","delay_ms":250}]}
//
// which travels as:
//
//	baggage: acme.fault={%22rules%22:[{%22point%22:%22http:payments:8443%22%2C%22abort%22:true}%2C{%22point%22:%22*%22%2C%22delay_ms%22:250}]}
//
// # The grammar
//
// The value is one JSON object with one field, rules, an array of rules
// tried in order, as fit.Scenario's are:
//
//	{"rules":[{"point":…,"delay_ms":…,"abort":…}]}
//
// Its fields are these:
//
//   - point is required and not empty. It is an injection point's name, a
//     prefix of one followed by "*", or a lone "*".
//   - delay_ms is a non-negative whole number of milliseconds, which any
//     language can write. It defaults to 0.
//   - abort is true or false. It defaults to false.
//   - Unknown fields are errors, so a typo such as "abrot" is reported by
//     the Injector's telemetry rather than read as a rule that spares its
//     point.
//   - Anything after the object, other than white space, is an error.
//
// An empty rules array, or none, is a scenario with no rules: it injects
// nothing, and every point the request reaches is still evaluated and
// reported, which is how the points a request passes through are
// discovered.
//
// # Limits
//
// [Codec.Encode] rejects a member that is over 2048 bytes percent-encoded,
// key included. That is a quarter of the 8192 bytes W3C Baggage guarantees
// a whole header. OpenTelemetry's baggage.Parse drops every member of a
// header that is over its limit, so an oversized scenario would wipe the
// rest of the request's baggage downstream. Encode also rejects a Delay
// that is negative or isn't a whole number of milliseconds, since the
// grammar can't carry it.
//
// [Codec.Decode] reads a member of any size that arrives. A delay_ms too
// large for a time.Duration reads as the longest one, and fault's
// MaxDelay clamps every Delay in any case.
//
// # Trust
//
// The codec has no signature and no expiry. Anyone who can set a
// request's baggage can write a scenario in this format, so trust comes
// from where a request came from: humus's servers strip scenarios from
// callers their TrustScenario setting doesn't trust, and the edge strips
// any a request arrived with before it writes its own. An end user who
// needs a scenario to prove where it came from writes a fit.Decoder that
// verifies one.
package jsonscenario
