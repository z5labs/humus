// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package migrate_test

import (
	"crypto/rand"
	"errors"
	"maps"
	"net"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/shoppingcart/migrate"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

type target struct {
	host     *humustest.Host
	env      map[string]string
	password string
}

func newTarget(t *testing.T) *target {
	t.Helper()
	tg := &target{password: rand.Text()}
	tg.host = humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"secrets/database-url": []byte("postgres://carts_owner:" + tg.password + "@db.internal:5432/carts?sslmode=disable\n"),
		"secrets/bad-url":      []byte("postgres://carts_owner:" + tg.password + "@db.internal:port/carts"),
	}))
	tg.env = map[string]string{
		"DATABASE_URL_FILE": "secrets/database-url",
		"MIGRATIONS":        "carts",
		"APP_ROLE":          "carts",
	}
	return tg
}

func (tg *target) run(t *testing.T, env map[string]string) bedrock.ExitStatus {
	t.Helper()
	all := maps.Clone(tg.env)
	for name, value := range env {
		if value == "" {
			delete(all, name)
			continue
		}
		all[name] = value
	}
	_, status := humustest.Start(t, migrate.Init,
		humustest.WithApp(humus.WithHost(tg.host.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(all),
	)
	return <-status
}

func TestInit_a_bad_setting_exits_78(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"DATABASE_URL_FILE missing":    {map[string]string{"DATABASE_URL_FILE": ""}, "config DATABASE_URL_FILE: not found"},
		"DATABASE_URL_FILE not found":  {map[string]string{"DATABASE_URL_FILE": "secrets/nowhere"}, "config DATABASE_URL_FILE:"},
		"DATABASE_URL_FILE not a URL":  {map[string]string{"DATABASE_URL_FILE": "secrets/bad-url"}, "config DATABASE_URL_FILE: must hold a PostgreSQL URL"},
		"MIGRATIONS missing":           {map[string]string{"MIGRATIONS": ""}, "config MIGRATIONS: not found"},
		"MIGRATIONS neither":           {map[string]string{"MIGRATIONS": "orders"}, "config MIGRATIONS: must be carts or catalog"},
		"MIGRATIONS not in lower case": {map[string]string{"MIGRATIONS": "Carts"}, "config MIGRATIONS: must be carts or catalog"},
		"APP_ROLE missing":             {map[string]string{"APP_ROLE": ""}, "config APP_ROLE: not found"},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tg := newTarget(t)

				st := tg.run(t, tc.env)

				require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
				require.ErrorContains(t, st.Cause(), tc.want)
				require.NotContains(t, st.Cause().Error(), tg.password)
			})
		})
	}
}

func TestInit_without_its_settings_exits_78_naming_each(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, migrate.Init,
			humustest.WithApp(humus.WithHost(humustest.NewHost(t).Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		for _, name := range []string{"DATABASE_URL_FILE", "MIGRATIONS", "APP_ROLE"} {
			require.ErrorContains(t, st.Cause(), "config "+name+": not found")
		}
	})
}

func TestInit_dials_PostgreSQL_through_the_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tg := newTarget(t)
		l, err := tg.host.Listen("tcp", "db.internal:5432")
		require.NoError(t, err)
		var (
			mu      sync.Mutex
			dialled int
		)
		accepted := make(chan struct{})
		go func() {
			defer close(accepted)
			for {
				conn, err := l.Accept()
				if errors.Is(err, net.ErrClosed) {
					return
				}
				if err != nil {
					continue
				}
				mu.Lock()
				dialled++
				mu.Unlock()
				_ = conn.Close()
			}
		}()

		st := tg.run(t, nil)

		require.NoError(t, l.Close())
		<-accepted
		require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
		require.ErrorContains(t, st.Cause(), "connecting to PostgreSQL")
		mu.Lock()
		defer mu.Unlock()
		require.Positive(t, dialled)
	})
}
