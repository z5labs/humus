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
	"google.golang.org/protobuf/proto"
)

type listStoreFunc func(context.Context) []*petstorepb.Pet

func (f listStoreFunc) Pets(ctx context.Context) []*petstorepb.Pet {
	return f(ctx)
}

func TestListPets(t *testing.T) {
	t.Run("will return pets", func(t *testing.T) {
		t.Run("if there are pets in the store", func(t *testing.T) {
			pets := []*petstorepb.Pet{
				{Id: 1, Name: "a"},
				{Id: 2, Name: "b"},
			}

			store := listStoreFunc(func(ctx context.Context) []*petstorepb.Pet {
				return pets
			})

			e := ListPets(store)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/pets", nil)

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

			var listResp petstorepb.ListPetsResponse
			err = proto.Unmarshal(b, &listResp)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Len(t, listResp.Pets, len(pets)) {
				return
			}
		})
	})
}
