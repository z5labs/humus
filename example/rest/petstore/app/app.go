package app

import (
	"context"
	"database/sql"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/example/rest/petstore/endpoint"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/z5labs/humus/rest"
)

// BuildApi creates the REST API with all endpoints registered.
// The database connection and hook registry are managed by the caller.
func BuildApi(ctx context.Context, db *sql.DB, h *app.HookRegistry) *rest.Api {
	api := rest.NewApi(
		"Pet Store API",
		"v0.0.0",
		endpoint.AdoptPet(ctx, db, h),
		endpoint.ListPets(ctx, db, h),
		endpoint.RegisterPet(ctx, db, h),
	)

	return api
}

// OpenDatabase opens a PostgreSQL database connection using the provided URL.
func OpenDatabase(ctx context.Context, url string) (*sql.DB, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}

	return db, nil
}
