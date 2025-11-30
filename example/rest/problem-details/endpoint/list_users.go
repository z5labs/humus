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

type listUsersHandler struct {
	tracer trace.Tracer
	log    *slog.Logger
	store  *UserStore
}

type ListUsersResponse struct {
	Users []UserSummary `json:"users"`
}

type UserSummary struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func ListUsers(ctx context.Context, store *UserStore) rest.ApiOption {
	h := &listUsersHandler{
		tracer: otel.Tracer("github.com/z5labs/humus/example/rest/problem-details/endpoint"),
		log:    humus.Logger("github.com/z5labs/humus/example/rest/problem-details/endpoint"),
		store:  store,
	}

	return rest.Operation(
		http.MethodGet,
		rest.BasePath("/users"),
		rest.ProduceJson(h),
	)
}

func (h *listUsersHandler) Produce(ctx context.Context) (*ListUsersResponse, error) {
	users := h.store.List()

	summaries := make([]UserSummary, 0, len(users))
	for _, user := range users {
		summaries = append(summaries, UserSummary{
			ID:    user.ID,
			Name:  user.Name,
			Email: user.Email,
		})
	}

	return &ListUsersResponse{
		Users: summaries,
	}, nil
}
