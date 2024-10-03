// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/z5labs/humus/example/internal/petstorepb"

	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
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
		rest.Returns(http.StatusBadRequest),
	)
}

func (h *findPetByIdHandler) Handle(ctx context.Context, req *emptypb.Empty) (*petstorepb.FindPetByIdResponse, error) {
	spanCtx, span := otel.Tracer("endpoint").Start(ctx, "findPetByIdHandler.Handle")
	defer span.End()

	pathId := rest.PathValue(ctx, "id")
	pathId = strings.TrimSpace(pathId)
	if len(pathId) == 0 {
		return nil, rest.Error(http.StatusBadRequest, "missing pet id")
	}

	id, err := strconv.ParseInt(pathId, 10, 64)
	if err != nil {
		span.RecordError(err)
		return nil, rest.Error(http.StatusBadRequest, "pet id must be an integer")
	}

	pet, found := h.store.Get(spanCtx, id)
	if !found {
		return nil, rest.Error(http.StatusNotFound, "")
	}

	resp := &petstorepb.FindPetByIdResponse{
		Pet: pet,
	}
	return resp, nil
}
