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
	"net/textproto"
	"strconv"
	"strings"

	"github.com/z5labs/humus/example/internal/petstorepb"

	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/bedrock/pkg/ptr"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type GetImageIndex interface {
	GetImage(context.Context, int64) ([]byte, bool)
}

type downloadHandler struct {
	store  PetByIdStore
	images GetImageIndex
}

func Download(store PetByIdStore, images GetImageIndex) rest.Endpoint {
	h := &downloadHandler{
		store:  store,
		images: images,
	}

	return rest.NewEndpoint(
		http.MethodGet,
		"/download/{id}",
		rest.ConsumesProto(
			rest.ProducesMultipartFormData(h),
		),
		rest.PathParams(
			rest.PathParam{
				Name:     "id",
				Required: true,
			},
		),
		rest.Returns(http.StatusBadRequest),
	)
}

type DownloadResponse struct {
	pet          *petstorepb.Pet
	imageContent io.Reader
}

func (DownloadResponse) OpenApiV3Schema() (*openapi3.Schema, error) {
	var req rest.ProtoRequest[petstorepb.Pet, *petstorepb.Pet]
	metadataSchema, err := req.OpenApiV3Schema()
	if err != nil {
		return nil, err
	}

	var schema openapi3.Schema
	schema.WithType(openapi3.SchemaTypeObject)
	schema.WithProperties(map[string]openapi3.SchemaOrRef{
		"pet": {
			Schema: metadataSchema,
		},
		"image": {
			Schema: &openapi3.Schema{
				Type:   ptr.Ref(openapi3.SchemaTypeString),
				Format: ptr.Ref("binary"),
			},
		},
	})
	return &schema, nil
}

func (resp *DownloadResponse) WriteParts(mw rest.MultipartWriter) error {
	b, err := proto.Marshal(resp.pet)
	if err != nil {
		return err
	}

	part, err := mw.CreatePart(textproto.MIMEHeader{})
	if err != nil {
		return err
	}

	_, err = io.Copy(part, bytes.NewReader(b))
	if err != nil {
		return err
	}

	part, err = mw.CreatePart(textproto.MIMEHeader{})
	if err != nil {
		return err
	}
	_, err = io.Copy(part, resp.imageContent)
	return err
}

func (h *downloadHandler) Handle(ctx context.Context, req *emptypb.Empty) (*DownloadResponse, error) {
	spanCtx, span := otel.Tracer("endpoint").Start(ctx, "downloadHandler.Handle")
	defer span.End()

	pathId := rest.PathValue(ctx, "id")
	pathId = strings.TrimSpace(pathId)
	if len(pathId) == 0 {
		return nil, rest.Error(http.StatusBadRequest, "missing pet id")
	}

	id, err := strconv.ParseInt(pathId, 10, 64)
	if err != nil {
		span.RecordError(err)
		return nil, rest.Error(http.StatusBadRequest, "pet id must be an integer")
	}

	pet, found := h.store.Get(spanCtx, id)
	if !found {
		return nil, rest.Error(http.StatusNotFound, "")
	}

	resp := &DownloadResponse{
		pet:          pet,
		imageContent: nil,
	}
	return resp, nil
}
