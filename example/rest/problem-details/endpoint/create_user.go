// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

type createUserHandler struct {
	tracer trace.Tracer
	log    *slog.Logger
	store  *UserStore
}

type CreateUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type CreateUserResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func CreateUser(ctx context.Context, store *UserStore) rest.ApiOption {
	h := &createUserHandler{
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
		http.MethodPost,
		rest.BasePath("/users"),
		rest.HandleJson(h),
		rest.OnError(errorHandler),
	)
}

func (h *createUserHandler) Handle(ctx context.Context, req *CreateUserRequest) (*CreateUserResponse, error) {
	// Validate request
	validationErrors := make(map[string][]string)

	if req.Name == "" {
		validationErrors["name"] = append(validationErrors["name"], "Name is required")
	} else if len(req.Name) < 3 {
		validationErrors["name"] = append(validationErrors["name"], "Name must be at least 3 characters")
	} else if len(req.Name) > 50 {
		validationErrors["name"] = append(validationErrors["name"], "Name must be at most 50 characters")
	}

	if req.Email == "" {
		validationErrors["email"] = append(validationErrors["email"], "Email is required")
	} else if !emailRegex.MatchString(req.Email) {
		validationErrors["email"] = append(validationErrors["email"], "Email format is invalid")
	}

	if len(validationErrors) > 0 {
		return nil, newValidationError(validationErrors)
	}

	// Create user in store
	user, err := h.store.Create(req.Name, req.Email)
	if err != nil {
		return nil, err
	}

	return &CreateUserResponse{
		ID:    user.ID,
		Name:  user.Name,
		Email: user.Email,
	}, nil
}
