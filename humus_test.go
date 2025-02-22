// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/z5labs/bedrock"
)

type appFunc func(context.Context) error

func (f appFunc) Run(ctx context.Context) error {
	return f(ctx)
}

func TestRunner_Run(t *testing.T) {
	t.Run("will handle an error", func(t *testing.T) {
		t.Run("if the bedrock.AppBuilder fails", func(t *testing.T) {
			buildErr := errors.New("failed to build")
			builder := bedrock.AppBuilderFunc[struct{}](func(ctx context.Context, cfg struct{}) (bedrock.App, error) {
				return nil, buildErr
			})

			var capturedErr error
			r := NewRunner(builder, OnError(ErrorHandlerFunc(func(err error) {
				capturedErr = err
			})))
			r.Run(context.Background(), struct{}{})

			if !assert.Equal(t, buildErr, capturedErr) {
				return
			}
		})

		t.Run("if the bedrock.App fails", func(t *testing.T) {
			runErr := errors.New("failed to run")
			app := appFunc(func(ctx context.Context) error {
				return runErr
			})
			builder := bedrock.AppBuilderFunc[struct{}](func(ctx context.Context, cfg struct{}) (bedrock.App, error) {
				return app, nil
			})

			var capturedErr error
			r := NewRunner(builder, OnError(ErrorHandlerFunc(func(err error) {
				capturedErr = err
			})))
			r.Run(context.Background(), struct{}{})

			if !assert.Equal(t, runErr, capturedErr) {
				return
			}
		})
	})
}
