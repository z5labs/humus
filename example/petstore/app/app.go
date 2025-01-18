// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/example/petstore/endpoint"
	"github.com/z5labs/humus/example/petstore/pet"

	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/mux"
)

type Config struct {
	rest.Config `config:",squash"`
}

func Init(ctx context.Context, cfg Config) (*mux.Router, error) {
	m := mux.New(
		cfg.OpenApi.Title,
		cfg.OpenApi.Version,
	)

	store := pet.NewStore()

	endpoint.RegisterPet(m, store)

	return m, nil
}
