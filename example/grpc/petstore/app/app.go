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

type Config struct {
	grpc.Config `config:",squash"`
}

func Init(ctx context.Context, cfg Config) (*grpc.Api, error) {
	store := pet.NewStore()

	api := grpc.NewApi()

	registrar.Register(api, store)

	return api, nil
}
