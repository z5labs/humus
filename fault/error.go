// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package fault

import (
	"github.com/z5labs/bedrock/fit"
)

// errorType is the error.type of every InjectedError.
const errorType = "github.com/z5labs/humus/fault.InjectedError"

// InjectedError is the error of an attempt that a fault ended at Point.
// With Err nil, it is an abort, and it matches fit.ErrInjected with
// errors.Is. Otherwise it is a Delay that Err, the attempt's context's
// cause, cut short, and it matches Err. Its error.type is
// github.com/z5labs/humus/fault.InjectedError either way.
type InjectedError struct {
	// Point is the injection point the fault was at.
	Point string

	// Err is what cut the fault's Delay short, or nil for an abort.
	Err error
}

// Error implements the error interface. An abort reads as fit.ErrInjected
// at Point, and a Delay cut short names Point and Err.
func (e *InjectedError) Error() string {
	if e.Err == nil {
		return fit.ErrInjected.Error() + " at " + e.Point
	}
	return "fault: the delay injected at " + e.Point + " was cut short: " + e.Err.Error()
}

// Unwrap returns Err, or fit.ErrInjected for an abort.
func (e *InjectedError) Unwrap() error {
	if e.Err == nil {
		return fit.ErrInjected
	}
	return e.Err
}

// ErrorType returns github.com/z5labs/humus/fault.InjectedError, the
// error.type of every injected failure, abort or Delay, in every client's
// spans and in otelhttp's metrics. It is a method because otelhttp names
// an error by its reflected type while semconv.ErrorType names a pointer
// to one, and the two would disagree without it.
func (e *InjectedError) ErrorType() string {
	return errorType
}
