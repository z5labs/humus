// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/example/grpc/petstore/pet/registrar"
	"github.com/z5labs/humus/example/internal/pet"
	"github.com/z5labs/humus/grpc"
)

// BuildApi creates the gRPC API with all services registered.
func BuildApi(ctx context.Context) *grpc.Api {
	store := pet.NewStore()

	api := grpc.NewApi()

	registrar.Register(api, store)

	return api
}
