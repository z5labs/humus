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

	"github.com/z5labs/bedrock/lifecycle"
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/rpc"
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

func AdoptPet(ctx context.Context, db StmtPreparer) rest.ApiOption {
	stmt, err := db.Prepare("")
	if err != nil {
		panic(err)
	}

	lc, ok := lifecycle.FromContext(ctx)
	if !ok {
		panic("lifecycle must be present in context")
	}

	lc.OnPostRun(lifecycle.HookFunc(func(ctx context.Context) error {
		return stmt.Close()
	}))

	h := &adoptPetHandler{
		tracer:       otel.Tracer("endpoint"),
		log:          humus.Logger("endpoint"),
		adoptPetStmt: stmt,
	}

	return rest.Handle(
		http.MethodPost,
		rest.BasePath("/pet").Param("id", rest.Required()),
		rpc.ConsumeJson(
			rpc.ReturnJson(h),
		),
	)
}

type AdoptPetRequest struct {
	Appointment time.Time `config:"appointment"`
}

type AdoptPetResponse struct{}

func (h *adoptPetHandler) Handle(ctx context.Context, req *AdoptPetRequest) (*AdoptPetResponse, error) {
	return nil, nil
}
