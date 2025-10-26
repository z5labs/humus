// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHandlerFunc_Handle(t *testing.T) {
	t.Run("will call the function successfully", func(t *testing.T) {
		called := false
		expectedResp := "response"

		f := HandlerFunc[string, string](func(ctx context.Context, req *string) (*string, error) {
			called = true
			assert.Equal(t, "request", *req)
			return &expectedResp, nil
		})

		req := "request"
		resp, err := f.Handle(context.Background(), &req)

		assert.True(t, called)
		assert.Nil(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, expectedResp, *resp)
	})

	t.Run("will return error from function", func(t *testing.T) {
		expectedErr := errors.New("test error")

		f := HandlerFunc[string, string](func(ctx context.Context, req *string) (*string, error) {
			return nil, expectedErr
		})

		req := "request"
		resp, err := f.Handle(context.Background(), &req)

		assert.Nil(t, resp)
		assert.Equal(t, expectedErr, err)
	})
}
