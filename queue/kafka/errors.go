// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/twmb/franz-go/pkg/kbin"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Error is a Kafka error code that the cluster answered with, as the Kafka
// protocol defines it. errors.As finds it in the Service's and the
// Producer's errors.
type Error struct {
	// Code is the code's number, such as 29.
	Code int16
	// Name is the code's name, such as TOPIC_AUTHORIZATION_FAILED.
	Name string
	// Retriable reports whether the protocol marks the code retriable.
	Retriable bool
}

// Error returns the code's name and number, such as
// "kafka: TOPIC_AUTHORIZATION_FAILED (error code 29)".
func (e *Error) Error() string {
	if e.Name == "" {
		return fmt.Sprintf("kafka: error code %d", e.Code)
	}
	return fmt.Sprintf("kafka: %s (error code %d)", e.Name, e.Code)
}

// Is reports whether target is an *Error with the same Code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t != nil && t.Code == e.Code
}

var (
	// ErrDeliveryTimeout is a record the cluster didn't acknowledge within
	// ProducerSettings.DeliveryTimeout. An error that matches it also
	// matches context.DeadlineExceeded.
	ErrDeliveryTimeout = errors.New("kafka: not acknowledged within DeliveryTimeout")

	// ErrProducerClosed is a record the Producer failed because it was
	// closed: at the end of the grace period, or on a Produce after the
	// Producer's OnStop function has run.
	ErrProducerClosed = errors.New("kafka: the Producer is closed")
)

// isCode reports whether err is, or wraps, the Kafka error code code.
func isCode(err error, code *kerr.Error) bool {
	return errors.Is(err, &Error{Code: code.Code})
}

// fromClient returns err, an error from the client, as an error of humus's,
// with no type or value of franz-go's in its chain. A Kafka error code is an
// *Error with the code's number, name and retriability. Any other error of
// franz-go's keeps its message, and the errors it wraps, each of them
// translated in turn, so the standard library's, such as context, net and
// crypto/tls errors, are still found. An error with nothing of franz-go's in
// its chain is returned as it is.
func fromClient(err error) error {
	if !hasClientError(err) {
		return err
	}
	if ke, ok := err.(*kerr.Error); ok {
		return &Error{Code: ke.Code, Name: ke.Message, Retriable: ke.Retriable}
	}
	var wrapped []error
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		wrapped = []error{u.Unwrap()}
	case interface{ Unwrap() []error }:
		wrapped = u.Unwrap()
	}
	errs := make([]error, 0, len(wrapped))
	for _, w := range wrapped {
		if w != nil {
			errs = append(errs, fromClient(w))
		}
	}
	return &clientError{msg: err.Error(), errs: errs}
}

// recordError returns err, the result franz-go gave a record the Producer
// produced, as an error of humus's. A record failed for time, whether by its
// call's DeliveryTimeout deadline or by the client's own timeout, matches
// ErrDeliveryTimeout and context.DeadlineExceeded. A record failed because
// the client was closed is ErrProducerClosed. Anything else is fromClient's.
func recordError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, kgo.ErrClientClosed):
		return ErrProducerClosed
	case err == context.DeadlineExceeded:
		return &timedOutError{}
	case errors.Is(err, kgo.ErrRecordTimeout), errors.Is(err, context.DeadlineExceeded):
		return &timedOutError{cause: fromClient(err)}
	default:
		return fromClient(err)
	}
}

// timedOutError is a record that failed for time. cause is the client's
// error, translated, or nil when it is the deadline's alone.
type timedOutError struct {
	cause error
}

func (e *timedOutError) Error() string {
	if e.cause == nil {
		return ErrDeliveryTimeout.Error()
	}
	return ErrDeliveryTimeout.Error() + ": " + e.cause.Error()
}

func (e *timedOutError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrDeliveryTimeout, context.DeadlineExceeded}
	}
	return []error{ErrDeliveryTimeout, context.DeadlineExceeded, e.cause}
}

// clientError is an error of franz-go's that is not a Kafka error code: its
// message, and the errors it wraps, translated.
type clientError struct {
	msg  string
	errs []error
}

func (e *clientError) Error() string { return e.msg }

func (e *clientError) Unwrap() []error { return e.errs }

// franzGo is the module path of franz-go, and of every one of its packages'
// paths.
const franzGo = "github.com/twmb/franz-go"

// clientValues are the error values franz-go's packages export whose type is
// not franz-go's own, so only the value tells them apart.
var clientValues = []error{
	kgo.ErrRecordTimeout,
	kgo.ErrRecordRetries,
	kgo.ErrMaxBuffered,
	kgo.ErrAborting,
	kgo.ErrClientClosed,
	kgo.ErrMaxDecompress,
	kbin.ErrNotEnoughData,
}

// hasClientError reports whether err, or any error it wraps, has a type or
// a value of franz-go's.
func hasClientError(err error) bool {
	if err == nil {
		return false
	}
	if isClientError(err) {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return hasClientError(u.Unwrap())
	case interface{ Unwrap() []error }:
		return slices.ContainsFunc(u.Unwrap(), hasClientError)
	}
	return false
}

// isClientError reports whether err itself, not what it wraps, has a type or
// a value of franz-go's.
func isClientError(err error) bool {
	t := reflect.TypeOf(err)
	for _, v := range clientValues {
		if t == reflect.TypeOf(v) && err == v {
			return true
		}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	path := t.PkgPath()
	return path == franzGo || strings.HasPrefix(path, franzGo+"/")
}
