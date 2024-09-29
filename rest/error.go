// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/z5labs/humus/humuspb"

	"github.com/z5labs/bedrock/rest/endpoint"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// Error
func Error(status int, message string, details ...*anypb.Any) *humuspb.Status {
	return &humuspb.Status{
		Code:    humuspb.HttpCodeToStatusCode(status),
		Message: message,
		Details: details,
	}
}

type errHandler struct {
	marshal func(proto.Message) ([]byte, error)
}

func (h *errHandler) HandleError(ctx context.Context, w http.ResponseWriter, err error) {
	_, span := otel.Tracer("rest").Start(ctx, "errHandler.HandleErr")
	defer span.End()

	status := mapErrorToStatus(err)
	httpCode := humuspb.StatusCodeToHttpCode(status.Code)
	b, err := h.marshal(status)
	if err != nil {
		span.RecordError(err)
		// TODO: should add a log here
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("Content-Type", ProtobufContentType)
	w.WriteHeader(httpCode)
	_, err = io.Copy(w, bytes.NewReader(b))
	if err == nil {
		return
	}

	span.RecordError(err)
	// TODO: should prolly log here
}

func mapErrorToStatus(err error) *humuspb.Status {
	switch e := err.(type) {
	case *humuspb.Status:
		return e
	case endpoint.InvalidHeaderError:
		return &humuspb.Status{
			Code:    humuspb.Code_INVALID_ARGUMENT,
			Message: fmt.Sprintf("invalid header: %s", e.Header),
		}
	case endpoint.InvalidPathParamError:
		return &humuspb.Status{
			Code:    humuspb.Code_INVALID_ARGUMENT,
			Message: fmt.Sprintf("invalid path param: %s", e.Param),
		}
	case endpoint.InvalidQueryParamError:
		return &humuspb.Status{
			Code:    humuspb.Code_INVALID_ARGUMENT,
			Message: fmt.Sprintf("invalid query param: %s", e.Param),
		}
	case endpoint.MissingRequiredHeaderError:
		return &humuspb.Status{
			Code:    humuspb.Code_FAILED_PRECONDITION,
			Message: fmt.Sprintf("missing required header: %s", e.Header),
		}
	case endpoint.MissingRequiredQueryParamError:
		return &humuspb.Status{
			Code:    humuspb.Code_FAILED_PRECONDITION,
			Message: fmt.Sprintf("missing required query param: %s", e.Param),
		}
	default:
		return &humuspb.Status{
			Code:    humuspb.Code_INTERNAL,
			Message: "internal error",
		}
	}
}
