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

type listPetsHandler struct{}

func ListPets() rest.Endpoint {
	h := &listPetsHandler{}

	return rest.NewEndpoint(
		http.MethodGet,
		"/pets",
		h,
	)
}

func (h *listPetsHandler) Handle(ctx context.Context, req *emptypb.Empty) (*petstorepb.ListPetsResponse, error) {
	return nil, nil
}
