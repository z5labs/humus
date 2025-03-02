// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package echo

import (
	"context"

	"github.com/z5labs/humus/grpc/internal/echopb"
	"github.com/z5labs/humus/internal/ptr"
)

type Service struct {
	echopb.UnimplementedEchoServer
}

func (s Service) Echo(ctx context.Context, req *echopb.EchoRequest) (*echopb.EchoResponse, error) {
	b := echopb.EchoResponse_builder{
		Msg: ptr.Ref(req.GetMsg()),
	}
	return b.Build(), nil
}
