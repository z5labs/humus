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
	"maps"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/wallet/ledgerpb"
	"github.com/z5labs/humus/example/wallet/server"
	"github.com/z5labs/humus/example/wallet/walletpb"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	walletAddr = "wallet:9443"
	probesAddr = "localhost:8081"
	ledgerAddr = "ledger:9090"
	keysHost   = "keys.internal"
	keysAddr   = keysHost + ":8443"
	keysURL    = "https://" + keysAddr + jwksPath
	unset      = "\x00unset"
)

var required = []string{
	"GRPC_ADDR",
	"GRPC_TLS_PEM_FILE",
	"PROBES_ADDR",
	"LEDGER_ADDR",
	"LEDGER_CA_FILE",
	"LEDGER_TLS_PEM_FILE",
	"TOKEN_KEYS_URL",
	"TOKEN_KEYS_CA_FILE",
	"TOKEN_ISSUER",
	"TOKEN_AUDIENCE",
}

type fakeLedger struct {
	ledgerpb.UnimplementedLedgerServer

	mu       sync.Mutex
	accounts map[string]int64
	err      error
	changed  chan struct{}
	calls    int
	carried  int
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{accounts: map[string]int64{}, changed: make(chan struct{})}
}

func (l *fakeLedger) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

func (l *fakeLedger) called() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func (l *fakeLedger) scenarios() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.carried
}

func (l *fakeLedger) trust(context.Context, string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.carried++
	return true
}

func (l *fakeLedger) deposit(id string, amountCents int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accounts[id] += amountCents
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *fakeLedger) OpenAccount(_ context.Context, req *ledgerpb.OpenAccountRequest) (*ledgerpb.OpenAccountResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	l.accounts[req.GetAccountId()] += 0
	return &ledgerpb.OpenAccountResponse{Account: &ledgerpb.Account{Id: req.GetAccountId(), BalanceCents: l.accounts[req.GetAccountId()]}}, nil
}

func (l *fakeLedger) GetAccount(_ context.Context, req *ledgerpb.GetAccountRequest) (*ledgerpb.GetAccountResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	balance, ok := l.accounts[req.GetAccountId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such account")
	}
	return &ledgerpb.GetAccountResponse{Account: &ledgerpb.Account{Id: req.GetAccountId(), BalanceCents: balance}}, nil
}

func (l *fakeLedger) Transfer(_ context.Context, req *ledgerpb.TransferRequest) (*ledgerpb.TransferResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	l.accounts[req.GetFromAccountId()] -= req.GetAmountCents()
	l.accounts[req.GetToAccountId()] += req.GetAmountCents()
	close(l.changed)
	l.changed = make(chan struct{})
	return &ledgerpb.TransferResponse{Transfer: &ledgerpb.Transfer{
		Id:            req.GetTransferId(),
		FromAccountId: req.GetFromAccountId(),
		ToAccountId:   req.GetToAccountId(),
		AmountCents:   req.GetAmountCents(),
	}}, nil
}

func (l *fakeLedger) WatchAccount(req *ledgerpb.WatchAccountRequest, stream grpc.ServerStreamingServer[ledgerpb.WatchAccountResponse]) error {
	for {
		l.mu.Lock()
		balance, ok := l.accounts[req.GetAccountId()]
		changed := l.changed
		l.mu.Unlock()
		if !ok {
			return status.Error(codes.NotFound, "no such account")
		}
		if err := stream.Send(&ledgerpb.WatchAccountResponse{Account: &ledgerpb.Account{Id: req.GetAccountId(), BalanceCents: balance}}); err != nil {
			return err
		}
		select {
		case <-changed:
		case <-stream.Context().Done():
			return status.FromContextError(stream.Context().Err()).Err()
		}
	}
}

type fixture struct {
	h     *humustest.Host
	ca    *humustest.CA
	idp   *idp
	files map[string][]byte
	env   map[string]string
	tel   telemetry.Builder
}

func newFixture(t *testing.T, files map[string][]byte) *fixture {
	t.Helper()
	ca := humustest.NewCA(t)
	all := map[string][]byte{
		"secrets/wallet.pem":        pemOf(t, ca.Issue(t, "wallet")),
		"secrets/ledger-client.pem": pemOf(t, ca.Issue(t, "wallet")),
		"certs/ledger-ca.pem":       caPEM(t, ca),
		"certs/keys-ca.pem":         caPEM(t, ca),
	}
	maps.Copy(all, files)
	return &fixture{
		h:     humustest.NewHost(t, humustest.WithFiles(all)),
		ca:    ca,
		idp:   newIdP(t),
		files: all,
		tel:   telemetry.NoopBuilder,
		env: map[string]string{
			"GRPC_ADDR":           ":9443",
			"GRPC_TLS_PEM_FILE":   "secrets/wallet.pem",
			"PROBES_ADDR":         ":8081",
			"LEDGER_ADDR":         ledgerAddr,
			"LEDGER_CA_FILE":      "certs/ledger-ca.pem",
			"LEDGER_TLS_PEM_FILE": "secrets/ledger-client.pem",
			"TOKEN_KEYS_URL":      keysURL,
			"TOKEN_KEYS_CA_FILE":  "certs/keys-ca.pem",
			"TOKEN_ISSUER":        issuer,
			"TOKEN_AUDIENCE":      audience,
		},
	}
}

func (f *fixture) start(t *testing.T, env map[string]string) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	merged := maps.Clone(f.env)
	for name, value := range env {
		if value == unset {
			delete(merged, name)
			continue
		}
		merged[name] = value
	}
	return grpcservertest.Start(t, Init,
		humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(f.tel), humus.WithFaults(Faults)),
		humustest.WithEnv(merged),
	)
}

func (f *fixture) startLedger(t *testing.T) (*fakeLedger, func()) {
	t.Helper()
	l := newFakeLedger()
	cert := f.ca.Issue(t, "ledger").TLS()
	p, exit := grpcservertest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: ledgerAddr,
			TLS: &tls.Config{
				Certificates: []tls.Certificate{cert},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    f.ca.Pool(),
				MinVersion:   tls.VersionTLS12,
			},
			TrustScenario: l.trust,
		})
		if err != nil {
			return nil, err
		}
		ledgerpb.RegisterLedgerServer(svc, l)
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder), humus.WithFaults(Faults)))
	<-f.h.Listening(ledgerAddr)

	var once sync.Once
	stopLedger := func() {
		once.Do(func() { stop(t, "the ledger", p, exit) })
	}
	t.Cleanup(func() {
		once.Do(func() { <-exit })
	})
	return l, stopLedger
}

func (f *fixture) startKeys(t *testing.T) {
	t.Helper()
	f.idp.serve(t, f.h.Host(), keysAddr, f.ca.Issue(t, keysHost))
	<-f.h.Listening(keysAddr)
}

type wallet struct {
	f      *fixture
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
	ledger *fakeLedger
}

func start(t *testing.T) *wallet {
	t.Helper()
	f := newFixture(t, nil)
	f.startKeys(t)
	l, _ := f.startLedger(t)
	w := startWallet(t, f)
	w.ledger = l
	return w
}

func startWallet(t *testing.T, f *fixture) *wallet {
	t.Helper()
	p, status := f.start(t, nil)
	for _, addr := range []string{walletAddr, probesAddr} {
		select {
		case <-f.h.Listening(addr):
		case st := <-status:
			t.Fatalf("the wallet exited %d: %v", st.Code(), st.Cause())
		}
	}
	return &wallet{f: f, p: p, status: status}
}

func (w *wallet) client(t *testing.T, opts ...grpc.DialOption) walletpb.WalletClient {
	t.Helper()
	return walletpb.NewWalletClient(grpcservertest.NewClient(t, w.f.h, walletAddr, w.f.ca.Pool(), opts...))
}

func (w *wallet) as(t *testing.T, subject string, scopes ...string) context.Context {
	t.Helper()
	return withToken(t.Context(), w.f.idp.token(t, subject, scopes...))
}

func withToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func (w *wallet) check(t *testing.T, service string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn := grpcservertest.NewClient(t, w.f.h, probesAddr, nil, grpc.WithTransportCredentials(insecure.NewCredentials()))
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: service})
	if status.Code(err) == codes.DeadlineExceeded {
		return false
	}
	require.NoError(t, err)
	return resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
}

func (w *wallet) stop(t *testing.T) {
	t.Helper()
	stop(t, "the wallet", w.p, w.status)
}

func stop(t *testing.T, name string, p *bedrocktest.Process, exit <-chan bedrock.ExitStatus) {
	t.Helper()
	select {
	case st := <-exit:
		require.Failf(t, "exited early", "%s: %v", name, st.Cause())
	case <-p.Notified(syscall.SIGTERM):
	}
	p.Signal(syscall.SIGTERM)
	st := <-exit
	require.Zero(t, st.Code(), "%s: %v", name, st.Cause())
}

func requireCode(t *testing.T, err error, want codes.Code) *status.Status {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "not a status: %v", err)
	require.Equal(t, want, st.Code(), st.Message())
	return st
}

type method struct {
	name  string
	write bool
	call  func(ctx context.Context, c walletpb.WalletClient) error
}

var methods = []method{
	{"OpenAccount", true, func(ctx context.Context, c walletpb.WalletClient) error {
		_, err := c.OpenAccount(ctx, &walletpb.OpenAccountRequest{})
		return err
	}},
	{"GetAccount", false, func(ctx context.Context, c walletpb.WalletClient) error {
		_, err := c.GetAccount(ctx, &walletpb.GetAccountRequest{})
		return err
	}},
	{"Send", true, func(ctx context.Context, c walletpb.WalletClient) error {
		_, err := c.Send(ctx, &walletpb.SendRequest{RequestId: rand.Text(), ToAccountId: "bob", AmountCents: 1})
		return err
	}},
	{"WatchAccount", false, func(ctx context.Context, c walletpb.WalletClient) error {
		watch, err := c.WatchAccount(ctx, &walletpb.WatchAccountRequest{})
		if err != nil {
			return err
		}
		_, err = watch.Recv()
		return err
	}},
}

func TestInit_serves_a_token_with_the_scopes_each_method_needs_through_the_ledger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := start(t)
		c := w.client(t)
		ctx := w.as(t, "alice", server.ScopeRead, server.ScopeWrite)

		_, err := c.OpenAccount(ctx, &walletpb.OpenAccountRequest{})
		require.NoError(t, err)
		w.ledger.deposit("alice", 500)

		got, err := c.GetAccount(ctx, &walletpb.GetAccountRequest{})
		require.NoError(t, err)
		require.Equal(t, "alice", got.GetAccount().GetId())
		require.EqualValues(t, 500, got.GetAccount().GetBalanceCents())

		watch, err := c.WatchAccount(ctx, &walletpb.WatchAccountRequest{})
		require.NoError(t, err)
		seen, err := watch.Recv()
		require.NoError(t, err)
		require.EqualValues(t, 500, seen.GetAccount().GetBalanceCents())

		sent, err := c.Send(ctx, &walletpb.SendRequest{RequestId: "r-1", ToAccountId: "bob", AmountCents: 200})
		require.NoError(t, err)
		require.Equal(t, "alice", sent.GetTransfer().GetFromAccountId())
		require.EqualValues(t, 200, sent.GetTransfer().GetAmountCents())

		seen, err = watch.Recv()
		require.NoError(t, err)
		require.EqualValues(t, 300, seen.GetAccount().GetBalanceCents())

		w.stop(t)
	})
}

func TestInit_answers_a_call_without_a_token_Unauthenticated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := start(t)
		c := w.client(t)

		for _, m := range methods {
			requireCode(t, m.call(t.Context(), c), codes.Unauthenticated)
		}

		w.stop(t)
	})
}

func TestInit_answers_a_token_it_cant_verify_Unauthenticated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := start(t)
		c := w.client(t)
		other := newIdP(t)

		for desc, token := range map[string]string{
			"not a JWT":             rand.Text(),
			"signed by another key": other.token(t, "alice", server.ScopeRead, server.ScopeWrite),
		} {
			for _, m := range methods {
				err := m.call(withToken(t.Context(), token), c)
				require.Equal(t, codes.Unauthenticated, status.Code(err), "%s, on %s: %v", desc, m.name, err)
			}
		}

		w.stop(t)
	})
}

func TestInit_answers_a_token_without_the_scope_a_method_needs_PermissionDenied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := start(t)
		c := w.client(t)
		w.ledger.deposit("alice", 0)

		for _, m := range methods {
			t.Logf("%s", m.name)
			if m.write {
				requireCode(t, m.call(w.as(t, "alice", server.ScopeRead), c), codes.PermissionDenied)
				require.NoError(t, m.call(w.as(t, "alice", server.ScopeWrite), c))
				continue
			}
			requireCode(t, m.call(w.as(t, "alice", server.ScopeWrite), c), codes.PermissionDenied)
			require.NoError(t, m.call(w.as(t, "alice", server.ScopeRead), c))
		}
		for _, m := range methods {
			requireCode(t, m.call(w.as(t, "alice"), c), codes.PermissionDenied)
		}

		w.stop(t)
	})
}

func TestInit_answers_Unavailable_KEYS_UNAVAILABLE_while_the_key_set_cant_be_fetched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := start(t)
		c := w.client(t)
		w.ledger.deposit("alice", 0)
		w.f.idp.setDown(true)

		for _, m := range methods {
			err := m.call(w.as(t, "alice", server.ScopeRead, server.ScopeWrite), c)
			st := requireCode(t, err, codes.Unavailable)
			var reasons []string
			for _, d := range st.Details() {
				if info, ok := d.(*errdetails.ErrorInfo); ok {
					reasons = append(reasons, info.GetReason())
					require.Equal(t, walletpb.Wallet_ServiceDesc.ServiceName, info.GetDomain())
				}
			}
			require.Equal(t, []string{reasonKeysUnavailable}, reasons, "%s: %s", m.name, st.Message())
		}

		w.f.idp.setDown(false)
		time.Sleep(2 * time.Minute)
		_, err := c.GetAccount(w.as(t, "alice", server.ScopeRead), &walletpb.GetAccountRequest{})
		require.NoError(t, err)

		w.stop(t)
	})
}

func TestInit_passes_on_what_the_ledger_answers_about_the_request_without_opening_its_breaker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := start(t)
		c := w.client(t)
		ctx := w.as(t, "alice", server.ScopeRead, server.ScopeWrite)

		for _, code := range []codes.Code{codes.NotFound, codes.FailedPrecondition, codes.InvalidArgument, codes.AlreadyExists} {
			w.ledger.fail(status.Error(code, "the ledger says no"))
			for range 25 {
				_, err := c.GetAccount(ctx, &walletpb.GetAccountRequest{})
				requireCode(t, err, code)
			}
		}

		w.ledger.fail(nil)
		w.ledger.deposit("alice", 100)
		got, err := c.GetAccount(ctx, &walletpb.GetAccountRequest{})
		require.NoError(t, err)
		require.EqualValues(t, 100, got.GetAccount().GetBalanceCents())

		w.stop(t)
	})
}

func TestInit_is_ready_once_it_has_reached_the_ledger_and_stays_ready_through_an_outage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		f.startKeys(t)
		w := startWallet(t, f)

		for range 3 {
			require.False(t, w.check(t, "readiness"))
			require.True(t, w.check(t, "liveness"))
		}

		_, stopLedger := f.startLedger(t)
		ready := false
		for range 300 {
			if ready = w.check(t, "readiness"); ready {
				break
			}
		}
		require.True(t, ready, "the wallet never turned ready")

		stopLedger()
		for range 3 {
			require.True(t, w.check(t, "readiness"))
		}

		w.stop(t)
	})
}

func TestInit_closes_a_connection_after_a_minute_but_leaves_its_open_watch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := start(t)
		var dials atomic.Int64
		c := w.client(t, grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			dials.Add(1)
			return w.f.h.Dial(ctx, "tcp", addr)
		}))
		ctx := w.as(t, "alice", server.ScopeRead, server.ScopeWrite)
		w.ledger.deposit("alice", 500)

		watch, err := c.WatchAccount(ctx, &walletpb.WatchAccountRequest{})
		require.NoError(t, err)
		_, err = watch.Recv()
		require.NoError(t, err)
		require.EqualValues(t, 1, dials.Load())

		time.Sleep(3 * time.Minute)
		_, err = c.Send(ctx, &walletpb.SendRequest{RequestId: "r-1", ToAccountId: "bob", AmountCents: 250})
		require.NoError(t, err)
		require.GreaterOrEqual(t, dials.Load(), int64(2))

		got, err := watch.Recv()
		require.NoError(t, err)
		require.EqualValues(t, 250, got.GetAccount().GetBalanceCents())

		w.stop(t)
	})
}

func TestInit_serves_TLS_without_asking_for_a_client_certificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := start(t)
		asked := make(chan bool, 1)
		conn, err := w.f.h.Dial(t.Context(), "tcp", walletAddr)
		require.NoError(t, err)
		tc := tls.Client(conn, &tls.Config{
			ServerName: "wallet",
			RootCAs:    w.f.ca.Pool(),
			NextProtos: []string{"h2"},
			MinVersion: tls.VersionTLS12,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				asked <- true
				return &tls.Certificate{}, nil
			},
		})
		require.NoError(t, tc.HandshakeContext(t.Context()))
		require.NoError(t, tc.Close())
		require.Empty(t, asked)

		w.stop(t)
	})
}

func TestInit_exits_0_on_SIGTERM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		w := startWallet(t, f)
		w.p.Signal(syscall.SIGTERM)
		st := <-w.status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func exitsConfig(t *testing.T, env map[string]string, files map[string][]byte) error {
	t.Helper()
	var cause error
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, files)
		_, status := f.start(t, env)
		st := <-status
		cause = st.Cause()
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", cause)
	})
	require.Error(t, cause)
	return cause
}

func TestInit_without_a_required_setting_exits_78(t *testing.T) {
	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			cause := exitsConfig(t, map[string]string{name: unset}, nil)
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
		cause := exitsConfig(t, env, nil)
		for _, name := range required {
			require.ErrorContains(t, cause, "config "+name+": not found")
		}
	})
}

func TestInit_with_a_bad_setting_exits_78(t *testing.T) {
	junk := map[string][]byte{"certs/junk.pem": []byte("junk")}
	testCases := map[string]struct {
		name  string
		value string
		files map[string][]byte
		cause string
	}{
		"an address with no port":                 {name: "GRPC_ADDR", value: "wallet", cause: "port"},
		"a named port":                            {name: "GRPC_ADDR", value: ":grpc", cause: "port"},
		"a PEM file that doesn't exist":           {name: "GRPC_TLS_PEM_FILE", value: "secrets/missing.pem", cause: "secrets/missing.pem"},
		"a PEM file with no key":                  {name: "GRPC_TLS_PEM_FILE", value: "certs/ledger-ca.pem", cause: "certs/ledger-ca.pem"},
		"a probes address with no port":           {name: "PROBES_ADDR", value: "localhost", cause: "port"},
		"a ledger address with no port":           {name: "LEDGER_ADDR", value: "ledger", cause: "port"},
		"a ledger address with no host":           {name: "LEDGER_ADDR", value: ":9090", cause: "host"},
		"a ledger CA file that doesn't exist":     {name: "LEDGER_CA_FILE", value: "certs/missing.pem", cause: "certs/missing.pem"},
		"a ledger CA file with no certificate":    {name: "LEDGER_CA_FILE", value: "certs/junk.pem", files: junk, cause: "no PEM certificate in the file"},
		"a client PEM file that doesn't exist":    {name: "LEDGER_TLS_PEM_FILE", value: "secrets/missing.pem", cause: "secrets/missing.pem"},
		"a client PEM file with no key":           {name: "LEDGER_TLS_PEM_FILE", value: "certs/ledger-ca.pem", cause: "certs/ledger-ca.pem"},
		"a ledger timeout of 0":                   {name: "LEDGER_TIMEOUT", value: "0s", cause: "positive"},
		"a ledger timeout that isn't a duration":  {name: "LEDGER_TIMEOUT", value: "soon", cause: "invalid duration"},
		"no attempts at the ledger":               {name: "LEDGER_MAX_ATTEMPTS", value: "0", cause: "1"},
		"ledger attempts that aren't a number":    {name: "LEDGER_MAX_ATTEMPTS", value: "three", cause: "invalid syntax"},
		"a key set URL that doesn't parse":        {name: "TOKEN_KEYS_URL", value: "https://keys.internal/%zz", cause: "must be a URL"},
		"an http key set URL":                     {name: "TOKEN_KEYS_URL", value: "http://keys.internal/jwks.json", cause: "must be an absolute https URL"},
		"a relative key set URL":                  {name: "TOKEN_KEYS_URL", value: "/jwks.json", cause: "must be an absolute https URL"},
		"a key set URL with user info":            {name: "TOKEN_KEYS_URL", value: "https://wallet@keys.internal/jwks.json", cause: "must not hold user info"},
		"a key set URL with a fragment":           {name: "TOKEN_KEYS_URL", value: "https://keys.internal/jwks.json#keys", cause: "must not have a fragment"},
		"a key set CA file that doesn't exist":    {name: "TOKEN_KEYS_CA_FILE", value: "certs/missing.pem", cause: "certs/missing.pem"},
		"a key set CA file with no certificate":   {name: "TOKEN_KEYS_CA_FILE", value: "certs/junk.pem", files: junk, cause: "no PEM certificate in the file"},
		"a key set timeout of 0":                  {name: "TOKEN_KEYS_TIMEOUT", value: "0s", cause: "positive"},
		"a key set timeout that isn't a duration": {name: "TOKEN_KEYS_TIMEOUT", value: "soon", cause: "invalid duration"},
		"no attempts at the key set":              {name: "TOKEN_KEYS_MAX_ATTEMPTS", value: "0", cause: "1"},
		"key set attempts that aren't a number":   {name: "TOKEN_KEYS_MAX_ATTEMPTS", value: "three", cause: "invalid syntax"},
		"an empty issuer":                         {name: "TOKEN_ISSUER", value: "", cause: "empty"},
		"an empty audience":                       {name: "TOKEN_AUDIENCE", value: "", cause: "empty"},
		"two trust scopes":                        {name: "FAULT_TRUST_SCOPE", value: "faults:rehearse wallet:write", cause: "must be one scope"},
		"a negative fault delay":                  {name: "FAULT_MAX_DELAY", value: "-1s", cause: "positive"},
		"a fault delay with no unit":              {name: "FAULT_MAX_DELAY", value: "10", cause: "missing unit"},
	}
	for desc, tc := range testCases {
		t.Run(desc, func(t *testing.T) {
			cause := exitsConfig(t, map[string]string{tc.name: tc.value}, tc.files)
			require.ErrorContains(t, cause, "config "+tc.name+": ")
			require.ErrorContains(t, cause, tc.cause)
		})
	}
}

func TestInit_reads_its_files_only_through_the_Host(t *testing.T) {
	ca := humustest.NewCA(t)
	for name, contents := range map[string][]byte{
		"GRPC_TLS_PEM_FILE":   pemOf(t, ca.Issue(t, "wallet")),
		"LEDGER_CA_FILE":      caPEM(t, ca),
		"LEDGER_TLS_PEM_FILE": pemOf(t, ca.Issue(t, "wallet")),
		"TOKEN_KEYS_CA_FILE":  caPEM(t, ca),
	} {
		t.Run(name, func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "file")
			require.NoError(t, os.WriteFile(outside, contents, 0o600))

			cause := exitsConfig(t, map[string]string{name: outside}, nil)
			require.ErrorContains(t, cause, "config "+name+": ")
		})
	}
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
