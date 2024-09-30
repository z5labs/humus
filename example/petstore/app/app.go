// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/petstore/endpoint"
	"github.com/z5labs/humus/rest"
)

type Config struct {
	rest.Config `config:",squash"`
}

func Init(ctx context.Context, cfg Config) (humus.App, error) {
	app := rest.New(
		rest.RegisterEndpoint(endpoint.AddPet()),
		rest.RegisterEndpoint(endpoint.DeletePet()),
		rest.RegisterEndpoint(endpoint.FindPetByID()),
		rest.RegisterEndpoint(endpoint.ListPets()),
	)
	return app, nil
}
