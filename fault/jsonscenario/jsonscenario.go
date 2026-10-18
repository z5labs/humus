// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jsonscenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
)

// maxMemberBytes is the longest member Encode writes, percent-encoded and
// key included: a quarter of W3C Baggage's 8192-byte guarantee.
const maxMemberBytes = 2048

// Codec carries a fit.Scenario as JSON in one baggage member:
//
//	acme.fault={"rules":[{"point":"http:payments:8443","abort":true},{"point":"*","delay_ms":250}]}
//
// percent-encoded, as W3C Baggage requires. It is a fit.Decoder and a
// fit.Encoder, and is safe for concurrent use. The zero Codec names no
// member: it decodes and strips nothing, and encodes nothing. Build one
// with New.
type Codec struct {
	key string
}

var (
	_ fit.Decoder = Codec{}
	_ fit.Encoder = Codec{}
)

// New returns the Codec of the member named key, which must be a valid
// W3C baggage key. There is no default key.
func New(key string) (Codec, error) {
	if !validKey(key) {
		return Codec{}, fmt.Errorf("jsonscenario: %q is not a valid W3C baggage key", key)
	}
	return Codec{key: key}, nil
}

// validKey reports whether key is a W3C baggage key: an RFC 9110 token,
// one or more of the visible ASCII characters other than the delimiters.
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// scenarioJSON is the member's value.
type scenarioJSON struct {
	Rules []ruleJSON `json:"rules"`
}

// ruleJSON is one rule of the member's value.
type ruleJSON struct {
	Point   string `json:"point"`
	DelayMS millis `json:"delay_ms,omitempty"`
	Abort   bool   `json:"abort,omitempty"`
}

// millis is delay_ms, a non-negative whole number of milliseconds.
type millis int64

// errDelayMS is the error for a delay_ms that isn't a non-negative whole
// number.
var errDelayMS = errors.New("delay_ms must be a non-negative whole number of milliseconds")

// UnmarshalJSON reads a JSON number made of digits alone. A number too
// large for a time.Duration reads as the longest one.
func (m *millis) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return errDelayMS
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return fmt.Errorf("%w, not %s", errDelayMS, b)
		}
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || n > math.MaxInt64/int64(time.Millisecond) {
		*m = math.MaxInt64 / millis(time.Millisecond)
		return nil
	}
	*m = millis(n)
	return nil
}

// Decode reads the scenario in the Codec's member of b. It reports false
// when b has no such member, and an error when the member breaks the
// grammar. It reads a member of any size.
func (c Codec) Decode(b baggage.Baggage) (fit.Scenario, bool, error) {
	if c.key == "" {
		return fit.Scenario{}, false, nil
	}
	m := b.Member(c.key)
	if m.Key() == "" {
		return fit.Scenario{}, false, nil
	}
	s, err := parse(m.Value())
	if err != nil {
		return fit.Scenario{}, true, fmt.Errorf("jsonscenario: baggage member %s: %w", c.key, err)
	}
	return s, true, nil
}

// parse reads a scenario from the member's value.
func parse(value string) (fit.Scenario, error) {
	dec := json.NewDecoder(strings.NewReader(value))
	dec.DisallowUnknownFields()

	var v *scenarioJSON
	if err := dec.Decode(&v); err != nil {
		return fit.Scenario{}, err
	}
	if v == nil {
		return fit.Scenario{}, errors.New(`want a JSON object, such as {"rules":[]}, not null`)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fit.Scenario{}, errors.New("trailing data after the JSON object")
	}

	s := fit.Scenario{Rules: make([]fit.Rule, 0, len(v.Rules))}
	for i, r := range v.Rules {
		if r.Point == "" {
			return fit.Scenario{}, fmt.Errorf("rules[%d]: point is required", i)
		}
		s.Rules = append(s.Rules, fit.Rule{
			Point: r.Point,
			Fault: fit.Fault{
				Delay: time.Duration(r.DelayMS) * time.Millisecond,
				Abort: r.Abort,
			},
		})
	}
	return s, nil
}

// Strip returns b without the Codec's member, and leaves every other
// member alone.
func (c Codec) Strip(b baggage.Baggage) baggage.Baggage {
	if c.key == "" {
		return b
	}
	return b.DeleteMember(c.key)
}

// Encode returns b with s written into the Codec's member, replacing any
// scenario the member held. It rejects a rule with an empty point, a Delay
// that is negative or isn't a whole number of milliseconds, and a member
// over 2048 bytes percent-encoded, key included. On error it returns b
// unchanged.
func (c Codec) Encode(b baggage.Baggage, s fit.Scenario) (baggage.Baggage, error) {
	if c.key == "" {
		return b, errors.New("jsonscenario: the zero Codec names no member: build one with New")
	}

	v := scenarioJSON{Rules: make([]ruleJSON, 0, len(s.Rules))}
	for i, r := range s.Rules {
		switch {
		case r.Point == "":
			return b, fmt.Errorf("jsonscenario: rules[%d]: point is required", i)
		case !utf8.ValidString(r.Point):
			return b, fmt.Errorf("jsonscenario: rules[%d]: point %q is not valid UTF-8", i, r.Point)
		case r.Fault.Delay < 0:
			return b, fmt.Errorf("jsonscenario: rules[%d] (%s): delay %v is negative", i, r.Point, r.Fault.Delay)
		case r.Fault.Delay%time.Millisecond != 0:
			return b, fmt.Errorf("jsonscenario: rules[%d] (%s): delay %v is not a whole number of milliseconds", i, r.Point, r.Fault.Delay)
		}
		v.Rules = append(v.Rules, ruleJSON{
			Point:   r.Point,
			DelayMS: millis(r.Fault.Delay / time.Millisecond),
			Abort:   r.Fault.Abort,
		})
	}

	value, err := json.Marshal(v)
	if err != nil {
		return b, fmt.Errorf("jsonscenario: %w", err)
	}
	m, err := baggage.NewMemberRaw(c.key, string(value))
	if err != nil {
		return b, fmt.Errorf("jsonscenario: %w", err)
	}
	if n := len(m.String()); n > maxMemberBytes {
		return b, fmt.Errorf("jsonscenario: baggage member %s is %d bytes percent-encoded, over the limit of %d", c.key, n, maxMemberBytes)
	}
	return b.SetMember(m)
}
