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
		tracer:       otel.Tracer("github.com/z5labs/humus/example/rest/petstore/endpoint"),
		log:          humus.Logger("github.com/z5labs/humus/example/rest/petstore/endpoint"),
		listPetsStmt: stmt,
	}

	return rest.Operation(
		http.MethodGet,
		rest.BasePath("/pets"),
		rest.ProduceJson(h),
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

func (h *listPetsHandler) Produce(ctx context.Context) (resp *ListPetsResponse, err error) {
	limitStr := rest.QueryParamValue(ctx, "limit")
	limit, parseErr := strconv.Atoi(limitStr[0])
	if parseErr != nil {
		return nil, parseErr
	}

	rows, queryErr := h.listPetsStmt.QueryContext(ctx, limit)
	if queryErr != nil {
		return nil, queryErr
	}
	defer func() {
		closeErr := rows.Close()
		if err == nil {
			err = closeErr
		}
	}()

	var i int
	pets := make(ListPetsResponse, limit)
	for rows.Next() {
		var record struct {
			id   string
			name string
			kind string
		}

		scanErr := rows.Scan(&record.id, &record.name, &record.kind)
		if scanErr != nil {
			return nil, scanErr
		}

		pets[i] = &Pet{
			ID:   record.id,
			Name: record.name,
			Kind: PetKind(record.kind),
		}
		i++
	}

	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, rowsErr
	}

	return &pets, nil
}
