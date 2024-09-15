// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/z5labs/bedrock/pkg/config"
	"go.opentelemetry.io/otel"
)

type configSourceFunc func(config.Store) error

func (f configSourceFunc) Apply(store config.Store) error {
	return f(store)
}

type appFunc func(context.Context) error

func (f appFunc) Run(ctx context.Context) error {
	return f(ctx)
}

func TestRun(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if it read one of the config sources", func(t *testing.T) {
			build := func(ctx context.Context, cfg Config) (App, error) {
				return nil, nil
			}

			srcErr := errors.New("failed to apply config")
			src := configSourceFunc(func(s config.Store) error {
				return srcErr
			})

			err := run(build, src)
			if !assert.Equal(t, srcErr, err) {
				t.Log(err)
				return
			}
		})

		t.Run("if provided build function fails", func(t *testing.T) {
			buildErr := errors.New("failed to build custom app")
			build := func(ctx context.Context, cfg Config) (App, error) {
				return nil, buildErr
			}

			err := run(build)

			if !assert.ErrorIs(t, err, buildErr) {
				t.Log(err)
				return
			}
		})

		t.Run("if the built app returns an error", func(t *testing.T) {
			appErr := errors.New("failed to run app")
			app := appFunc(func(ctx context.Context) error {
				return appErr
			})

			build := func(ctx context.Context, cfg Config) (App, error) {
				return app, nil
			}

			err := run(build)

			if !assert.ErrorIs(t, err, appErr) {
				t.Log(err)
				return
			}
		})

		t.Run("if the otlp target can not be reached", func(t *testing.T) {
			app := appFunc(func(ctx context.Context) error {
				_, span := otel.Tracer("test").Start(ctx, "test")
				defer span.End()

				return nil
			})

			build := func(ctx context.Context, cfg Config) (App, error) {
				return app, nil
			}

			src := config.FromJson(strings.NewReader(`{
				"otel": {
					"otlp": {
						"target": "localhost:30000"
					}
				}
			}`))

			err := run(build, src)

			if !assert.Nil(t, err) {
				t.Log(err)
				return
			}
		})
	})
}
