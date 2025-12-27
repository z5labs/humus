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
	"time"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type StmtPreparer interface {
	Prepare(query string) (*sql.Stmt, error)
}

type adoptPetHandler struct {
	tracer       trace.Tracer
	log          *slog.Logger
	adoptPetStmt *sql.Stmt
}

func AdoptPet(ctx context.Context, db StmtPreparer, h *app.HookRegistry) rest.ApiOption {
	stmt, err := db.Prepare("")
	if err != nil {
		panic(err)
	}

	h.OnPostRun(func(ctx context.Context) error {
		return stmt.Close()
	})

	handler := &adoptPetHandler{
		tracer:       otel.Tracer("github.com/z5labs/humus/example/rest/petstore/endpoint"),
		log:          humus.Logger("github.com/z5labs/humus/example/rest/petstore/endpoint"),
		adoptPetStmt: stmt,
	}

	return rest.Operation(
		http.MethodPost,
		rest.BasePath("/pet").Param("id", rest.Required()),
		rest.HandleJson(handler),
	)
}

type AdoptPetRequest struct {
	Appointment time.Time `config:"appointment"`
}

type AdoptPetResponse struct{}

func (h *adoptPetHandler) Handle(ctx context.Context, req *AdoptPetRequest) (*AdoptPetResponse, error) {
	return nil, nil
}
