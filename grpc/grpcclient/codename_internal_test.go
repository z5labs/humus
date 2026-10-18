// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// TestCodeName checks codeName against gRPC's canonical names, which
// otelgrpc writes in rpc.response.status_code.
func TestCodeName(t *testing.T) {
	t.Parallel()
	canonical := map[codes.Code]string{
		codes.OK:                 "OK",
		codes.Canceled:           "CANCELLED",
		codes.Unknown:            "UNKNOWN",
		codes.InvalidArgument:    "INVALID_ARGUMENT",
		codes.DeadlineExceeded:   "DEADLINE_EXCEEDED",
		codes.NotFound:           "NOT_FOUND",
		codes.AlreadyExists:      "ALREADY_EXISTS",
		codes.PermissionDenied:   "PERMISSION_DENIED",
		codes.ResourceExhausted:  "RESOURCE_EXHAUSTED",
		codes.FailedPrecondition: "FAILED_PRECONDITION",
		codes.Aborted:            "ABORTED",
		codes.OutOfRange:         "OUT_OF_RANGE",
		codes.Unimplemented:      "UNIMPLEMENTED",
		codes.Internal:           "INTERNAL",
		codes.Unavailable:        "UNAVAILABLE",
		codes.DataLoss:           "DATA_LOSS",
		codes.Unauthenticated:    "UNAUTHENTICATED",
	}
	for code, want := range canonical {
		require.Equal(t, want, codeName(code), "code %d", code)
	}
}
