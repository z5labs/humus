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
	"google.golang.org/protobuf/types/known/emptypb"
)

type PetByIdStore interface {
	Get(context.Context, int64) (*petstorepb.Pet, bool)
}

type findPetByIdHandler struct {
	store PetByIdStore
}

func FindPetByID(store PetByIdStore) rest.Endpoint {
	h := &findPetByIdHandler{
		store: store,
	}

	return rest.NewEndpoint(
		http.MethodGet,
		"/pet/{id}",
		h,
		rest.PathParams(
			rest.PathParam{
				Name:     "id",
				Required: true,
			},
		),
	)
}

func (h *findPetByIdHandler) Handle(ctx context.Context, req *emptypb.Empty) (*petstorepb.FindPetByIdResponse, error) {
	return nil, nil
}
