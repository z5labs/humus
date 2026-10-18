// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package fault_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/z5labs/humus/fault"
)

func TestInjectedError_ErrorType_is_the_same_for_an_abort_and_a_Delay(t *testing.T) {
	const want = "github.com/z5labs/humus/fault.InjectedError"
	for name, err := range map[string]*fault.InjectedError{
		"abort":           {Point: "http:payments:8443"},
		"delay cut short": {Point: "http:payments:8443", Err: context.DeadlineExceeded},
		"delay cancelled": {Point: "grpc:payments.v1.Payments/Charge", Err: context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, err.ErrorType())
			require.Equal(t, semconv.ErrorTypeKey.String(want), semconv.ErrorType(err), "semconv.ErrorType of a pointer")
			require.Equal(t, semconv.ErrorTypeKey.String(want), semconv.ErrorType(fmt.Errorf("attempt: %w", err)), "semconv.ErrorType of a wrapped one")
		})
	}
}
