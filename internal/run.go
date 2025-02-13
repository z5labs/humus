// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package internal

import (
	"context"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
)

func Run(ctx context.Context, cfg config.Source, builder bedrock.AppBuilder[config.Source]) error {
	app, err := builder.Build(ctx, cfg)
	if err != nil {
		return err
	}
	return app.Run(ctx)
}
