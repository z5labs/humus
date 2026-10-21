// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/example/wallet/ledger/server"
	"github.com/z5labs/humus/example/wallet/ledger/store"
	"github.com/z5labs/humus/example/wallet/ledgerpb"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	ledgerAddr  = "ledger:9090"
	probesAddr  = "localhost:8081"
	postgresURL = "postgres://ledger@postgres.internal:5432/ledger"
	storePoint  = "postgresql:ledger"
	unset       = "\x00unset"
)

var required = []string{
	"GRPC_ADDR",
	"GRPC_TLS_PEM_FILE",
	"GRPC_CLIENT_CA_FILE",
	"PROBES_ADDR",
	"POSTGRES_URL",
	"POSTGRES_PASSWORD_FILE",
	"POSTGRES_TLS_CA_FILE",
}

type fakeStore struct {
	mu       sync.Mutex
	accounts map[string]store.Account
	down     bool
	pings    int
	cfg      *pgxpool.Config
	faults   fault.Injector
	calls    int
}

func newFakeStore(down bool) *fakeStore {
	return &fakeStore{
		accounts: map[string]store.Account{store.External: {ID: store.External, BalanceChangedAt: time.Now()}},
		down:     down,
	}
}

func (f *fakeStore) opener() OpenStore {
	return func(_ context.Context, s *humus.Setup, cfg *pgxpool.Config) (server.Store, func(context.Context) error, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cfg = cfg
		f.faults = s.Faults
		return f, f.ping, nil
	}
}

func (f *fakeStore) ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pings++
	if f.down {
		return errors.New("PostgreSQL is down")
	}
	return nil
}

func (f *fakeStore) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakeStore) pinged() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pings
}

func (f *fakeStore) inject(ctx context.Context) error {
	f.calls++
	if f.faults.Eval(ctx, storePoint).Abort {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, &fault.InjectedError{Point: storePoint})
	}
	return nil
}

func (f *fakeStore) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeStore) OpenAccount(ctx context.Context, id string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.inject(ctx); err != nil {
		return store.Account{}, err
	}
	a, ok := f.accounts[id]
	if !ok {
		a = store.Account{ID: id, BalanceChangedAt: time.Now()}
		f.accounts[id] = a
	}
	return a, nil
}

func (f *fakeStore) Account(ctx context.Context, id string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.inject(ctx); err != nil {
		return store.Account{}, err
	}
	a, ok := f.accounts[id]
	if !ok {
		return store.Account{}, fmt.Errorf("reading account %s: %w", id, store.ErrNotFound)
	}
	return a, nil
}

func (f *fakeStore) Deposit(ctx context.Context, transferID, accountID string, amountCents int64) (store.Transfer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.inject(ctx); err != nil {
		return store.Transfer{}, err
	}
	a, ok := f.accounts[accountID]
	if !ok {
		return store.Transfer{}, fmt.Errorf("account %s: %w", accountID, store.ErrNotFound)
	}
	t := store.Transfer{ID: transferID, FromAccountID: store.External, ToAccountID: accountID, AmountCents: amountCents, CreatedAt: time.Now()}
	a.BalanceCents += amountCents
	a.BalanceChangedAt = t.CreatedAt
	f.accounts[accountID] = a
	return t, nil
}

func (f *fakeStore) Transfer(context.Context, string, string, string, int64) (store.Transfer, error) {
	return store.Transfer{}, errors.New("not used by these tests")
}

type fixture struct {
	h        *humustest.Host
	ca       *humustest.CA
	password string
	files    map[string][]byte
	env      map[string]string
	tel      telemetry.Builder
}

func newFixture(t *testing.T, files map[string][]byte) fixture {
	t.Helper()
	ca := humustest.NewCA(t)
	password := rand.Text()
	all := map[string][]byte{
		"secrets/ledger.pem":        pemOf(t, ca.Issue(t, "ledger")),
		"certs/clients-ca.pem":      caPEM(t, ca),
		"secrets/postgres-password": []byte(password + "\n"),
		"certs/postgres-ca.pem":     caPEM(t, ca),
	}
	maps.Copy(all, files)
	return fixture{
		h:        humustest.NewHost(t, humustest.WithFiles(all)),
		ca:       ca,
		password: password,
		files:    all,
		tel:      telemetry.NoopBuilder,
		env: map[string]string{
			"GRPC_ADDR":              ":9090",
			"GRPC_TLS_PEM_FILE":      "secrets/ledger.pem",
			"GRPC_CLIENT_CA_FILE":    "certs/clients-ca.pem",
			"PROBES_ADDR":            ":8081",
			"POSTGRES_URL":           postgresURL,
			"POSTGRES_PASSWORD_FILE": "secrets/postgres-password",
			"POSTGRES_TLS_CA_FILE":   "certs/postgres-ca.pem",
		},
	}
}

func (f fixture) start(t *testing.T, init func(context.Context, *humus.Setup) ([]humus.Service, error), env map[string]string) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	merged := maps.Clone(f.env)
	for name, value := range env {
		if value == unset {
			delete(merged, name)
			continue
		}
		merged[name] = value
	}
	return grpcservertest.Start(t, init,
		humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(f.tel), humus.WithFaults(Faults)),
		humustest.WithEnv(merged),
	)
}

type ledger struct {
	f      fixture
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

func start(t *testing.T, init func(context.Context, *humus.Setup) ([]humus.Service, error)) *ledger {
	t.Helper()
	return startOn(t, newFixture(t, nil), init)
}

func startOn(t *testing.T, f fixture, init func(context.Context, *humus.Setup) ([]humus.Service, error)) *ledger {
	t.Helper()
	p, status := f.start(t, init, nil)
	for _, addr := range []string{ledgerAddr, probesAddr} {
		select {
		case <-f.h.Listening(addr):
		case st := <-status:
			t.Fatalf("the ledger exited %d: %v", st.Code(), st.Cause())
		}
	}
	synctest.Wait()
	return &ledger{f: f, p: p, status: status}
}

func onFake(st *fakeStore) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
		return InitWith(ctx, s, st.opener())
	}
}

func (l *ledger) client(t *testing.T, cert *humustest.Certificate, opts ...grpc.DialOption) ledgerpb.LedgerClient {
	t.Helper()
	cfg := &tls.Config{RootCAs: l.f.ca.Pool(), MinVersion: tls.VersionTLS12}
	if cert != nil {
		presented := cert.TLS()
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &presented, nil
		}
	}
	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(cfg))}, opts...)
	return ledgerpb.NewLedgerClient(grpcservertest.NewClient(t, l.f.h, ledgerAddr, l.f.ca.Pool(), opts...))
}

func (l *ledger) wallet(t *testing.T, opts ...grpc.DialOption) ledgerpb.LedgerClient {
	t.Helper()
	return l.client(t, l.f.ca.Issue(t, "wallet"), opts...)
}

func (l *ledger) readiness(t *testing.T) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	return l.check(t, "readiness")
}

func (l *ledger) check(t *testing.T, service string) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	conn := grpcservertest.NewClient(t, l.f.h, probesAddr, nil, grpc.WithTransportCredentials(insecure.NewCredentials()))
	resp, err := healthpb.NewHealthClient(conn).Check(t.Context(), &healthpb.HealthCheckRequest{Service: service})
	require.NoError(t, err)
	return resp.GetStatus()
}

func (l *ledger) stop(t *testing.T) {
	t.Helper()
	<-l.p.Notified(syscall.SIGTERM)
	l.p.Signal(syscall.SIGTERM)
	st := <-l.status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

func requireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "not a status: %v", err)
	require.Equal(t, want, st.Code(), st.Message())
}

func TestInit_serves_a_caller_whose_certificate_the_internal_CA_issued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore(false)
		l := start(t, onFake(st))
		c := l.wallet(t)

		_, err := c.OpenAccount(t.Context(), &ledgerpb.OpenAccountRequest{AccountId: "alice"})
		require.NoError(t, err)
		_, err = c.Deposit(t.Context(), &ledgerpb.DepositRequest{TransferId: "t-1", AccountId: "alice", AmountCents: 500})
		require.NoError(t, err)
		resp, err := c.GetAccount(t.Context(), &ledgerpb.GetAccountRequest{AccountId: "alice"})
		require.NoError(t, err)
		require.EqualValues(t, 500, resp.GetAccount().GetBalanceCents())

		watch, err := c.WatchAccount(t.Context(), &ledgerpb.WatchAccountRequest{AccountId: "alice"})
		require.NoError(t, err)
		got, err := watch.Recv()
		require.NoError(t, err)
		require.EqualValues(t, 500, got.GetAccount().GetBalanceCents())

		l.stop(t)
	})
}

func TestInit_answers_a_call_without_a_client_certificate_Unauthenticated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore(false)
		l := start(t, onFake(st))
		c := l.client(t, nil)

		_, err := c.OpenAccount(t.Context(), &ledgerpb.OpenAccountRequest{AccountId: "alice"})
		requireCode(t, err, codes.Unauthenticated)
		_, err = c.GetAccount(t.Context(), &ledgerpb.GetAccountRequest{AccountId: store.External})
		requireCode(t, err, codes.Unauthenticated)

		watch, err := c.WatchAccount(t.Context(), &ledgerpb.WatchAccountRequest{AccountId: store.External})
		require.NoError(t, err)
		_, err = watch.Recv()
		requireCode(t, err, codes.Unauthenticated)

		_, err = st.Account(t.Context(), "alice")
		require.ErrorIs(t, err, store.ErrNotFound)

		l.stop(t)
	})
}

func TestInit_refuses_a_certificate_another_CA_issued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := start(t, onFake(newFakeStore(false)))
		c := l.client(t, humustest.NewCA(t).Issue(t, "wallet"))

		_, err := c.GetAccount(t.Context(), &ledgerpb.GetAccountRequest{AccountId: store.External})
		requireCode(t, err, codes.Unavailable)

		l.stop(t)
	})
}

func TestInit_closes_a_connection_after_a_minute_but_leaves_its_open_watch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore(false)
		l := start(t, onFake(st))
		var dials atomic.Int64
		c := l.wallet(t, grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			dials.Add(1)
			return l.f.h.Dial(ctx, "tcp", addr)
		}))

		_, err := c.OpenAccount(t.Context(), &ledgerpb.OpenAccountRequest{AccountId: "alice"})
		require.NoError(t, err)
		watch, err := c.WatchAccount(t.Context(), &ledgerpb.WatchAccountRequest{AccountId: "alice"})
		require.NoError(t, err)
		_, err = watch.Recv()
		require.NoError(t, err)
		require.EqualValues(t, 1, dials.Load())

		time.Sleep(3 * time.Minute)
		_, err = c.Deposit(t.Context(), &ledgerpb.DepositRequest{TransferId: "t-1", AccountId: "alice", AmountCents: 250})
		require.NoError(t, err)
		require.GreaterOrEqual(t, dials.Load(), int64(2))

		got, err := watch.Recv()
		require.NoError(t, err)
		require.EqualValues(t, 250, got.GetAccount().GetBalanceCents())

		l.stop(t)
	})
}

func TestInit_is_ready_once_PostgreSQL_is_first_reached_and_stays_ready_through_an_outage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore(true)
		l := start(t, onFake(st))

		for range 2 {
			require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, l.readiness(t))
			require.Equal(t, healthpb.HealthCheckResponse_SERVING, l.check(t, "liveness"))
		}
		require.Equal(t, 2, st.pinged())

		st.setDown(false)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, l.readiness(t))
		require.Equal(t, 3, st.pinged())

		st.setDown(true)
		for range 2 {
			require.Equal(t, healthpb.HealthCheckResponse_SERVING, l.readiness(t))
		}
		require.Equal(t, 3, st.pinged())

		l.stop(t)
	})
}

func TestInit_dials_PostgreSQL_through_the_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := start(t, Init)

		db, err := l.f.h.Listen("tcp", "postgres.internal:5432")
		require.NoError(t, err)
		requested := make(chan []byte, 1)
		go func() {
			conn, err := db.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			b := make([]byte, 8)
			_, _ = conn.Read(b)
			requested <- b
		}()

		require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, l.readiness(t))
		sslRequest := []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}
		require.Equal(t, sslRequest, <-requested)

		require.NoError(t, db.Close())
		l.stop(t)
	})
}

func TestInit_gives_pgx_the_URL_the_password_file_and_the_CA(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore(false)
		l := start(t, onFake(st))

		conn := st.cfg.ConnConfig
		require.Equal(t, "postgres.internal", conn.Host)
		require.EqualValues(t, 5432, conn.Port)
		require.Equal(t, "ledger", conn.User)
		require.Equal(t, l.f.password, conn.Password)
		require.Equal(t, "ledger", conn.Database)
		require.NotNil(t, conn.TLSConfig)
		require.Equal(t, "postgres.internal", conn.TLSConfig.ServerName)
		require.False(t, conn.TLSConfig.InsecureSkipVerify)
		require.Empty(t, conn.Fallbacks)

		l.stop(t)
	})
}

func TestInit_exits_0_on_SIGTERM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := start(t, Init)
		l.p.Signal(syscall.SIGTERM)
		st := <-l.status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func exitsConfig(t *testing.T, env map[string]string, files map[string][]byte) (password string, cause error) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, files)
		password = f.password
		_, status := f.start(t, Init, env)
		st := <-status
		cause = st.Cause()
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", cause)
	})
	require.Error(t, cause)
	return password, cause
}

func TestInit_without_a_required_setting_exits_78(t *testing.T) {
	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			_, cause := exitsConfig(t, map[string]string{name: unset}, nil)
			require.ErrorContains(t, cause, "config "+name+": not found")
			for _, other := range required {
				if other != name {
					require.NotContains(t, cause.Error(), "config "+other+":")
				}
			}
		})
	}

	t.Run("all of them", func(t *testing.T) {
		env := map[string]string{}
		for _, name := range required {
			env[name] = unset
		}
		_, cause := exitsConfig(t, env, nil)
		for _, name := range required {
			require.ErrorContains(t, cause, "config "+name+": not found")
		}
	})
}

func TestInit_with_a_bad_setting_exits_78(t *testing.T) {
	password := rand.Text()
	testCases := map[string]struct {
		name   string
		value  string
		files  map[string][]byte
		cause  string
		secret string
	}{
		"an address with no port":                  {name: "GRPC_ADDR", value: "ledger", cause: "port"},
		"a named port":                             {name: "GRPC_ADDR", value: ":grpc", cause: "port"},
		"a PEM file that doesn't exist":            {name: "GRPC_TLS_PEM_FILE", value: "secrets/missing.pem", cause: "secrets/missing.pem"},
		"a PEM file with no key":                   {name: "GRPC_TLS_PEM_FILE", value: "certs/clients-ca.pem", cause: "certs/clients-ca.pem"},
		"a client CA file that doesn't exist":      {name: "GRPC_CLIENT_CA_FILE", value: "certs/missing.pem", cause: "certs/missing.pem"},
		"a client CA file with no certificate":     {name: "GRPC_CLIENT_CA_FILE", value: "certs/junk.pem", files: map[string][]byte{"certs/junk.pem": []byte("junk")}, cause: "no PEM certificate in the file"},
		"a probes address with no port":            {name: "PROBES_ADDR", value: "localhost", cause: "port"},
		"a PostgreSQL URL with a password":         {name: "POSTGRES_URL", value: "postgres://ledger:" + password + "@postgres.internal:5432/ledger", cause: "must not hold a password", secret: password},
		"a PostgreSQL URL that doesn't parse":      {name: "POSTGRES_URL", value: "postgres://ledger:" + password + "%zz@postgres.internal:5432/ledger", cause: "must be a URL", secret: password},
		"a MySQL URL":                              {name: "POSTGRES_URL", value: "mysql://ledger@postgres.internal:5432/ledger", cause: "must be a postgres URL"},
		"a PostgreSQL URL with no port":            {name: "POSTGRES_URL", value: "postgres://ledger@postgres.internal/ledger", cause: "must have a host and a port"},
		"a PostgreSQL URL with no user":            {name: "POSTGRES_URL", value: "postgres://postgres.internal:5432/ledger", cause: "must have a user"},
		"a PostgreSQL URL with no database":        {name: "POSTGRES_URL", value: "postgres://ledger@postgres.internal:5432/", cause: "must name a database"},
		"a PostgreSQL URL with its own sslmode":    {name: "POSTGRES_URL", value: postgresURL + "?sslmode=disable", cause: "must not set sslmode"},
		"a PostgreSQL URL with a root certificate": {name: "POSTGRES_URL", value: postgresURL + "?sslrootcert=/etc/ssl/ca.pem", cause: "must not set sslrootcert"},
		"a PostgreSQL URL with a client key":       {name: "POSTGRES_URL", value: postgresURL + "?sslkey=/etc/ssl/key.pem", cause: "must not set sslkey"},
		"a PostgreSQL URL with a passfile":         {name: "POSTGRES_URL", value: postgresURL + "?passfile=/root/.pgpass", cause: "must not set passfile"},
		"a PostgreSQL URL with a service":          {name: "POSTGRES_URL", value: postgresURL + "?service=ledger", cause: "must not set service"},
		"a PostgreSQL URL with a password param":   {name: "POSTGRES_URL", value: postgresURL + "?password=" + password, cause: "must not set password", secret: password},
		"a password file that doesn't exist":       {name: "POSTGRES_PASSWORD_FILE", value: "secrets/missing", cause: "secrets/missing"},
		"an empty password file":                   {name: "POSTGRES_PASSWORD_FILE", value: "secrets/empty", files: map[string][]byte{"secrets/empty": []byte("\n")}, cause: "the file is empty"},
		"a PostgreSQL CA file that doesn't exist":  {name: "POSTGRES_TLS_CA_FILE", value: "certs/missing.pem", cause: "certs/missing.pem"},
		"a PostgreSQL CA file with no certificate": {name: "POSTGRES_TLS_CA_FILE", value: "certs/junk.pem", files: map[string][]byte{"certs/junk.pem": []byte("junk")}, cause: "no PEM certificate in the file"},
		"a watch interval of 0":                    {name: "WATCH_INTERVAL", value: "0s", cause: "must be positive"},
		"a negative watch interval":                {name: "WATCH_INTERVAL", value: "-1s", cause: "must be positive"},
		"a watch interval with no unit":            {name: "WATCH_INTERVAL", value: "1", cause: "missing unit"},
		"a watch interval that isn't a duration":   {name: "WATCH_INTERVAL", value: "often", cause: "invalid duration"},
		"a negative fault delay":                   {name: "FAULT_MAX_DELAY", value: "-1s", cause: "positive"},
		"a fault delay with no unit":               {name: "FAULT_MAX_DELAY", value: "10", cause: "missing unit"},
	}
	for desc, tc := range testCases {
		t.Run(desc, func(t *testing.T) {
			filePassword, cause := exitsConfig(t, map[string]string{tc.name: tc.value}, tc.files)
			require.ErrorContains(t, cause, "config "+tc.name+": ")
			require.ErrorContains(t, cause, tc.cause)
			require.NotContains(t, cause.Error(), filePassword)
			if tc.secret != "" {
				require.NotContains(t, cause.Error(), tc.secret)
			}
		})
	}
}

func TestInit_reads_its_files_only_through_the_Host(t *testing.T) {
	ca := humustest.NewCA(t)
	for name, contents := range map[string][]byte{
		"GRPC_TLS_PEM_FILE":      pemOf(t, ca.Issue(t, "ledger")),
		"GRPC_CLIENT_CA_FILE":    caPEM(t, ca),
		"POSTGRES_PASSWORD_FILE": []byte(rand.Text()),
		"POSTGRES_TLS_CA_FILE":   caPEM(t, ca),
	} {
		t.Run(name, func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "file")
			require.NoError(t, os.WriteFile(outside, contents, 0o600))

			_, cause := exitsConfig(t, map[string]string{name: outside}, nil)
			require.ErrorContains(t, cause, "config "+name+": ")
		})
	}
}

func TestPoolConfig_leaves_nothing_for_pgx_to_read(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("PGPASSWORD", rand.Text())
	t.Setenv("PGPASSFILE", missing)
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGSSLROOTCERT", missing)
	t.Setenv("PGSSLCERT", missing)
	t.Setenv("PGSSLKEY", missing)
	t.Setenv("PGHOST", "elsewhere.internal")
	t.Setenv("PGDATABASE", "elsewhere")

	roots := humustest.NewCA(t).Pool()
	u, err := url.Parse(postgresURL + "?application_name=ledger")
	require.NoError(t, err)
	password := rand.Text()

	cfg, err := poolConfig(postgres{URL: u, Password: password, CA: roots})
	require.NoError(t, err)

	conn := cfg.ConnConfig
	require.Equal(t, "postgres.internal", conn.Host)
	require.Equal(t, uint16(5432), conn.Port)
	require.Equal(t, "ledger", conn.User)
	require.Equal(t, password, conn.Password)
	require.Equal(t, "ledger", conn.Database)
	require.Equal(t, "ledger", conn.RuntimeParams["application_name"])
	require.Empty(t, conn.Fallbacks)
	require.NotNil(t, conn.TLSConfig)
	require.Same(t, roots, conn.TLSConfig.RootCAs)
	require.Equal(t, "postgres.internal", conn.TLSConfig.ServerName)
	require.False(t, conn.TLSConfig.InsecureSkipVerify)
	require.Empty(t, conn.TLSConfig.Certificates)
}

func TestSubject_is_the_verified_certificates_subject(t *testing.T) {
	leaf, err := x509.ParseCertificate(humustest.NewCA(t).Issue(t, "wallet").TLS().Certificate[0])
	require.NoError(t, err)

	got, err := subject(t.Context(), [][]*x509.Certificate{{leaf}})
	require.NoError(t, err)
	require.Equal(t, leaf.Subject, got)

	_, err = subject(t.Context(), nil)
	require.ErrorIs(t, err, auth.ErrUnauthenticated)
}

func pemOf(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var out []byte
	for _, der := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}

func caPEM(t *testing.T, ca *humustest.CA) []byte {
	t.Helper()
	c := ca.Issue(t, "unused").TLS()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[len(c.Certificate)-1]})
}
