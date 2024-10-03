// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/example/internal/petstorepb"

	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/types/known/emptypb"
)

type ListStore interface {
	Pets(context.Context) []*petstorepb.Pet
}

type listPetsHandler struct {
	store ListStore
}

func ListPets(store ListStore) rest.Endpoint {
	h := &listPetsHandler{
		store: store,
	}

	return rest.NewEndpoint(
		http.MethodGet,
		"/pets",
		h,
	)
}

func (h *listPetsHandler) Handle(ctx context.Context, req *emptypb.Empty) (*petstorepb.ListPetsResponse, error) {
	spanCtx, span := otel.Tracer("endpoint").Start(ctx, "listPetsHandler.Handle")
	defer span.End()

	pets := h.store.Pets(spanCtx)

	resp := &petstorepb.ListPetsResponse{
		Pets: pets,
	}

	return resp, nil
}
