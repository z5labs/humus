// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type getUserHandler struct {
	tracer trace.Tracer
	log    *slog.Logger
	store  *UserStore
}

type GetUserResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func GetUser(ctx context.Context, store *UserStore) rest.ApiOption {
	h := &getUserHandler{
		tracer: otel.Tracer("github.com/z5labs/humus/example/rest/problem-details/endpoint"),
		log:    humus.Logger("github.com/z5labs/humus/example/rest/problem-details/endpoint"),
		store:  store,
	}

	// Configure Problem Details error handler
	includeDetails := true // Development mode - set to false in production
	errorHandler := rest.NewProblemDetailsErrorHandler(rest.ProblemDetailsConfig{
		DefaultType:    "https://api.example.com/errors",
		IncludeDetails: &includeDetails,
	})

	return rest.Operation(
		http.MethodGet,
		rest.BasePath("/users").Param("id", rest.Required()),
		rest.ProduceJson(h),
		rest.OnError(errorHandler),
	)
}

func (h *getUserHandler) Produce(ctx context.Context) (*GetUserResponse, error) {
	// Get user ID from path parameter
	userID := rest.PathParamValue(ctx, "id")

	user, err := h.store.Get(userID)
	if err != nil {
		return nil, err
	}

	return &GetUserResponse{
		ID:    user.ID,
		Name:  user.Name,
		Email: user.Email,
	}, nil
}
