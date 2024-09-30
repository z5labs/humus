// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/example/petstore/petstorepb"

	"github.com/z5labs/humus/rest"
	"google.golang.org/protobuf/types/known/emptypb"
)

func FindPetByID() rest.Endpoint {
	h := &findPetByIdHandler{}

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

type findPetByIdHandler struct{}

func (h *findPetByIdHandler) Handle(ctx context.Context, req *emptypb.Empty) (*petstorepb.FindPetByIdResponse, error) {
	return nil, nil
}
