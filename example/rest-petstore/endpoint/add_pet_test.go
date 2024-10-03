// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/z5labs/humus/example/internal/petstorepb"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/proto"
)

type addStoreFunc func(context.Context, *petstorepb.Pet)

func (f addStoreFunc) Add(ctx context.Context, pet *petstorepb.Pet) {
	f(ctx, pet)
}

func TestAddPet(t *testing.T) {
	t.Run("will save pet with random id", func(t *testing.T) {
		t.Run("if the id is zero", func(t *testing.T) {
			petId := int64(0)
			store := addStoreFunc(func(ctx context.Context, p *petstorepb.Pet) {
				petId = p.Id
			})

			req := &petstorepb.AddPetRequest{
				Pet: &petstorepb.Pet{
					Id:   0,
					Name: "test",
				},
			}
			b, err := proto.Marshal(req)
			if !assert.Nil(t, err) {
				return
			}

			e := AddPet(store)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/pet", bytes.NewReader(b))

			e.ServeHTTP(w, r)

			resp := w.Result()
			if !assert.NotNil(t, resp) {
				return
			}
			if !assert.Equal(t, http.StatusOK, resp.StatusCode) {
				return
			}
			defer resp.Body.Close()

			b, err = io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}

			var addPetResp petstorepb.AddPetResponse
			err = proto.Unmarshal(b, &addPetResp)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.NotNil(t, addPetResp.Pet) {
				return
			}
			if !assert.Equal(t, petId, addPetResp.Pet.Id) {
				return
			}
		})
	})

	t.Run("will save pet with non-random id", func(t *testing.T) {
		t.Run("if the id is non-zero", func(t *testing.T) {
			petId := int64(0)
			store := addStoreFunc(func(ctx context.Context, p *petstorepb.Pet) {
				petId = p.Id
			})

			req := &petstorepb.AddPetRequest{
				Pet: &petstorepb.Pet{
					Id:   5,
					Name: "test",
				},
			}
			b, err := proto.Marshal(req)
			if !assert.Nil(t, err) {
				return
			}

			e := AddPet(store)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/pet", bytes.NewReader(b))

			e.ServeHTTP(w, r)

			resp := w.Result()
			if !assert.NotNil(t, resp) {
				return
			}
			if !assert.Equal(t, http.StatusOK, resp.StatusCode) {
				return
			}
			defer resp.Body.Close()

			b, err = io.ReadAll(resp.Body)
			if !assert.Nil(t, err) {
				return
			}

			var addPetResp petstorepb.AddPetResponse
			err = proto.Unmarshal(b, &addPetResp)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.NotNil(t, addPetResp.Pet) {
				return
			}
			if !assert.Equal(t, petId, addPetResp.Pet.Id) {
				return
			}
		})
	})
}
