// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/example/rest/petstore/endpoint"
	"github.com/z5labs/humus/rest"
)

type Config struct {
	rest.Config `config:",squash"`
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
	api := rest.NewApi(
		cfg.OpenApi.Title,
		cfg.OpenApi.Version,
	)

	endpoint.RegisterPet(api)

	return api, nil
}
