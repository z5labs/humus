package endpoint

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/z5labs/bedrock/lifecycle"
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/rpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type listPetsHandler struct {
	tracer       trace.Tracer
	log          *slog.Logger
	listPetsStmt *sql.Stmt
}

func ListPets(ctx context.Context, db StmtPreparer) rest.ApiOption {
	stmt, err := db.Prepare("SELECT id, name, kind FROM pets LIMIT ?")
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

	h := &listPetsHandler{
		tracer:       otel.Tracer("endpoint"),
		log:          humus.Logger("endpoint"),
		listPetsStmt: stmt,
	}

	return rest.Handle(
		http.MethodGet,
		rest.BasePath("/pets"),
		rpc.ReturnJson(
			rpc.ConsumeNothing(h),
		),
		rest.QueryParam(
			"limit",
			rest.Regex(regexp.MustCompile(`^\d+$`)),
		),
		rest.QueryParam(
			"after",
		),
	)
}

type Pet struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Kind PetKind `json:"kind"`
}

type ListPetsResponse []*Pet

func (h *listPetsHandler) Produce(ctx context.Context) (*ListPetsResponse, error) {
	limitStr := rest.QueryParamValue(ctx, "limit")
	limit, err := strconv.Atoi(limitStr[0])
	if err != nil {
		return nil, err
	}

	rows, err := h.listPetsStmt.QueryContext(ctx, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var i int
	pets := make(ListPetsResponse, limit)
	for rows.Next() {
		var record struct {
			id   string
			name string
			kind string
		}

		err := rows.Scan(&record.id, &record.name, &record.kind)
		if err != nil {
			return nil, err
		}

		pets[i] = &Pet{
			ID:   record.id,
			Name: record.name,
			Kind: PetKind(record.kind),
		}
	}

	return &pets, nil
}
