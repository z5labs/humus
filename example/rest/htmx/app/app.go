// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/example/rest/htmx/endpoint"
	"github.com/z5labs/humus/rest"
)

type Config struct {
	rest.Config `config:",squash"`
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
	// Create in-memory item store
	store := endpoint.NewItemStore()

	api := rest.NewApi(
		cfg.OpenApi.Title,
		cfg.OpenApi.Version,
		endpoint.MainPage(ctx, store),
		endpoint.AddItem(ctx, store),
	)

	return api, nil
}
