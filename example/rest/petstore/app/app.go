package app

import (
	"context"
	"database/sql"

	"github.com/z5labs/humus/example/rest/petstore/endpoint"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/z5labs/bedrock/lifecycle"
	"github.com/z5labs/humus/rest"
)

type Config struct {
	rest.Config `config:",squash"`

	Postgres struct {
		URL string `config:"url"`
	} `config:"postgres"`
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
	db, err := sql.Open("pgx", cfg.Postgres.URL)
	if err != nil {
		return nil, err
	}

	lc, _ := lifecycle.FromContext(ctx)
	lc.OnPostRun(lifecycle.HookFunc(func(ctx context.Context) error {
		return db.Close()
	}))

	api := rest.NewApi(
		cfg.OpenApi.Title,
		cfg.OpenApi.Version,
		endpoint.AdoptPet(ctx, db),
		endpoint.ListPets(ctx, db),
		endpoint.RegisterPet(ctx, db),
	)

	return api, nil
}
