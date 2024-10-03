// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/rest"
	"google.golang.org/protobuf/types/known/emptypb"
)

type DeleteStore interface {
	Delete(context.Context, int64)
}

type deletePetHandler struct {
	store DeleteStore
}

func DeletePet(store DeleteStore) rest.Endpoint {
	h := &deletePetHandler{
		store: store,
	}

	return rest.NewEndpoint(
		http.MethodDelete,
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

func (h *deletePetHandler) Handle(ctx context.Context, req *emptypb.Empty) (*emptypb.Empty, error) {
	return nil, nil
}
