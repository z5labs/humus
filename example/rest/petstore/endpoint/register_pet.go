// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type registerPetHandler struct {
	tracer          trace.Tracer
	log             *slog.Logger
	registerPetStmt *sql.Stmt
}

func RegisterPet(ctx context.Context, db StmtPreparer, h *app.HookRegistry) rest.ApiOption {
	stmt, err := db.Prepare("")
	if err != nil {
		panic(err)
	}

	h.OnPostRun(func(ctx context.Context) error {
		return stmt.Close()
	})

	handler := &registerPetHandler{
		tracer:          otel.Tracer("github.com/z5labs/humus/example/rest/petstore/endpoint"),
		log:             humus.Logger("github.com/z5labs/humus/example/rest/petstore/endpoint"),
		registerPetStmt: stmt,
	}

	return rest.Operation(
		http.MethodPost,
		rest.BasePath("/pets"),
		rest.HandleJson(handler),
	)
}

type PetKind string

const (
	Cat PetKind = "cat"
	Dog PetKind = "dog"
)

type RegisterPetRequest struct {
	Name string  `json:"name"`
	Kind PetKind `json:"kind"`
}

type RegisterPetResponse struct {
	ID string `json:"id"`
}

func (h *registerPetHandler) Handle(ctx context.Context, req *RegisterPetRequest) (*RegisterPetResponse, error) {
	return nil, nil
}
