// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/rpc"

	"github.com/google/uuid"
	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/sdk-go/ptr"
	"github.com/z5labs/sdk-go/try"
)

type registerPetHandler struct{}

func RegisterPet(api *rest.Api) {
	h := &registerPetHandler{}

	api.Operation(
		http.MethodPost,
		rest.StaticPath("/pet"),
		rpc.NewOperation(h),
	)
}

type RegisterPetRequest struct {
	Name string `json:"name"`
}

func (req *RegisterPetRequest) Spec() (*openapi3.RequestBody, error) {
	def := &openapi3.RequestBody{
		Content: map[string]openapi3.MediaType{
			"application/json": {
				Schema: &openapi3.SchemaOrRef{
					Schema: &openapi3.Schema{
						Type: ptr.Ref(openapi3.SchemaTypeObject),
						Properties: map[string]openapi3.SchemaOrRef{
							"name": {
								Schema: &openapi3.Schema{
									Type: ptr.Ref(openapi3.SchemaTypeString),
								},
							},
						},
					},
				},
			},
		},
	}
	return def, nil
}

func (req *RegisterPetRequest) ReadRequest(ctx context.Context, r *http.Request) (err error) {
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		return rpc.InvalidContentTypeError{
			ContentType: ct,
		}
	}
	defer try.Close(&err, r.Body)

	dec := json.NewDecoder(r.Body)
	return dec.Decode(req)
}

type RegisterPetResponse struct {
	Id           string    `json:"id"`
	Name         string    `json:"name"`
	RegisteredAt time.Time `json:"registered_at"`
}

func (resp *RegisterPetResponse) Spec() (int, *openapi3.Response, error) {
	def := &openapi3.Response{
		Content: map[string]openapi3.MediaType{
			"application/json": {
				Schema: &openapi3.SchemaOrRef{
					Schema: &openapi3.Schema{
						Type: ptr.Ref(openapi3.SchemaTypeObject),
						Properties: map[string]openapi3.SchemaOrRef{
							"id": {
								Schema: &openapi3.Schema{
									Type: ptr.Ref(openapi3.SchemaTypeString),
								},
							},
							"name": {
								Schema: &openapi3.Schema{
									Type: ptr.Ref(openapi3.SchemaTypeString),
								},
							},
							"registered_at": {
								Schema: &openapi3.Schema{
									Type: ptr.Ref(openapi3.SchemaTypeString),
								},
							},
						},
					},
				},
			},
		},
	}
	return http.StatusOK, def, nil
}

func (resp *RegisterPetResponse) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	return enc.Encode(resp)
}

func (h *registerPetHandler) Handle(ctx context.Context, req *RegisterPetRequest) (*RegisterPetResponse, error) {
	resp := &RegisterPetResponse{
		Id:           uuid.New().String(),
		Name:         req.Name,
		RegisteredAt: time.Now(),
	}
	return resp, nil
}
