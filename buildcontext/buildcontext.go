// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package buildcontext

import (
	"context"

	"github.com/z5labs/bedrock"
)

type buildContextKeyType string

const buildContextKey = buildContextKeyType("buildContextKey")

// From
func From[T any](ctx context.Context) (T, bool) {
	t, ok := ctx.Value(buildContextKey).(T)
	return t, ok
}

func inject[T any](ctx context.Context, t T) context.Context {
	return context.WithValue(ctx, buildContextKey, t)
}

type AppBuilder[B, T any] interface {
	Build(context.Context, B, T) (bedrock.App, error)
}

type AppBuilderFunc[B, T any] func(context.Context, B, T) (bedrock.App, error)

func (f AppBuilderFunc[B, T]) Build(ctx context.Context, b B, t T) (bedrock.App, error) {
	return f(ctx, b, t)
}

// BuildWith
func BuildWith[B, T any](b B, base AppBuilder[B, T]) bedrock.AppBuilder[T] {
	return bedrock.AppBuilderFunc[T](func(ctx context.Context, cfg T) (bedrock.App, error) {
		ctx = inject(ctx, b)
		return base.Build(ctx, b, cfg)
	})
}
