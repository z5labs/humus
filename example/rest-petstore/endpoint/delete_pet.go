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

	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
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

	return rest.NewProtoEndpoint(
		http.MethodDelete,
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

func (h *deletePetHandler) Handle(ctx context.Context, req *emptypb.Empty) (*emptypb.Empty, error) {
	spanCtx, span := otel.Tracer("endpoint").Start(ctx, "deletePetHandler.Handle")
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

	h.store.Delete(spanCtx, id)

	return &emptypb.Empty{}, nil
}
