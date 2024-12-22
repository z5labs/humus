// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/rest"
)

type Config struct {
	rest.Config `config:",squash"`
}

func Init(ctx context.Context, cfg Config) (rest.Api, error) {
	m := rest.NewMux(
		cfg.OpenApi.Title,
		cfg.OpenApi.Version,
	)

	return m, nil
}
