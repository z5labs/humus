// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/z5labs/humus/grpc/grpcclient"
)

func TestDefaultSettings_Classify(t *testing.T) {
	t.Parallel()

	classify := grpcclient.DefaultSettings().Classify

	// Every code, with what the default decides of it without a pushback,
	// and whether a pushback of 0 or more is the wait before its retry.
	rows := []struct {
		code     codes.Code
		want     grpcclient.Classification
		pushedBy bool
	}{
		{code: codes.OK, want: grpcclient.Classification{Retry: retry.Success, Breaker: circuit.Success}},
		{code: codes.Unavailable, want: grpcclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}, pushedBy: true},
		{code: codes.ResourceExhausted, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}, pushedBy: true},
		{code: codes.DeadlineExceeded, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}},
		{code: codes.Internal, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}},
		{code: codes.Unknown, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}},
		{code: codes.DataLoss, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}},
		{code: codes.Canceled, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Ignored}},
		{code: codes.InvalidArgument, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Success}},
		{code: codes.NotFound, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Success}},
		{code: codes.AlreadyExists, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Success}},
		{code: codes.PermissionDenied, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Success}},
		{code: codes.FailedPrecondition, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Success}},
		{code: codes.Aborted, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Success}},
		{code: codes.OutOfRange, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Success}},
		{code: codes.Unimplemented, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Success}},
		{code: codes.Unauthenticated, want: grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Success}},
	}
	require.Len(t, rows, 17, "every code gRPC defines")

	errOf := func(c codes.Code) error {
		if c == codes.OK {
			return nil
		}
		return status.Error(c, "the answer")
	}

	for _, row := range rows {
		for _, p := range []struct {
			pushback string // "" sends none
			// valid is a pushback of 0 or more, wait its length.
			valid bool
			wait  time.Duration
		}{
			{pushback: ""},
			{pushback: "0", valid: true, wait: 0},
			{pushback: "2000", valid: true, wait: 2 * time.Second},
			{pushback: "-1"},
			{pushback: "soon"},
		} {
			t.Run(fmt.Sprintf("%s with pushback %q", row.code, p.pushback), func(t *testing.T) {
				t.Parallel()
				var trailer metadata.MD
				if p.pushback != "" {
					trailer = metadata.Pairs("grpc-retry-pushback-ms", p.pushback)
				}

				want := row.want
				switch {
				case p.pushback == "":
				case p.valid && row.pushedBy:
					want.Retry, want.RetryAfter = retry.Retry, p.wait
				case !p.valid && row.code != codes.OK:
					want.Retry = retry.Stop
				}
				require.Equal(t, want, classify("/cart.v1.CartService/AddItem", errOf(row.code), trailer))
			})
		}
	}

	t.Run("an error with no gRPC status is Unknown", func(t *testing.T) {
		t.Parallel()
		require.Equal(t,
			grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure},
			classify("/cart.v1.CartService/AddItem", errors.New("an interceptor's error"), nil))
	})

	t.Run("a pushback given twice stops the retries", func(t *testing.T) {
		t.Parallel()
		trailer := metadata.Pairs("grpc-retry-pushback-ms", "100", "grpc-retry-pushback-ms", "200")
		require.Equal(t,
			grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure},
			classify("/cart.v1.CartService/AddItem", status.Error(codes.Unavailable, "not now"), trailer))
	})

	t.Run("a pushback too large for a Duration is the largest one", func(t *testing.T) {
		t.Parallel()
		for _, ms := range []string{"9223372036854775807", "99999999999999999999"} {
			trailer := metadata.Pairs("grpc-retry-pushback-ms", ms)
			require.Equal(t,
				grpcclient.Classification{Retry: retry.Retry, RetryAfter: math.MaxInt64, Breaker: circuit.Failure},
				classify("/cart.v1.CartService/AddItem", status.Error(codes.Unavailable, "not now"), trailer), ms)
		}
	})

	t.Run("a status's details are never read", func(t *testing.T) {
		t.Parallel()
		st, err := status.New(codes.Unavailable, "not now").WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(time.Minute)})
		require.NoError(t, err)
		require.Equal(t,
			grpcclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure},
			classify("/cart.v1.CartService/AddItem", st.Err(), nil))
	})
}
