// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package postgres_test

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"

	"github.com/z5labs/humus/example/databases/postgres"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
)

func TestServer_an_abort_fails_before_reaching_the_server(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://admin@127.0.0.1:1/postgres?connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	fs := fault.DefaultSettings()
	fs.Decoder = codec
	faults, err := fault.New(fs)
	require.NoError(t, err)
	var admin postgres.Admin = postgres.New(pool, faults)
	password := rand.Text()

	testCases := []struct {
		point string
		call  func(ctx context.Context) error
	}{
		{"postgresql:provision", func(ctx context.Context) error { return admin.Provision(ctx, "orders", password) }},
		{"postgresql:drop", func(ctx context.Context) error { return admin.Drop(ctx, "orders") }},
	}
	for _, tc := range testCases {
		t.Run(tc.point, func(t *testing.T) {
			ctx, err := fit.ContextWithScenario(t.Context(), codec, fit.Scenario{Rules: []fit.Rule{
				{Point: tc.point, Fault: fit.Fault{Abort: true}},
			}})
			require.NoError(t, err)

			err = tc.call(ctx)

			var injected *fault.InjectedError
			require.ErrorAs(t, err, &injected)
			require.Equal(t, tc.point, injected.Point)
			require.ErrorIs(t, err, fit.ErrInjected)
			require.NotContains(t, err.Error(), password)
			require.Zero(t, pool.Stat().TotalConns())
		})
	}
}
