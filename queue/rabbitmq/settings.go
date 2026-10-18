// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus/queue"
)

// Connection is how a Service or a Publisher reaches the broker. Every field
// is a plain value, and a field's zero value means zero, never "use the
// default": start from the Connection in DefaultSettings and change the
// fields you source. Addr and SASL have no default, so they are always among
// them.
type Connection struct {
	// Addr is the broker's address, as host:port. It is required.
	//
	// Default: none (""). Valid: host:port, with a host and a port from 1
	// to 65535. Check: CheckAddr.
	Addr string

	// Vhost is the virtual host the connection opens.
	//
	// Default: "/". Valid: not empty. Check: CheckVhost.
	Vhost string

	// TLS is the config the broker is dialled with. nil means plain text.
	// An empty ServerName defaults to Addr's host. A client certificate in
	// Certificates makes it mTLS, and authenticates with SASL set to
	// amqp091's ExternalAuth.
	//
	// Default: nil, which means plain text. Valid: any. Check: none.
	TLS *tls.Config

	// SASL is how the connection authenticates: amqp091's PlainAuth,
	// AMQPlainAuth, or ExternalAuth for a client certificate in TLS. AMQP
	// always authenticates, so it is required.
	//
	// Default: none (nil). Valid: not nil. Check: CheckSASL.
	SASL amqp.Authentication

	// Heartbeat is the heartbeat interval the client proposes. The
	// broker's, if shorter, wins. A connection silent for 1.5 intervals is
	// dead. Heartbeats can't be turned off.
	//
	// Default: 10s. Valid: a whole number of seconds, from 1s to 65535s.
	// Check: CheckHeartbeat.
	Heartbeat time.Duration

	// ConnectTimeout bounds each dial, its TLS handshake and its AMQP
	// handshake.
	//
	// Default: 30s. Valid: positive. Check: CheckConnectTimeout.
	ConnectTimeout time.Duration
}

// Settings configures a Service. Every field is a plain value, and a field's
// zero value means zero, never "use the default": start from DefaultSettings
// and change the fields you source. Connection.Addr, Connection.SASL and
// Queue have no default, so they are always among them.
type Settings struct {
	// Connection is how the Service reaches the broker.
	Connection Connection

	// Queue is the queue the Service consumes. It must already exist: the
	// Service declares nothing. It is required.
	//
	// Default: none (""). Valid: 1 to 255 bytes. Check: CheckQueue.
	Queue string

	// Delivery is when a delivery is acked, relative to processing it.
	//
	// Default: queue.AtLeastOnce. Valid: queue.AtLeastOnce or
	// queue.AtMostOnce. Check: queue.CheckDelivery.
	Delivery queue.Delivery

	// Prefetch bounds the deliveries the broker sends and the Service holds
	// unacked. Under queue.AtMostOnce it also bounds the batch acked at
	// once.
	//
	// Default: 10. Valid: from 1 to 65535. Check: CheckPrefetch.
	Prefetch int

	// Concurrency is how many deliveries are processed at once. At 1 they
	// are processed one at a time, in the order the broker delivered them.
	//
	// Default: 1. Valid: from 1 to Prefetch. Check: CheckConcurrency, and
	// NewService for the bound of Prefetch.
	Concurrency int

	// TrustScenario reports whether the failure scenario a delivery
	// carries reaches the Processor. A delivery it doesn't trust has the
	// scenario stripped from its context first. It is asked only about a
	// delivery that carries a scenario, and only when the App has failure
	// injection. It is set in code, not sourced.
	//
	// Default: nil, which trusts no publisher. Valid: any. Check: none.
	TrustScenario func(m Message) bool
}

// DefaultSettings returns the Service's defaults. Connection.Addr,
// Connection.SASL and Queue have none, so they are "", nil and "", which
// CheckAddr, CheckSASL and CheckQueue reject: the end user always sources
// all three.
func DefaultSettings() Settings {
	return Settings{
		Connection: Connection{
			Addr:           "",
			Vhost:          "/",
			TLS:            nil,
			SASL:           nil,
			Heartbeat:      10 * time.Second,
			ConnectTimeout: 30 * time.Second,
		},
		Queue:         "",
		Delivery:      queue.AtLeastOnce,
		Prefetch:      10,
		Concurrency:   1,
		TrustScenario: nil,
	}
}

// PublisherSettings configures a Publisher. Every field is a plain value,
// and a field's zero value means zero, never "use the default": start from
// DefaultPublisherSettings and change the fields you source.
// Connection.Addr and Connection.SASL have no default, so they are always
// among them.
type PublisherSettings struct {
	// Connection is how the Publisher reaches the broker, on a connection
	// of its own.
	Connection Connection

	// Channels bounds the publishes in flight at once.
	//
	// Default: 8. Valid: 1 to 65535. Check: CheckChannels.
	Channels int

	// Timeout bounds a whole Publish: every attempt, the waits between
	// them, and each confirm. A deadline the caller's context has already,
	// if it is earlier, wins.
	//
	// Default: 10s. Valid: positive. Check: CheckTimeout.
	Timeout time.Duration

	// MaxAttempts bounds the attempts of one Publish, the first included.
	// 1 turns retries off.
	//
	// Default: 3. Valid: 1 or more. Check: CheckMaxAttempts.
	MaxAttempts int

	// BackoffBase and BackoffMax are retry.Exponential's arguments: the wait
	// after attempt n is drawn from [0, min(BackoffMax, BackoffBase·2ⁿ⁻¹)).
	//
	// Default: 100ms and 5s. Valid: positive, and BackoffMax at least
	// BackoffBase, which NewPublisher checks. Check: CheckBackoffBase,
	// CheckBackoffMax.
	BackoffBase time.Duration
	BackoffMax  time.Duration

	// RetryBudget limits the retries of every Publish of the Publisher
	// together, as retry.Budget describes.
	//
	// Default: {MaxTokens: 10, TokenRatio: 0.1}. Valid: MaxTokens from 3 to
	// retry.TokenLimit; TokenRatio from 0.001 to retry.TokenLimit. Check:
	// CheckRetryBudgetMaxTokens, CheckRetryBudgetTokenRatio.
	RetryBudget retry.BudgetConfig

	// Breaker configures the circuit breaker in front of the broker, one
	// for the Publisher, which every attempt of every Publish goes
	// through.
	//
	// Default: circuit.Config{Window: 20, Probes: 1, OpenFor: 10 *
	// time.Second, ProbeTimeout: 10 * time.Second}. Valid: Window and
	// Probes from 1 to circuit.MaxCount, OpenFor and ProbeTimeout positive.
	// Checks: CheckBreakerWindow, CheckBreakerProbes, CheckBreakerOpenFor,
	// CheckBreakerProbeTimeout.
	Breaker circuit.Config

	// BreakerPolicy decides when the breaker opens and closes. It is set in
	// code, not sourced. A policy whose Trip always returns false turns the
	// breaker off.
	//
	// Default: open after 5 failures in a row, or once half of the last 20
	// or more results failed; while half-open, reopen on a failed probe and
	// close after 3 successful ones. Valid: Trip and Probe set, which
	// NewPublisher checks. Check: none.
	BreakerPolicy circuit.Policy

	// ForwardScenario sends the failure scenario a call carries on in the
	// headers of the messages it publishes, for consumers that take part
	// in failure injection with the same codec. When it is false, the
	// scenario is stripped from each message's baggage header, and the
	// rest of the call's baggage still goes. It is set in code, not
	// sourced.
	//
	// Default: false. Valid: either. Check: none.
	ForwardScenario bool
}

// DefaultPublisherSettings returns the Publisher's defaults: the Connection
// of DefaultSettings, 8 Channels, a Timeout of 10s, 3 MaxAttempts, a
// BackoffBase of 100ms and a BackoffMax of 5s, and a RetryBudget of 10
// tokens, each confirmed attempt adding back 0.1, and a Breaker and
// BreakerPolicy as their docs describe. Connection.Addr and Connection.SASL
// have none, so they are "" and nil, which CheckAddr and CheckSASL reject:
// the end user always sources both.
func DefaultPublisherSettings() PublisherSettings {
	return PublisherSettings{
		Connection:  DefaultSettings().Connection,
		Channels:    8,
		Timeout:     10 * time.Second,
		MaxAttempts: 3,
		BackoffBase: 100 * time.Millisecond,
		BackoffMax:  5 * time.Second,
		RetryBudget: retry.BudgetConfig{MaxTokens: 10, TokenRatio: 0.1},
		Breaker: circuit.Config{
			Window:       20,
			Probes:       1,
			OpenFor:      10 * time.Second,
			ProbeTimeout: 10 * time.Second,
		},
		BreakerPolicy: circuit.Policy{
			Trip:  defaultTrip,
			Probe: defaultProbe,
		},
		ForwardScenario: false,
	}
}

// defaultTrip opens the breaker after 5 failures in a row, or once half of
// the last 20 or more results failed.
func defaultTrip(c circuit.Counts) bool {
	n := c.Successes + c.Failures
	return c.ConsecutiveFailures >= 5 || (n >= 20 && 2*c.Failures >= n)
}

// defaultProbe reopens the breaker on a failed probe, closes it after 3
// successful ones, and keeps probing otherwise, including after a probe
// times out.
func defaultProbe(c circuit.Counts) circuit.State {
	switch {
	case c.Failures > 0:
		return circuit.Open
	case c.ConsecutiveSuccesses >= 3:
		return circuit.Closed
	default:
		return circuit.HalfOpen
	}
}

// maxQueueName is the longest queue name, in bytes, that AMQP's short
// strings carry.
const maxQueueName = 255

// maxHeartbeat is the longest heartbeat AMQP's 16-bit field carries.
const maxHeartbeat = 65535 * time.Second

// The causes the checks report. None of them names a field: whoever calls a
// check names the field, or the variable that sourced it.
var (
	errURI           = errors.New("is a URI, not host:port")
	errNotHostPort   = errors.New("is not host:port")
	errNoHost        = errors.New("has no host")
	errNoPort        = errors.New("has no port from 1 to 65535")
	errEmpty         = errors.New("must not be empty")
	errNotSet        = errors.New("must be set")
	errNotPositive   = errors.New("must be positive")
	errNotWholeSecs  = errors.New("must be a whole number of seconds")
	errHeartbeatSpan = errors.New("must be from 1s to 65535s")
	errTooLong       = fmt.Errorf("must be at most %d bytes", maxQueueName)
	errNotUint16     = errors.New("must be from 1 to 65535")
	errBelowOne      = errors.New("must be at least 1")
	errTokens        = fmt.Errorf("must be from 3 to %d", retry.TokenLimit)
	errTokenRatio    = fmt.Errorf("must be from 0.001 to %d", retry.TokenLimit)
	errOutOfCount    = fmt.Errorf("must be from 1 to %d", circuit.MaxCount)
)

// CheckAddr rejects anything but host:port with a host and a port from 1 to
// 65535. Its error never quotes the value: the likeliest wrong value is an
// AMQP URI, whose login would otherwise reach the logs.
func CheckAddr(addr string) error {
	if strings.Contains(addr, "://") {
		return errURI
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errNotHostPort
	}
	if host == "" {
		return errNoHost
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return errNoPort
	}
	return nil
}

// CheckVhost rejects "".
func CheckVhost(vhost string) error {
	if vhost == "" {
		return errEmpty
	}
	return nil
}

// CheckSASL rejects nil, including a nil pointer of one of amqp091's
// Authentication types, such as a (*amqp.PlainAuth)(nil).
func CheckSASL(a amqp.Authentication) error {
	if a == nil {
		return errNotSet
	}
	if v := reflect.ValueOf(a); v.Kind() == reflect.Pointer && v.IsNil() {
		return errNotSet
	}
	return nil
}

// CheckHeartbeat rejects a duration that isn't a whole number of seconds,
// and one below 1s or above 65535s.
func CheckHeartbeat(d time.Duration) error {
	if d%time.Second != 0 {
		return errNotWholeSecs
	}
	if d < time.Second || d > maxHeartbeat {
		return errHeartbeatSpan
	}
	return nil
}

// CheckConnectTimeout rejects a timeout that is not positive.
func CheckConnectTimeout(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// CheckQueue rejects "" and a name longer than 255 bytes.
func CheckQueue(name string) error {
	switch {
	case name == "":
		return errEmpty
	case len(name) > maxQueueName:
		return errTooLong
	}
	return nil
}

// CheckPrefetch rejects a count outside 1 to 65535.
func CheckPrefetch(n int) error {
	if n < 1 || n > 65535 {
		return errNotUint16
	}
	return nil
}

// CheckConcurrency rejects a count below 1. That it is at most Prefetch is
// checked by NewService, which has both.
func CheckConcurrency(n int) error {
	if n < 1 {
		return errBelowOne
	}
	return nil
}

// CheckChannels rejects a count outside 1 to 65535.
func CheckChannels(n int) error {
	if n < 1 || n > 65535 {
		return errNotUint16
	}
	return nil
}

// CheckTimeout rejects a timeout that is not positive.
func CheckTimeout(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// CheckMaxAttempts rejects fewer than 1 attempt: every Publish makes at
// least its first. 1 turns retries off.
func CheckMaxAttempts(n int) error {
	if n < 1 {
		return errBelowOne
	}
	return nil
}

// CheckBackoffBase rejects a duration that is not positive, as
// retry.Exponential does.
func CheckBackoffBase(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// CheckBackoffMax rejects a duration that is not positive. NewPublisher
// also rejects a BackoffMax below BackoffBase, which takes both fields to
// tell.
func CheckBackoffMax(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// CheckRetryBudgetMaxTokens accepts 3 to retry.TokenLimit tokens, as
// retry.NewBudget does: a budget allows no retry while it holds half of its
// tokens or fewer, so one of 1 or 2 tokens would never allow one.
func CheckRetryBudgetMaxTokens(n int) error {
	if n < 3 || n > retry.TokenLimit {
		return errTokens
	}
	return nil
}

// CheckRetryBudgetTokenRatio accepts 0.001 to retry.TokenLimit tokens, as
// retry.NewBudget does. NaN is rejected.
func CheckRetryBudgetTokenRatio(r float64) error {
	if !(r >= 0.001 && r <= retry.TokenLimit) {
		return errTokenRatio
	}
	return nil
}

// CheckBreakerWindow rejects a window that is not from 1 to
// circuit.MaxCount.
func CheckBreakerWindow(n int) error {
	return checkCount(n)
}

// CheckBreakerProbes rejects a number of probes that is not from 1 to
// circuit.MaxCount.
func CheckBreakerProbes(n int) error {
	return checkCount(n)
}

// CheckBreakerOpenFor rejects a period that is not positive.
func CheckBreakerOpenFor(d time.Duration) error {
	return checkPositive(d)
}

// CheckBreakerProbeTimeout rejects a timeout that is not positive.
func CheckBreakerProbeTimeout(d time.Duration) error {
	return checkPositive(d)
}

func checkCount(n int) error {
	if n < 1 || n > circuit.MaxCount {
		return errOutOfCount
	}
	return nil
}

func checkPositive(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}
