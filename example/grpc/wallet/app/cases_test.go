// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/grpc/wallet/cases"
	ledgerapp "github.com/z5labs/humus/example/grpc/wallet/ledger/app"
	"github.com/z5labs/humus/example/grpc/wallet/ledgerpb"
	"github.com/z5labs/humus/example/grpc/wallet/walletpb"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	trustScope     = "faults:rehearse"
	ledgerReplicas = 2
	pollReady      = 100 * time.Millisecond
)

type dialFunc = func(ctx context.Context, network, address string) (net.Conn, error)

type network struct {
	base dialFunc

	mu       sync.Mutex
	services map[string]*service
}

func newNetwork(base dialFunc) *network {
	return &network{base: base, services: map[string]*service{}}
}

func (n *network) service(addr string) *service {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := &service{addr: addr}
	n.services[addr] = s
	return s
}

func (n *network) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	s := n.services[address]
	n.mu.Unlock()
	if s == nil {
		return n.base(ctx, network, address)
	}
	endpoint, ok := s.pick()
	if !ok {
		return nil, &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	return n.base(ctx, network, endpoint)
}

type service struct {
	addr string

	mu        sync.Mutex
	replicas  []*replica
	endpoints []string
	next      int
}

func (s *service) pick() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.endpoints) == 0 {
		return "", false
	}
	endpoint := s.endpoints[s.next%len(s.endpoints)]
	s.next++
	return endpoint, true
}

func (s *service) add(r *replica) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoints = append(s.endpoints, r.addr)
}

func (s *service) remove(r *replica) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoints = slices.DeleteFunc(s.endpoints, func(e string) bool { return e == r.addr })
}

func (s *service) run(ctx context.Context, r *replica) error {
	s.mu.Lock()
	s.replicas = append(s.replicas, r)
	s.mu.Unlock()
	r.run()
	if err := r.ready(ctx); err != nil {
		return err
	}
	s.add(r)
	return nil
}

func (s *service) rollout(ctx context.Context) error {
	s.mu.Lock()
	replicas := slices.Clone(s.replicas)
	s.mu.Unlock()
	for _, r := range replicas {
		s.remove(r)
		if err := r.stop(ctx); err != nil {
			return err
		}
		r.run()
		if err := r.ready(ctx); err != nil {
			return err
		}
		s.add(r)
	}
	return nil
}

func (s *service) stop(ctx context.Context) error {
	s.mu.Lock()
	replicas := slices.Clone(s.replicas)
	s.mu.Unlock()
	var errs []error
	for _, r := range replicas {
		s.remove(r)
		errs = append(errs, r.stop(ctx))
	}
	return errors.Join(errs...)
}

type replica struct {
	name  string
	addr  string
	start func() (*bedrocktest.Process, <-chan bedrock.ExitStatus)
	ready func(ctx context.Context) error

	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

func (r *replica) run() {
	r.p, r.status = r.start()
}

func (r *replica) stop(ctx context.Context) error {
	select {
	case st := <-r.status:
		return fmt.Errorf("%s exited %d before it was stopped: %w", r.name, st.Code(), st.Cause())
	case <-r.p.Notified(syscall.SIGTERM):
	case <-ctx.Done():
		return fmt.Errorf("stopping %s: %w", r.name, context.Cause(ctx))
	}
	r.p.Signal(syscall.SIGTERM)
	select {
	case st := <-r.status:
		if st.Code() != 0 {
			return fmt.Errorf("%s exited %d: %w", r.name, st.Code(), st.Cause())
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for %s to exit: %w", r.name, context.Cause(ctx))
	}
}

type place struct {
	host     humus.Host
	dir      string
	addr     func(name string) string
	ledger   ledgerapp.OpenStore
	postgres func(ca *humustest.CA, file func(name string, contents []byte) string) map[string]string
	timeout  time.Duration
}

type harness struct {
	target cases.Target
	wallet *service
	ledger *service
	keys   *service
}

func inSynctest(t *testing.T) place {
	h := humustest.NewHost(t)
	memory := newMemoryLedger()
	return place{
		host:   h.Host(),
		addr:   func(name string) string { return name },
		ledger: memory.open,
		postgres: func(_ *humustest.CA, file func(string, []byte) string) map[string]string {
			return map[string]string{
				"POSTGRES_URL":           "postgres://ledger@postgres.internal:5432/ledger",
				"POSTGRES_PASSWORD_FILE": file("secrets/postgres-password", []byte(rand.Text()+"\n")),
			}
		},
		timeout: time.Minute,
	}
}

func startHarness(t *testing.T, pl place) *harness {
	t.Helper()
	return startHarnessWith(t, pl, telemetry.NoopBuilder, telemetry.NoopBuilder)
}

func startHarnessWith(t *testing.T, pl place, walletTel, ledgerTel telemetry.Builder) *harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), pl.timeout)
	defer cancel()

	file := func(name string, contents []byte) string {
		return writeHostFile(t, pl.host.FS, filepath.Join(pl.dir, name), contents)
	}
	ca := humustest.NewCA(t)
	caFile := file("certs/ca.pem", caPEM(t, ca))

	n := newNetwork(pl.host.Dial)
	host := pl.host
	host.Dial = n.Dial
	walletApp := humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(walletTel), humus.WithFaults(Faults))
	ledgerApp := humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(ledgerTel), humus.WithFaults(ledgerapp.Faults))

	provider := newIdP(t)
	keysCert := ca.Issue(t, keysHost)
	keys := n.service(keysAddr)
	keysReplica := &replica{name: "the key set", addr: pl.addr("keys-0:8443")}
	keysReplica.start = func() (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
		return provider.serve(t, host, keysReplica.addr, keysCert)
	}
	keysReplica.ready = func(ctx context.Context) error { return dialable(ctx, pl.host.Dial, keysReplica) }
	require.NoError(t, keys.run(ctx, keysReplica))

	ledgerEnv := map[string]string{
		"GRPC_TLS_PEM_FILE":    file("secrets/ledger.pem", pemOf(t, ca.Issue(t, "ledger"))),
		"GRPC_CLIENT_CA_FILE":  caFile,
		"POSTGRES_TLS_CA_FILE": caFile,
	}
	maps.Copy(ledgerEnv, pl.postgres(ca, file))
	ledger := n.service(ledgerAddr)
	for i := range ledgerReplicas {
		r := &replica{name: fmt.Sprintf("ledger %d", i), addr: pl.addr(fmt.Sprintf("ledger-%d:9090", i))}
		probes := pl.addr(fmt.Sprintf("ledger-%d:8081", i))
		env := maps.Clone(ledgerEnv)
		env["GRPC_ADDR"] = r.addr
		env["PROBES_ADDR"] = probes
		r.start = func() (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
			return grpcservertest.Start(t, func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
				if pl.ledger == nil {
					return ledgerapp.Init(ctx, s)
				}
				return ledgerapp.InitWith(ctx, s, pl.ledger)
			}, ledgerApp, humustest.WithEnv(env))
		}
		r.ready = func(ctx context.Context) error { return serving(ctx, pl.host.Dial, r, probes) }
		require.NoError(t, ledger.run(ctx, r))
	}

	wallet := n.service(walletAddr)
	w := &replica{name: "the wallet", addr: pl.addr("wallet-0:9443")}
	walletProbes := pl.addr("wallet-0:8081")
	walletEnv := map[string]string{
		"GRPC_ADDR":           w.addr,
		"GRPC_TLS_PEM_FILE":   file("secrets/wallet.pem", pemOf(t, ca.Issue(t, "wallet"))),
		"PROBES_ADDR":         walletProbes,
		"LEDGER_ADDR":         ledgerAddr,
		"LEDGER_CA_FILE":      caFile,
		"LEDGER_TLS_PEM_FILE": file("secrets/wallet-ledger.pem", pemOf(t, ca.Issue(t, "wallet"))),
		"TOKEN_KEYS_URL":      keysURL,
		"TOKEN_KEYS_CA_FILE":  caFile,
		"TOKEN_ISSUER":        issuer,
		"TOKEN_AUDIENCE":      audience,
		"FAULT_TRUST_SCOPE":   trustScope,
	}
	w.start = func() (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
		return grpcservertest.Start(t, Init, walletApp, humustest.WithEnv(walletEnv))
	}
	w.ready = func(ctx context.Context) error { return serving(ctx, pl.host.Dial, w, walletProbes) }
	require.NoError(t, wallet.run(ctx, w))

	casesCert := ca.Issue(t, "cases").TLS()
	return &harness{
		target: cases.Target{
			Wallet:                   connect(t, n, walletAddr, &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12}),
			Ledger:                   connect(t, n, ledgerAddr, &tls.Config{RootCAs: ca.Pool(), Certificates: []tls.Certificate{casesCert}, MinVersion: tls.VersionTLS12}),
			LedgerWithoutCertificate: connect(t, n, ledgerAddr, &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12}),
			Token:                    provider.sign,
			RolloutWallet:            wallet.rollout,
			RolloutLedger:            ledger.rollout,
			ScenarioKey:              scenarioKey,
			TrustScope:               trustScope,
			Timeout:                  pl.timeout,
		},
		wallet: wallet,
		ledger: ledger,
		keys:   keys,
	}
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), h.target.Timeout)
	defer cancel()
	require.NoError(t, h.wallet.stop(ctx))
	require.NoError(t, h.ledger.stop(ctx))
	require.NoError(t, h.keys.stop(ctx))
}

func connect(t *testing.T, n *network, addr string, cfg *tls.Config) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return n.Dial(ctx, "tcp", addr)
		}),
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func serving(ctx context.Context, dial dialFunc, r *replica, probes string) error {
	return poll(ctx, r, func(ctx context.Context) bool {
		conn, err := grpc.NewClient("passthrough:///"+probes,
			grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
				return dial(ctx, "tcp", addr)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return false
		}
		defer func() { _ = conn.Close() }()
		check, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		resp, err := healthpb.NewHealthClient(conn).Check(check, &healthpb.HealthCheckRequest{Service: "readiness"})
		return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
	})
}

func dialable(ctx context.Context, dial dialFunc, r *replica) error {
	return poll(ctx, r, func(ctx context.Context) bool {
		conn, err := dial(ctx, "tcp", r.addr)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
}

func poll(ctx context.Context, r *replica, ok func(context.Context) bool) error {
	for {
		if ok(ctx) {
			return nil
		}
		select {
		case st := <-r.status:
			return fmt.Errorf("%s exited %d before it was ready: %w", r.name, st.Code(), st.Cause())
		case <-ctx.Done():
			return fmt.Errorf("%s never turned ready: %w", r.name, context.Cause(ctx))
		case <-time.After(pollReady):
		}
	}
}

func writeHostFile(t *testing.T, fsys humus.FS, name string, contents []byte) string {
	t.Helper()
	if dir := filepath.Dir(name); dir != "." {
		require.NoError(t, fsys.MkdirAll(dir, 0o700))
	}
	f, err := fsys.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	_, err = f.Write(contents)
	require.NoError(t, errors.Join(err, f.Close()))
	return name
}

func runCase(t *testing.T, c cases.Case) {
	synctest.Test(t, func(t *testing.T) {
		h := startHarness(t, inSynctest(t))
		c.Run(t, h.target)
		h.stop(t)
	})
}

func TestInit_passes_the_shared_cases(t *testing.T) {
	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.Parallel {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				runCase(t, c)
			})
		}
	})
	for _, c := range cases.Alone {
		t.Run(c.Name, func(t *testing.T) {
			runCase(t, c)
		})
	}
}

func TestHarness_rolls_the_ledger_out_while_the_wallet_answers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := startHarness(t, inSynctest(t))
		ctx := withToken(t.Context(), mustSign(t, h.target, "alice"))
		wallet := walletpb.NewWalletClient(h.target.Wallet)
		_, err := wallet.OpenAccount(ctx, &walletpb.OpenAccountRequest{})
		require.NoError(t, err)

		require.NoError(t, h.target.RolloutLedger(t.Context()))

		_, err = ledgerpb.NewLedgerClient(h.target.Ledger).Deposit(t.Context(), &ledgerpb.DepositRequest{TransferId: rand.Text(), AccountId: "alice", AmountCents: 250})
		require.NoError(t, err)
		got, err := wallet.GetAccount(ctx, &walletpb.GetAccountRequest{})
		require.NoError(t, err)
		require.EqualValues(t, 250, got.GetAccount().GetBalanceCents())

		h.stop(t)
	})
}

func mustSign(t *testing.T, target cases.Target, subject string) string {
	t.Helper()
	tok, err := target.Token(subject, "wallet:read", "wallet:write")
	require.NoError(t, err)
	return tok
}
