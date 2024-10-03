// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/z5labs/humus/humuspb"
	"google.golang.org/protobuf/proto"
)

type deleteStoreFunc func(context.Context, int64)

func (f deleteStoreFunc) Delete(ctx context.Context, id int64) {
	f(ctx, id)
}

func TestDeletePet(t *testing.T) {
	t.Run("will return HTTP 400 with a response body", func(t *testing.T) {
		t.Run("if the path value for param, id, is not set", func(t *testing.T) {
			store := deleteStoreFunc(func(ctx context.Context, i int64) {})

			e := DeletePet(store)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodDelete, "/pet/", nil)

			e.ServeHTTP(w, r)

			resp := w.Result()
			if !assert.NotNil(t, resp) {
				return
			}
			if !assert.Equal(t, http.StatusBadRequest, resp.StatusCode) {
				return
			}
			defer resp.Body.Close()

			b, err := io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}

			var status humuspb.Status
			err = proto.Unmarshal(b, &status)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, humuspb.Code_FAILED_PRECONDITION, status.Code) {
				return
			}
		})

		t.Run("if the path value for param, id, is not an integer", func(t *testing.T) {
			store := deleteStoreFunc(func(ctx context.Context, i int64) {})

			e := DeletePet(store)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodDelete, "/pet/abc", nil)
			r.SetPathValue("id", "abc")

			e.ServeHTTP(w, r)

			resp := w.Result()
			if !assert.NotNil(t, resp) {
				return
			}
			if !assert.Equal(t, http.StatusBadRequest, resp.StatusCode) {
				return
			}
			defer resp.Body.Close()

			b, err := io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}

			var status humuspb.Status
			err = proto.Unmarshal(b, &status)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, humuspb.Code_INVALID_ARGUMENT, status.Code) {
				return
			}
		})
	})

	t.Run("will return HTTP 200 without response body", func(t *testing.T) {
		t.Run("if it successfully deletes pet from store", func(t *testing.T) {
			deletedPetId := int64(0)
			store := deleteStoreFunc(func(ctx context.Context, i int64) {
				deletedPetId = i
			})

			e := DeletePet(store)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodDelete, "/pet/123", nil)
			r.SetPathValue("id", "123")

			e.ServeHTTP(w, r)

			resp := w.Result()
			if !assert.NotNil(t, resp) {
				return
			}
			if !assert.Equal(t, http.StatusOK, resp.StatusCode) {
				return
			}
			defer resp.Body.Close()

			b, err := io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Empty(t, b) {
				return
			}

			if !assert.Equal(t, int64(123), deletedPetId) {
				return
			}
		})
	})
}
