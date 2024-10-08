// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"net/http"

	"github.com/z5labs/bedrock/pkg/health"
	"google.golang.org/protobuf/types/known/emptypb"
)

type readinessHandler struct {
	health.Metric
}

func readinessEndpoint(m health.Metric) Endpoint {
	h := &readinessHandler{
		Metric: m,
	}
	return NewEndpoint(
		http.MethodGet,
		"/health/readiness",
		ConsumesProto(
			ProducesProto(h),
		),
		Returns(http.StatusServiceUnavailable),
	)
}

func (h *readinessHandler) Handle(ctx context.Context, req *emptypb.Empty) (*emptypb.Empty, error) {
	healthy := h.Healthy(ctx)
	if healthy {
		return &emptypb.Empty{}, nil
	}
	return nil, Error(http.StatusServiceUnavailable, "not ready")
}

type livenessHandler struct {
	health.Metric
}

func livenessEndpoint(m health.Metric) Endpoint {
	h := &livenessHandler{
		Metric: m,
	}
	return NewEndpoint(
		http.MethodGet,
		"/health/liveness",
		ConsumesProto(
			ProducesProto(h),
		),
		Returns(http.StatusServiceUnavailable),
	)
}

func (h *livenessHandler) Handle(ctx context.Context, req *emptypb.Empty) (*emptypb.Empty, error) {
	healthy := h.Healthy(ctx)
	if healthy {
		return &emptypb.Empty{}, nil
	}
	return nil, Error(http.StatusServiceUnavailable, "not alive")
}
