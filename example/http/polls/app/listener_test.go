// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/humustest"
)

func TestListener_keeps_the_Service_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		cfg, err := pgx.ParseConfig("postgres://polls@" + dbHost + "/polls?sslmode=disable")
		require.NoError(t, err)
		cfg.DialFunc = s.Host.Dial
		cfg.LookupFunc = func(_ context.Context, host string) ([]string, error) {
			return []string{host}, nil
		}
		l := store.NewListener(cfg, store.New(nil), s.Telemetry.LoggerProvider.Logger(loggerName))
		return newListener(l.Run)
	})
}
