// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"math/rand/v2"
	"net/http"

	"github.com/z5labs/humus/example/internal/petstorepb"

	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
)

type AddStore interface {
	Add(context.Context, *petstorepb.Pet)
}

type addPetHandler struct {
	store AddStore
}

func AddPet(store AddStore) rest.Endpoint {
	h := &addPetHandler{
		store: store,
	}

	return rest.NewEndpoint(
		http.MethodPost,
		"/pet",
		h,
	)
}

func (h *addPetHandler) Handle(ctx context.Context, req *petstorepb.AddPetRequest) (*petstorepb.AddPetResponse, error) {
	spanCtx, span := otel.Tracer("endpoint").Start(ctx, "addPetHandler.Handle")
	defer span.End()

	if req.Pet.Id == 0 {
		req.Pet.Id = rand.Int64()
	}

	h.store.Add(spanCtx, req.Pet)

	resp := &petstorepb.AddPetResponse{
		Pet: req.Pet,
	}
	return resp, nil
}
