// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/example/petstore/pet"
	"go.opentelemetry.io/otel"

	"github.com/z5labs/humus/rest/mux"
	"github.com/z5labs/humus/rest/rpc"
)

type RegisterPetStore interface {
	Register(context.Context, *pet.RegisterRequest) (*pet.RegisterResponse, error)
}

type registerPetHandler struct {
	store RegisterPetStore
}

func RegisterPet(m mux.Muxer, store RegisterPetStore) {
	h := &registerPetHandler{
		store: store,
	}

	mux.MustRoute(
		m,
		http.MethodPost,
		"/pet/register",
		rpc.NewOperation(
			rpc.ConsumesJson(
				rpc.ProducesJson(h),
			),
		),
	)
}

type RegisterPetRequest struct {
	Name string   `json:"name"`
	Kind pet.Kind `json:"kind"`
}

type RegisterPetResponse struct {
	Pet pet.Pet `json:"pet"`
}

func (h *registerPetHandler) Handle(ctx context.Context, req *RegisterPetRequest) (*RegisterPetResponse, error) {
	spanCtx, span := otel.Tracer("endpoint").Start(ctx, "registerPetHandler.Handle")
	defer span.End()

	resp, err := h.store.Register(spanCtx, &pet.RegisterRequest{
		Name: req.Name,
		Kind: req.Kind,
	})
	if err != nil {
		return nil, err
	}

	petResp := &RegisterPetResponse{
		Pet: resp.Pet,
	}
	return petResp, nil
}
