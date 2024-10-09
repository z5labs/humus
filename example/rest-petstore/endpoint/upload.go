// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/z5labs/humus/example/internal/petstorepb"
	"go.opentelemetry.io/otel"

	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/bedrock/pkg/ptr"
	"github.com/z5labs/humus/rest"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type AddImageIndex interface {
	IndexImage(context.Context, *petstorepb.Pet, io.Reader) error
}

type uploadHandler struct {
	store  AddStore
	images AddImageIndex
}

func Upload(store AddStore, images AddImageIndex) rest.Endpoint {
	h := &uploadHandler{
		store:  store,
		images: images,
	}

	return rest.NewEndpoint(
		http.MethodPost,
		"/upload",
		rest.ConsumesMultipartFormData[UploadSchema](
			rest.ProducesProto(h),
		),
	)
}

type UploadSchema struct{}

func (UploadSchema) OpenApiV3Schema() (*openapi3.Schema, error) {
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

func (h *uploadHandler) Handle(ctx context.Context, req *multipart.Reader) (*emptypb.Empty, error) {
	spanCtx, span := otel.Tracer("endpoint").Start(ctx, "uploadHandler.Handle")
	defer span.End()

	var pet petstorepb.Pet
	var imageContents bytes.Buffer
	for {
		part, err := req.NextPart()
		if err != nil && err != io.EOF {
			return nil, err
		}
		if part == nil && err == io.EOF {
			break
		}

		switch formName := part.FormName(); formName {
		case "pet":
			b, err := io.ReadAll(part)
			if err != nil {
				return nil, err
			}
			err = proto.Unmarshal(b, &pet)
			if err != nil {
				return nil, err
			}
		case "image":
			_, err = io.Copy(&imageContents, part)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown form name: %s", formName)
		}
	}

	h.store.Add(spanCtx, &pet)

	err := h.images.IndexImage(spanCtx, &pet, &imageContents)
	if err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}
