// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/retry"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	forced := &amqp.Error{Code: amqp.ConnectionForced, Reason: "CONNECTION_FORCED - broker forced connection closure with reason 'shutdown'", Server: true}
	for _, tc := range []struct {
		name string
		err  error
		want retry.Outcome
	}{
		{name: "confirmed", err: nil, want: retry.Success},
		{name: "returned unroutable", err: fmt.Errorf("rabbitmq: publishing to x: %w: 312 NO_ROUTE", ErrUnroutable), want: retry.Stop},
		{name: "404 NOT_FOUND", err: fmt.Errorf("rabbitmq: publishing to x: %w", &amqp.Error{Code: amqp.NotFound, Server: true}), want: retry.Stop},
		{name: "403 ACCESS_REFUSED", err: fmt.Errorf("rabbitmq: publishing to x: %w", &amqp.Error{Code: amqp.AccessRefused, Server: true}), want: retry.Stop},
		{name: "a channel that failed to open on an open connection", err: fmt.Errorf("rabbitmq: opening a channel to x: %w", &amqp.Error{Code: amqp.NotAllowed}), want: retry.Stop},
		{name: "nacked", err: fmt.Errorf("rabbitmq: publishing to x: %w", errNacked), want: retry.Retry},
		{name: "a dropped connection", err: lost(fmt.Errorf("rabbitmq: publishing to x: %w", forced)), want: retry.Retry},
		{name: "a channel closed without the broker's error", err: lost(fmt.Errorf("rabbitmq: publishing to x: %w", amqp.ErrClosed)), want: retry.Retry},
		{name: "a failed dial", err: lost(fmt.Errorf("rabbitmq: dialling x: %w", refused)), want: retry.Retry},
		{name: "a failed handshake", err: lost(fmt.Errorf("rabbitmq: AMQP handshake with x: %w", amqp.ErrCredentials)), want: retry.Retry},
		{name: "a dial ConnectTimeout ended", err: lost(fmt.Errorf("rabbitmq: dialling x: %w", context.DeadlineExceeded)), want: retry.Retry},
		{name: "the Publisher stopped", err: errStopped, want: retry.Stop},
		{name: "anything else", err: errors.New("something else"), want: retry.Stop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, wait := classify(t.Context(), tc.err)
			require.Equal(t, tc.want, got)
			require.Zero(t, wait, "the Publisher's waits are retry.Exponential's alone")
		})
	}
}

func TestClassify_an_attempt_ended_by_its_context_is_retried_so_retry_decides_Expired(t *testing.T) {
	t.Parallel()

	for name, cancel := range map[string]func() context.Context{
		"its deadline": func() context.Context {
			ctx, cancel := context.WithTimeout(context.Background(), 0)
			cancel()
			return ctx
		},
		"the caller cancelling": func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := cancel()
			got, _ := classify(ctx, fmt.Errorf("rabbitmq: waiting for the broker to confirm: %w", ctx.Err()))
			require.Equal(t, retry.Retry, got)

			// What the broker refused stays refused.
			got, _ = classify(ctx, ErrUnroutable)
			require.Equal(t, retry.Stop, got)
		})
	}
}

func TestLost_changes_neither_the_message_nor_the_error_type(t *testing.T) {
	t.Parallel()

	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, err := range []error{
		fmt.Errorf("rabbitmq: dialling x: %w", refused),
		fmt.Errorf("rabbitmq: publishing to x: %w", amqp.ErrClosed),
		context.DeadlineExceeded,
	} {
		marked := lost(err)
		require.Equal(t, err.Error(), marked.Error())
		require.ErrorIs(t, marked, err)
		require.Equal(t, semconv.ErrorType(err), semconv.ErrorType(marked))
		require.Equal(t, semconv.ErrorType(err), semconv.ErrorType(fmt.Errorf("wrapped: %w", marked)))
	}
}
