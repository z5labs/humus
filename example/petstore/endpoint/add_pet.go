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
)

func AddPet() rest.Endpoint {
	h := &addPetHandler{}

	return rest.NewEndpoint(
		http.MethodPost,
		"/pet",
		h,
	)
}

type addPetHandler struct{}

func (h *addPetHandler) Handle(ctx context.Context, req *petstorepb.AddPetRequest) (*petstorepb.AddPetResponse, error) {
	return nil, nil
}
