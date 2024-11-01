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

	"github.com/z5labs/humus/example/internal/petstorepb"

	"github.com/stretchr/testify/assert"
	"github.com/z5labs/humus/humuspb"
	"google.golang.org/protobuf/proto"
)

type petByIdStoreFunc func(context.Context, int64) (*petstorepb.Pet, bool)

func (f petByIdStoreFunc) Get(ctx context.Context, id int64) (*petstorepb.Pet, bool) {
	return f(ctx, id)
}

func TestFindPetByID(t *testing.T) {
	t.Run("will return HTTP 400 with response body", func(t *testing.T) {
		t.Run("if the path value for param, id, is not set", func(t *testing.T) {
			store := petByIdStoreFunc(func(ctx context.Context, i int64) (*petstorepb.Pet, bool) {
				return nil, false
			})

			e := FindPetByID(store)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/pet/", nil)

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
			if !assert.Equal(t, humuspb.Code_FAILED_PRECONDITION, status.GetCode()) {
				return
			}
		})

		t.Run("if the path value for param, id, is not an integer", func(t *testing.T) {
			store := petByIdStoreFunc(func(ctx context.Context, i int64) (*petstorepb.Pet, bool) {
				return nil, false
			})

			e := FindPetByID(store)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/pet/abc", nil)
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
			if !assert.Equal(t, humuspb.Code_INVALID_ARGUMENT, status.GetCode()) {
				return
			}
		})
	})

	t.Run("will return HTTP 404 with a response body", func(t *testing.T) {
		t.Run("if the pet with given id is not in the store", func(t *testing.T) {
			store := petByIdStoreFunc(func(ctx context.Context, i int64) (*petstorepb.Pet, bool) {
				return nil, false
			})

			e := FindPetByID(store)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/pet/123", nil)
			r.SetPathValue("id", "123")

			e.ServeHTTP(w, r)

			resp := w.Result()
			if !assert.NotNil(t, resp) {
				return
			}
			if !assert.Equal(t, http.StatusNotFound, resp.StatusCode) {
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
			if !assert.Equal(t, humuspb.Code_NOT_FOUND, status.GetCode()) {
				return
			}
		})
	})

	t.Run("will return HTTP 200 with a response body", func(t *testing.T) {
		t.Run("if the pet with given id is in the store", func(t *testing.T) {
			pet := &petstorepb.Pet{Id: 123, Name: "test"}
			store := petByIdStoreFunc(func(ctx context.Context, i int64) (*petstorepb.Pet, bool) {
				return pet, true
			})

			e := FindPetByID(store)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/pet/123", nil)
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

			var findPetByIdResp petstorepb.FindPetByIdResponse
			err = proto.Unmarshal(b, &findPetByIdResp)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.NotNil(t, findPetByIdResp.Pet) {
				return
			}
			if !assert.Equal(t, pet.Id, findPetByIdResp.Pet.Id) {
				return
			}
			if !assert.Equal(t, pet.Name, findPetByIdResp.Pet.Name) {
				return
			}
		})
	})
}
