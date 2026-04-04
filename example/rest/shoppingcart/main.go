// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Shopping cart REST API example demonstrating CRUD operations backed by PostgreSQL.
//
// # Environment Variables
//
//   - DATABASE_URL - PostgreSQL connection string (required)
//     Example: postgres://user:pass@localhost:5432/shoppingcart
//
// All other server settings follow the defaults from github.com/z5labs/humus/rest.
package main

import (
	"context"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/z5labs/humus/example/rest/shoppingcart/app"
	"github.com/z5labs/humus/example/rest/shoppingcart/service/database"
	"github.com/z5labs/humus/rest"
)

func main() {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer pool.Close()

	store := database.New(pool)

	if err := rest.Run(ctx, app.Options(store)...); err != nil {
		log.Fatal(err)
	}
}
