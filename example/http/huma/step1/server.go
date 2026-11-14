// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func newServer(store *store, logger *slog.Logger) *http.ServeMux {
	router := http.NewServeMux()
	api := humago.New(router, huma.DefaultConfig("Users", "1.0.0"))

	v1 := huma.NewGroup(api, "/v1")
	v1.UseMiddleware(logRequests(logger))
	users := huma.NewGroup(v1, "/users")

	h := &handlers{store: store}
	huma.Register(users, huma.Operation{
		OperationID: "listUsers",
		Method:      http.MethodGet,
		Path:        "",
		Summary:     "List the users",
	}, h.listUsers)
	huma.Register(users, huma.Operation{
		OperationID:   "createUser",
		Method:        http.MethodPost,
		Path:          "",
		Summary:       "Create a user",
		DefaultStatus: http.StatusCreated,
	}, h.createUser)
	huma.Register(users, huma.Operation{
		OperationID: "getUser",
		Method:      http.MethodGet,
		Path:        "/{id}",
		Summary:     "Get a user",
	}, h.getUser)
	huma.Register(users, huma.Operation{
		OperationID: "updateUser",
		Method:      http.MethodPut,
		Path:        "/{id}",
		Summary:     "Update a user",
	}, h.updateUser)
	huma.Register(users, huma.Operation{
		OperationID:   "deleteUser",
		Method:        http.MethodDelete,
		Path:          "/{id}",
		Summary:       "Delete a user",
		DefaultStatus: http.StatusNoContent,
	}, h.deleteUser)
	return router
}

func logRequests(logger *slog.Logger) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		next(ctx)
		logger.InfoContext(ctx.Context(), "request",
			"method", ctx.Method(),
			"path", ctx.URL().Path,
			"status", ctx.Status(),
		)
	}
}
