// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/example/internal/petstore"
	"github.com/z5labs/humus/example/rest-petstore/endpoint"

	"github.com/z5labs/humus/rest"
)

type Config struct {
	rest.Config `config:",squash"`
}

func Init(ctx context.Context, cfg Config) (*rest.App, error) {
	store := petstore.NewInMemory()

	app := rest.New(
		rest.ListenOn(cfg.Http.Port),
		rest.Title("Pet Store API"),
		rest.Version("v0.0.0"),
		rest.RegisterEndpoint(endpoint.AddPet(store)),
		rest.RegisterEndpoint(endpoint.DeletePet(store)),
		rest.RegisterEndpoint(endpoint.FindPetByID(store)),
		rest.RegisterEndpoint(endpoint.ListPets(store)),
		rest.RegisterEndpoint(endpoint.Upload(store, store)),
		rest.RegisterEndpoint(endpoint.Download(store, store)),
	)

	return app, nil
}
