// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/z5labs/bedrock/circuit"

	"github.com/z5labs/humus/queue"
)

// Settings configures a Service. Every field is a plain value, and a field's
// zero value means zero, never "use the default": start from DefaultSettings
// and change the fields you source. Brokers, GroupID and Topics have no
// default, so they are always among them.
type Settings struct {
	// Brokers are the addresses of the brokers the Service bootstraps from,
	// each as host:port. They are required.
	//
	// Default: none. Valid: at least one; each host:port, with a host and a
	// port from 1 to 65535. Check: CheckBrokers.
	Brokers []string

	// GroupID is the consumer group the Service joins. Every replica of a
	// consumer shares it, so the group's partitions are split among them.
	// It is required.
	//
	// Default: none (""). Valid: not empty. Check: CheckGroupID.
	GroupID string

	// Topics are the topics the Service consumes. They are required.
	//
	// Default: none. Valid: at least one; none empty; no topic twice.
	// Check: CheckTopics.
	Topics []string

	// Delivery is when a record is committed, relative to processing it.
	//
	// Default: queue.AtLeastOnce. Valid: queue.AtLeastOnce or
	// queue.AtMostOnce. Check: queue.CheckDelivery.
	Delivery queue.Delivery

	// TLS is the config the Service dials the brokers with. nil means
	// plaintext. A client certificate in Certificates makes it mTLS.
	//
	// Default: nil, which means plaintext. Valid: any. Check: none.
	TLS *tls.Config

	// SASL is the mechanism the Service authenticates to the brokers with,
	// one of franz-go's from github.com/twmb/franz-go/pkg/sasl, carrying
	// the end user's credentials. nil means no authentication.
	//
	// Default: nil, which means no authentication. Valid: any. Check: none.
	SASL sasl.Mechanism

	// SessionTimeout is how long the group's coordinator waits for a
	// heartbeat before it removes the Service from the group and gives its
	// partitions to another member.
	//
	// Default: 45s. Valid: positive. Check: CheckSessionTimeout.
	SessionTimeout time.Duration

	// RebalanceTimeout is how long the group's coordinator waits for every
	// member to rejoin during a rebalance. A revoked partition's record in
	// flight has to finish within it.
	//
	// Default: 60s. Valid: positive. Check: CheckRebalanceTimeout.
	RebalanceTimeout time.Duration

	// FetchMaxBytes bounds how many bytes a broker returns for one fetch.
	//
	// Default: 50 MiB (50 << 20). Valid: positive. Check: CheckFetchMaxBytes.
	FetchMaxBytes int32

	// MaxPollRecords bounds how many records one poll hands out to be
	// processed.
	//
	// Default: 500. Valid: positive. Check: CheckMaxPollRecords.
	MaxPollRecords int

	// TrustScenario reports whether the failure scenario a record carries
	// reaches the Processor. A record it doesn't trust has the scenario
	// stripped from its context first. It is asked only about a record
	// that carries a scenario, and only when the App has failure
	// injection. It is set in code, not sourced.
	//
	// Default: nil, which trusts no producer. Valid: any. Check: none.
	TrustScenario func(m Message) bool
}

// DefaultSettings returns the Service's defaults. Brokers, GroupID and
// Topics have none, so its Brokers and Topics are nil and its GroupID is
// "", which CheckBrokers, CheckGroupID and CheckTopics reject: the end user
// always sources all three.
func DefaultSettings() Settings {
	return Settings{
		Brokers:          nil,
		GroupID:          "",
		Topics:           nil,
		Delivery:         queue.AtLeastOnce,
		TLS:              nil,
		SASL:             nil,
		SessionTimeout:   45 * time.Second,
		RebalanceTimeout: 60 * time.Second,
		FetchMaxBytes:    50 << 20,
		MaxPollRecords:   500,
		TrustScenario:    nil,
	}
}

// The causes the checks report. None of them names a field: whoever calls a
// check names the field, or the variable that sourced it.
var (
	errEmpty       = errors.New("must not be empty")
	errNoneListed  = errors.New("must list at least one")
	errNotPositive = errors.New("must be positive")
	errNotSet      = errors.New("must be set")
)

// CheckBrokers rejects an empty list, and each broker that is not host:port
// with a host and a port from 1 to 65535. Every bad broker is reported, each
// by its value.
func CheckBrokers(brokers []string) error {
	if len(brokers) == 0 {
		return errNoneListed
	}
	var errs []error
	for _, b := range brokers {
		if err := checkBroker(b); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func checkBroker(b string) error {
	host, port, err := net.SplitHostPort(b)
	if err != nil {
		return fmt.Errorf("%q is not host:port", b)
	}
	if host == "" {
		return fmt.Errorf("%q has no host", b)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return fmt.Errorf("%q has no port from 1 to 65535", b)
	}
	return nil
}

// CheckGroupID rejects "".
func CheckGroupID(id string) error {
	if id == "" {
		return errEmpty
	}
	return nil
}

// CheckTopics rejects an empty list, an empty topic, and a topic named
// twice. Every problem is reported.
func CheckTopics(topics []string) error {
	if len(topics) == 0 {
		return errNoneListed
	}
	var errs []error
	seen := make(map[string]int, len(topics))
	for i, t := range topics {
		seen[t]++
		switch {
		case t == "":
			errs = append(errs, fmt.Errorf("has an empty name at index %d", i))
		case seen[t] == 2: // reported once, however many times it repeats
			errs = append(errs, fmt.Errorf("names %q more than once", t))
		}
	}
	return errors.Join(errs...)
}

// CheckSessionTimeout rejects a timeout that is not positive.
func CheckSessionTimeout(d time.Duration) error {
	return checkPositive(d)
}

// CheckRebalanceTimeout rejects a timeout that is not positive.
func CheckRebalanceTimeout(d time.Duration) error {
	return checkPositive(d)
}

// CheckFetchMaxBytes rejects a size that is not positive.
func CheckFetchMaxBytes(n int32) error {
	return checkPositive(n)
}

// CheckMaxPollRecords rejects a count that is not positive.
func CheckMaxPollRecords(n int) error {
	return checkPositive(n)
}

func checkPositive[T time.Duration | int32 | int](v T) error {
	if v <= 0 {
		return errNotPositive
	}
	return nil
}

// ProducerSettings configures a Producer. Every field is a plain value, and a
// field's zero value means zero, never "use the default": start from
// DefaultProducerSettings and change the fields you source. Brokers has no
// default, so it is always among them.
type ProducerSettings struct {
	// Brokers are the addresses of the brokers the Producer bootstraps
	// from, each as host:port. They are required.
	//
	// Default: none. Valid: at least one; each host:port, with a host and a
	// port from 1 to 65535. Check: CheckBrokers.
	Brokers []string

	// TLS is the config the Producer dials the brokers with. nil means
	// plaintext. A client certificate in Certificates makes it mTLS.
	//
	// Default: nil, which means plaintext. Valid: any. Check: none.
	TLS *tls.Config

	// SASL is the mechanism the Producer authenticates to the brokers with,
	// one of franz-go's from github.com/twmb/franz-go/pkg/sasl, carrying
	// the end user's credentials. nil means no authentication.
	//
	// Default: nil, which means no authentication. Valid: any. Check: none.
	SASL sasl.Mechanism

	// DeliveryTimeout bounds how long the cluster has to acknowledge the
	// records of one call to Produce, franz-go's retries included. A record
	// that isn't acknowledged by then fails, even one already sent, and
	// may still have been stored.
	//
	// Default: 10s. Valid: positive. Check: CheckDeliveryTimeout.
	DeliveryTimeout time.Duration

	// Breaker configures the circuit breaker in front of the cluster.
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
	// or more calls failed; while half-open, reopen on a failed probe and
	// close after 3 successful ones. Valid: Trip and Probe set, which
	// NewProducer checks. Check: none.
	BreakerPolicy circuit.Policy

	// ForwardScenario sends the failure scenario a call carries on in the
	// headers of the records it produces, for consumers that take part in
	// failure injection with the same codec. When it is false, the
	// scenario is stripped from each record's baggage header, and the rest
	// of the call's baggage still goes. It is set in code, not sourced.
	//
	// Default: false. Valid: either. Check: none.
	ForwardScenario bool
}

// DefaultProducerSettings returns the Producer's defaults. Brokers has none,
// so it is nil, which CheckBrokers rejects: the end user always sources it.
func DefaultProducerSettings() ProducerSettings {
	return ProducerSettings{
		Brokers:         nil,
		TLS:             nil,
		SASL:            nil,
		DeliveryTimeout: 10 * time.Second,
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
// the last 20 or more calls failed.
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

// CheckDeliveryTimeout rejects a timeout that is not positive.
func CheckDeliveryTimeout(d time.Duration) error {
	return checkPositive(d)
}

// errOutOfCount is the cause CheckBreakerWindow and CheckBreakerProbes
// report.
var errOutOfCount = fmt.Errorf("must be from 1 to %d", circuit.MaxCount)

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
