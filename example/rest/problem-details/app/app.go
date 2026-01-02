// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/example/rest/problem-details/endpoint"
	"github.com/z5labs/humus/rest"
)

// BuildApi creates the REST API with all endpoints registered.
func BuildApi(ctx context.Context) *rest.Api {
	// Create in-memory user store
	store := endpoint.NewUserStore()

	api := rest.NewApi(
		"Problem Details API",
		"v0.0.0",
		endpoint.CreateUser(ctx, store),
		endpoint.GetUser(ctx, store),
		endpoint.ListUsers(ctx, store),
	)

	return api
}
