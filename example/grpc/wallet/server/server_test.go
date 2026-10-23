// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"net"
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
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/example/grpc/wallet/ledgerpb"
	"github.com/z5labs/humus/example/grpc/wallet/server"
	"github.com/z5labs/humus/example/grpc/wallet/walletpb"
	"github.com/z5labs/humus/grpc/grpcauth"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	ledgerAddr = "ledger:9090"
	walletAddr = "wallet:9443"
)

type watchEnd struct {
	done chan struct{}
	err  error
}

type fakeLedger struct {
	ledgerpb.UnimplementedLedgerServer

	mu        sync.Mutex
	accounts  map[string]*ledgerpb.Account
	transfers map[string]*ledgerpb.Transfer
	changed   chan struct{}
	end       *watchEnd
	err       error
	opens     int
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		accounts:  map[string]*ledgerpb.Account{},
		transfers: map[string]*ledgerpb.Transfer{},
		changed:   make(chan struct{}),
		end:       &watchEnd{done: make(chan struct{})},
	}
}

func (l *fakeLedger) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

func (l *fakeLedger) endWatches(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.end.err = err
	close(l.end.done)
	l.end = &watchEnd{done: make(chan struct{})}
}

func (l *fakeLedger) watchOpens() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opens
}

func (l *fakeLedger) transfer(id string) (*ledgerpb.Transfer, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.transfers[id]
	return t, ok
}

func (l *fakeLedger) transferCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.transfers)
}

func (l *fakeLedger) balance(id string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accounts[id].GetBalanceCents()
}

func (l *fakeLedger) open(ids ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if _, ok := l.accounts[id]; !ok {
			l.accounts[id] = &ledgerpb.Account{Id: id, BalanceChangeTime: timestamppb.Now()}
		}
	}
}

func (l *fakeLedger) deposit(id string, amountCents int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[id]
	a.BalanceCents += amountCents
	a.BalanceChangeTime = timestamppb.Now()
	l.notify()
}

func (l *fakeLedger) notify() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *fakeLedger) OpenAccount(_ context.Context, req *ledgerpb.OpenAccountRequest) (*ledgerpb.OpenAccountResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	a, ok := l.accounts[req.GetAccountId()]
	if !ok {
		a = &ledgerpb.Account{Id: req.GetAccountId(), BalanceChangeTime: timestamppb.Now()}
		l.accounts[a.GetId()] = a
	}
	return &ledgerpb.OpenAccountResponse{Account: proto.CloneOf(a)}, nil
}

func (l *fakeLedger) GetAccount(_ context.Context, req *ledgerpb.GetAccountRequest) (*ledgerpb.GetAccountResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	a, ok := l.accounts[req.GetAccountId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "account %s: not found", req.GetAccountId())
	}
	return &ledgerpb.GetAccountResponse{Account: proto.CloneOf(a)}, nil
}

func (l *fakeLedger) Transfer(_ context.Context, req *ledgerpb.TransferRequest) (*ledgerpb.TransferResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	switch {
	case req.GetToAccountId() == "":
		return nil, badRequest("to_account_id", "is required")
	case req.GetAmountCents() <= 0:
		return nil, badRequest("amount_cents", "must be more than zero")
	case req.GetFromAccountId() == req.GetToAccountId():
		return nil, badRequest("to_account_id", "is the account the transfer is from")
	}
	want := &ledgerpb.Transfer{
		Id:            req.GetTransferId(),
		FromAccountId: req.GetFromAccountId(),
		ToAccountId:   req.GetToAccountId(),
		AmountCents:   req.GetAmountCents(),
	}
	if t, ok := l.transfers[want.GetId()]; ok {
		if t.GetFromAccountId() != want.GetFromAccountId() || t.GetToAccountId() != want.GetToAccountId() || t.GetAmountCents() != want.GetAmountCents() {
			return nil, status.Errorf(codes.AlreadyExists, "transfer %s: already made, as another transfer", want.GetId())
		}
		return &ledgerpb.TransferResponse{Transfer: proto.CloneOf(t)}, nil
	}
	from, ok := l.accounts[want.GetFromAccountId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "account %s: not found", want.GetFromAccountId())
	}
	to, ok := l.accounts[want.GetToAccountId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "account %s: not found", want.GetToAccountId())
	}
	if from.GetBalanceCents() < want.GetAmountCents() {
		return nil, withDetails(codes.FailedPrecondition, "the balance is short", &errdetails.PreconditionFailure{
			Violations: []*errdetails.PreconditionFailure_Violation{{Type: "BALANCE", Subject: from.GetId(), Description: "the balance is short"}},
		})
	}
	want.CreateTime = timestamppb.Now()
	from.BalanceCents -= want.GetAmountCents()
	from.BalanceChangeTime = want.GetCreateTime()
	to.BalanceCents += want.GetAmountCents()
	to.BalanceChangeTime = want.GetCreateTime()
	l.transfers[want.GetId()] = want
	l.notify()
	return &ledgerpb.TransferResponse{Transfer: proto.CloneOf(want)}, nil
}

func (l *fakeLedger) WatchAccount(req *ledgerpb.WatchAccountRequest, stream grpc.ServerStreamingServer[ledgerpb.WatchAccountResponse]) error {
	l.mu.Lock()
	l.opens++
	if l.err != nil {
		defer l.mu.Unlock()
		return l.err
	}
	a, ok := l.accounts[req.GetAccountId()]
	if !ok {
		defer l.mu.Unlock()
		return status.Errorf(codes.NotFound, "account %s: not found", req.GetAccountId())
	}
	sent, changed, end := proto.CloneOf(a), l.changed, l.end
	l.mu.Unlock()

	if err := stream.Send(&ledgerpb.WatchAccountResponse{Account: sent}); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-end.done:
			return end.err
		case <-changed:
		}
		l.mu.Lock()
		a, changed = proto.CloneOf(l.accounts[req.GetAccountId()]), l.changed
		l.mu.Unlock()
		if a.GetBalanceCents() == sent.GetBalanceCents() {
			continue
		}
		if err := stream.Send(&ledgerpb.WatchAccountResponse{Account: a}); err != nil {
			return err
		}
		sent = a
	}
}

func badRequest(field, description string) error {
	return withDetails(codes.InvalidArgument, "the request is invalid", &errdetails.BadRequest{
		FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: field, Description: description}},
	})
}

func withDetails(code codes.Code, msg string, details ...protoadapt.MessageV1) error {
	st, err := status.New(code, msg).WithDetails(details...)
	if err != nil {
		panic(err)
	}
	return st.Err()
}

type tokens struct {
	mu     sync.Mutex
	claims map[string]server.Claims
}

func (ts *tokens) issue(subject string) string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	token := rand.Text()
	ts.claims[token] = server.Claims{Subject: subject, Scope: server.ScopeRead + " " + server.ScopeWrite}
	return token
}

func (ts *tokens) verify(_ context.Context, token string) (server.Claims, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	c, ok := ts.claims[token]
	if !ok {
		return server.Claims{}, fmt.Errorf("unknown token: %w", auth.ErrUnauthenticated)
	}
	return c, nil
}

type env struct {
	t        *testing.T
	h        *humustest.Host
	ca       *humustest.CA
	ledger   *fakeLedger
	tokens   *tokens
	wallet   walletpb.WalletClient
	route    atomic.Pointer[string]
	replicas map[string]func()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		t:        t,
		h:        humustest.NewHost(t),
		ca:       humustest.NewCA(t),
		ledger:   newFakeLedger(),
		tokens:   &tokens{claims: map[string]server.Claims{}},
		replicas: map[string]func(){},
	}
	e.startReplica("ledger-1:9090")
	e.routeTo("ledger-1:9090")
	e.startWallet()
	return e
}

func (e *env) routeTo(addr string) {
	e.route.Store(&addr)
}

func (e *env) startReplica(addr string) {
	t := e.t
	t.Helper()
	cert := e.ca.Issue(t, "ledger").TLS()
	p, exit := grpcservertest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: addr,
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		})
		if err != nil {
			return nil, err
		}
		ledgerpb.RegisterLedgerServer(svc, e.ledger)
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(e.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-e.h.Listening(addr)

	var once sync.Once
	e.replicas[addr] = func() {
		once.Do(func() { stop(t, "the ledger at "+addr, p, exit) })
	}
	t.Cleanup(func() {
		once.Do(func() { <-exit })
	})
}

func (e *env) stopReplica(addr string) {
	e.replicas[addr]()
}

func (e *env) startWallet() {
	t := e.t
	t.Helper()
	host := e.h.Host()
	host.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == ledgerAddr {
			address = *e.route.Load()
		}
		return e.h.Dial(ctx, network, address)
	}
	cert := e.ca.Issue(t, "wallet").TLS()
	_, exit := grpcservertest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := grpcclient.DefaultSettings()
		settings.Addr = ledgerAddr
		settings.TLS = &tls.Config{RootCAs: e.ca.Pool(), MinVersion: tls.VersionTLS12}
		ledger, err := grpcclient.NewClient(s, settings)
		if err != nil {
			return nil, err
		}
		bearer := auth.Bearer("bearer", e.tokens.verify)
		write := auth.RequireScopes(bearer, server.ScopeWrite)
		unary, stream, err := grpcauth.Interceptors(s,
			grpcauth.Default(auth.RequireScopes(bearer, server.ScopeRead)),
			grpcauth.Method(walletpb.Wallet_OpenAccount_FullMethodName, write),
			grpcauth.Method(walletpb.Wallet_Send_FullMethodName, write),
		)
		if err != nil {
			return nil, err
		}
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: walletAddr,
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		}, unary, stream)
		if err != nil {
			return nil, err
		}
		wallet, err := server.New(ledgerpb.NewLedgerClient(ledger), bearer, s.Telemetry)
		if err != nil {
			return nil, err
		}
		walletpb.RegisterWalletServer(svc, wallet)
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-e.h.Listening(walletAddr)
	t.Cleanup(func() { <-exit })
	e.wallet = walletpb.NewWalletClient(grpcservertest.NewClient(t, e.h, walletAddr, e.ca.Pool()))
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

func (e *env) as(subject string) context.Context {
	return metadata.AppendToOutgoingContext(e.t.Context(), "authorization", "Bearer "+e.tokens.issue(subject))
}

func requireCode(t *testing.T, err error, want codes.Code) *status.Status {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "not a status: %v", err)
	require.Equal(t, want, st.Code(), st.Message())
	return st
}

func requireReason(t *testing.T, err error, reason string) *status.Status {
	t.Helper()
	st := requireCode(t, err, codes.Unavailable)
	var reasons []string
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reasons = append(reasons, info.GetReason())
			require.Equal(t, "wallet.v1.Wallet", info.GetDomain())
		}
	}
	require.Equal(t, []string{reason}, reasons, st.Message())
	return st
}

func retryDelay(st *status.Status) (time.Duration, bool) {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.RetryInfo); ok {
			return info.GetRetryDelay().AsDuration(), true
		}
	}
	return 0, false
}

type method struct {
	name string
	call func(ctx context.Context, c walletpb.WalletClient) error
}

var methods = []method{
	{"OpenAccount", func(ctx context.Context, c walletpb.WalletClient) error {
		_, err := c.OpenAccount(ctx, &walletpb.OpenAccountRequest{})
		return err
	}},
	{"GetAccount", func(ctx context.Context, c walletpb.WalletClient) error {
		_, err := c.GetAccount(ctx, &walletpb.GetAccountRequest{})
		return err
	}},
	{"Send", func(ctx context.Context, c walletpb.WalletClient) error {
		_, err := c.Send(ctx, &walletpb.SendRequest{RequestId: rand.Text(), ToAccountId: "bob", AmountCents: 1})
		return err
	}},
	{"WatchAccount", func(ctx context.Context, c walletpb.WalletClient) error {
		w, err := c.WatchAccount(ctx, &walletpb.WatchAccountRequest{})
		if err != nil {
			return err
		}
		_, err = w.Recv()
		return err
	}},
}

func TestEveryMethod_is_Unauthenticated_for_a_token_without_a_subject(t *testing.T) {
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				e.ledger.open("", "bob")
				err := m.call(e.as(""), e.wallet)
				requireCode(t, err, codes.Unauthenticated)
				require.Zero(t, e.ledger.transferCount())
			})
		})
	}
}

func TestEveryMethod_is_Unavailable_when_the_ledger_cant_be_reached(t *testing.T) {
	outages := []struct {
		name  string
		start func(e *env)
	}{
		{"the ledger answers Unavailable", func(e *env) {
			e.ledger.fail(status.Error(codes.Unavailable, "the ledger's database can't be reached"))
		}},
		{"the ledger answers Internal", func(e *env) {
			e.ledger.fail(status.Error(codes.Internal, "the ledger failed to answer the request"))
		}},
		{"the ledger answers DeadlineExceeded", func(e *env) {
			e.ledger.fail(status.Error(codes.DeadlineExceeded, "the ledger's database didn't answer in time"))
		}},
		{"nothing listens at the ledger's address", func(e *env) {
			e.routeTo("nowhere:9090")
		}},
	}
	for _, o := range outages {
		for _, m := range methods {
			t.Run(o.name+"/"+m.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					e := newEnv(t)
					e.ledger.open("alice", "bob")
					e.ledger.deposit("alice", 100)
					o.start(e)
					st := requireReason(t, m.call(e.as("alice"), e.wallet), "LEDGER_UNAVAILABLE")
					_, ok := retryDelay(st)
					require.False(t, ok, "a RetryInfo on an answer the breaker didn't make")
					require.Zero(t, e.ledger.transferCount())
				})
			})
		}
	}
}

func TestEveryMethod_is_LEDGER_SHED_with_a_RetryInfo_once_the_breaker_opens(t *testing.T) {
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				e.ledger.open("alice", "bob")
				e.ledger.deposit("alice", 100)
				e.ledger.fail(status.Error(codes.Unavailable, "the ledger's database can't be reached"))

				shed := false
				for range 10 {
					_, err := e.wallet.GetAccount(e.as("alice"), &walletpb.GetAccountRequest{})
					if st, _ := status.FromError(err); len(st.Details()) > 0 && reason(st) == "LEDGER_SHED" {
						shed = true
						break
					}
					requireReason(t, err, "LEDGER_UNAVAILABLE")
				}
				require.True(t, shed, "the breaker never opened")

				e.ledger.fail(nil)
				st := requireReason(t, m.call(e.as("alice"), e.wallet), "LEDGER_SHED")
				delay, ok := retryDelay(st)
				require.True(t, ok, "no RetryInfo")
				require.Positive(t, delay)
				require.LessOrEqual(t, delay, grpcclient.DefaultSettings().Breaker.OpenFor)
				require.Zero(t, e.ledger.transferCount())
			})
		})
	}
}

func reason(st *status.Status) string {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}

func TestOpenAccount(t *testing.T) {
	t.Run("opens the caller's account in the ledger, with a zero balance", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			resp, err := e.wallet.OpenAccount(e.as("alice"), &walletpb.OpenAccountRequest{})
			require.NoError(t, err)
			require.Equal(t, "alice", resp.GetAccount().GetId())
			require.Zero(t, resp.GetAccount().GetBalanceCents())
			require.True(t, resp.GetAccount().GetBalanceChangeTime().AsTime().Equal(time.Now()))
		})
	})

	t.Run("answers an account already open as it is", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice")
			e.ledger.deposit("alice", 500)
			resp, err := e.wallet.OpenAccount(e.as("alice"), &walletpb.OpenAccountRequest{})
			require.NoError(t, err)
			require.Equal(t, int64(500), resp.GetAccount().GetBalanceCents())
		})
	})
}

func TestGetAccount(t *testing.T) {
	t.Run("answers the caller's account", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice", "bob")
			e.ledger.deposit("alice", 500)
			resp, err := e.wallet.GetAccount(e.as("alice"), &walletpb.GetAccountRequest{})
			require.NoError(t, err)
			require.Equal(t, "alice", resp.GetAccount().GetId())
			require.Equal(t, int64(500), resp.GetAccount().GetBalanceCents())
		})
	})

	t.Run("is NotFound for an account that isn't open", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			_, err := e.wallet.GetAccount(e.as("alice"), &walletpb.GetAccountRequest{})
			requireCode(t, err, codes.NotFound)
		})
	})
}

func send(e *env, subject string, req *walletpb.SendRequest) (*walletpb.Transfer, error) {
	resp, err := e.wallet.Send(e.as(subject), req)
	return resp.GetTransfer(), err
}

func TestSend(t *testing.T) {
	t.Run("moves the money from the caller's account, and answers with the transfer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice", "bob")
			e.ledger.deposit("alice", 500)
			tr, err := send(e, "alice", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "bob", AmountCents: 200})
			require.NoError(t, err)
			require.Equal(t, "alice", tr.GetFromAccountId())
			require.Equal(t, "bob", tr.GetToAccountId())
			require.Equal(t, int64(200), tr.GetAmountCents())
			require.True(t, tr.GetCreateTime().AsTime().Equal(time.Now()))
			require.Equal(t, int64(300), e.ledger.balance("alice"))
			require.Equal(t, int64(200), e.ledger.balance("bob"))
		})
	})

	t.Run("answers a repeated request with the same transfer, and moves the money once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice", "bob")
			e.ledger.deposit("alice", 500)
			req := &walletpb.SendRequest{RequestId: "r1", ToAccountId: "bob", AmountCents: 200}
			first, err := send(e, "alice", req)
			require.NoError(t, err)
			time.Sleep(time.Second)
			again, err := send(e, "alice", req)
			require.NoError(t, err)
			require.True(t, proto.Equal(first, again), "first %v, again %v", first, again)
			require.Equal(t, int64(300), e.ledger.balance("alice"))
			require.Equal(t, 1, e.ledger.transferCount())
		})
	})

	t.Run("is AlreadyExists for a request ID reused for another transfer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice", "bob")
			e.ledger.deposit("alice", 500)
			_, err := send(e, "alice", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "bob", AmountCents: 200})
			require.NoError(t, err)
			_, err = send(e, "alice", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "bob", AmountCents: 300})
			requireCode(t, err, codes.AlreadyExists)
			require.Equal(t, int64(300), e.ledger.balance("alice"))
		})
	})

	t.Run("keeps each caller's request IDs apart", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice", "bob", "carol", "a/b", "a")
			e.ledger.deposit("alice", 500)
			e.ledger.deposit("bob", 500)
			e.ledger.deposit("a/b", 500)
			e.ledger.deposit("a", 500)

			fromAlice, err := send(e, "alice", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "carol", AmountCents: 100})
			require.NoError(t, err)
			fromBob, err := send(e, "bob", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "carol", AmountCents: 200})
			require.NoError(t, err)
			require.NotEqual(t, fromAlice.GetId(), fromBob.GetId())

			fromAB, err := send(e, "a/b", &walletpb.SendRequest{RequestId: "c", ToAccountId: "carol", AmountCents: 1})
			require.NoError(t, err)
			fromA, err := send(e, "a", &walletpb.SendRequest{RequestId: "b/c", ToAccountId: "carol", AmountCents: 2})
			require.NoError(t, err)
			require.NotEqual(t, fromAB.GetId(), fromA.GetId())

			require.Equal(t, int64(303), e.ledger.balance("carol"))
			for _, tr := range []*walletpb.Transfer{fromAlice, fromBob, fromAB, fromA} {
				kept, ok := e.ledger.transfer(tr.GetId())
				require.True(t, ok, tr.GetId())
				require.Equal(t, tr.GetFromAccountId(), kept.GetFromAccountId())
			}
		})
	})

	t.Run("is FailedPrecondition, with the ledger's PreconditionFailure, when the balance is short", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice", "bob")
			e.ledger.deposit("alice", 100)
			_, err := send(e, "alice", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "bob", AmountCents: 200})
			st := requireCode(t, err, codes.FailedPrecondition)
			require.Len(t, st.Details(), 1)
			pf, ok := st.Details()[0].(*errdetails.PreconditionFailure)
			require.True(t, ok, "details: %v", st.Details())
			require.Len(t, pf.GetViolations(), 1)
			require.Equal(t, "BALANCE", pf.GetViolations()[0].GetType())
			require.Equal(t, "alice", pf.GetViolations()[0].GetSubject())
			require.Equal(t, int64(100), e.ledger.balance("alice"))
			require.Zero(t, e.ledger.transferCount())
		})
	})

	t.Run("is NotFound for an account that isn't open", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice")
			e.ledger.deposit("alice", 100)
			_, err := send(e, "alice", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "nobody", AmountCents: 50})
			requireCode(t, err, codes.NotFound)
			_, err = send(e, "carol", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "alice", AmountCents: 50})
			requireCode(t, err, codes.NotFound)
		})
	})

	invalid := []struct {
		name  string
		req   *walletpb.SendRequest
		field string
	}{
		{"without a request_id", &walletpb.SendRequest{ToAccountId: "bob", AmountCents: 50}, "request_id"},
		{"without a to_account_id", &walletpb.SendRequest{RequestId: "r1", AmountCents: 50}, "to_account_id"},
		{"for an amount of zero", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "bob"}, "amount_cents"},
		{"for a negative amount", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "bob", AmountCents: -50}, "amount_cents"},
		{"to the caller's own account", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "alice", AmountCents: 50}, "to_account_id"},
	}
	for _, tc := range invalid {
		t.Run("is InvalidArgument, with a BadRequest naming "+tc.field+", "+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				e.ledger.open("alice", "bob")
				e.ledger.deposit("alice", 100)
				_, err := send(e, "alice", tc.req)
				st := requireCode(t, err, codes.InvalidArgument)
				require.Len(t, st.Details(), 1)
				br, ok := st.Details()[0].(*errdetails.BadRequest)
				require.True(t, ok, "details: %v", st.Details())
				require.Len(t, br.GetFieldViolations(), 1)
				require.Equal(t, tc.field, br.GetFieldViolations()[0].GetField())
				require.NotEmpty(t, br.GetFieldViolations()[0].GetDescription())
				require.Zero(t, e.ledger.transferCount())
			})
		})
	}
}

func watch(t *testing.T, e *env, subject string) grpc.ServerStreamingClient[walletpb.WatchAccountResponse] {
	t.Helper()
	w, err := e.wallet.WatchAccount(e.as(subject), &walletpb.WatchAccountRequest{})
	require.NoError(t, err)
	return w
}

func requireBalance(t *testing.T, w grpc.ServerStreamingClient[walletpb.WatchAccountResponse], want int64) {
	t.Helper()
	resp, err := w.Recv()
	require.NoError(t, err)
	require.Equal(t, "alice", resp.GetAccount().GetId())
	require.Equal(t, want, resp.GetAccount().GetBalanceCents())
}

func TestWatchAccount(t *testing.T) {
	t.Run("sends the caller's account at once, and again each time its balance changes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice", "bob")
			w := watch(t, e, "alice")
			requireBalance(t, w, 0)
			e.ledger.deposit("alice", 500)
			requireBalance(t, w, 500)
			_, err := send(e, "alice", &walletpb.SendRequest{RequestId: "r1", ToAccountId: "bob", AmountCents: 200})
			require.NoError(t, err)
			requireBalance(t, w, 300)
		})
	})

	t.Run("is NotFound for an account that isn't open", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			_, err := watch(t, e, "alice").Recv()
			requireCode(t, err, codes.NotFound)
			require.Equal(t, 1, e.ledger.watchOpens())
		})
	})

	t.Run("opens the ledger's watch again when it ends with Unavailable after a message", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice")
			w := watch(t, e, "alice")
			requireBalance(t, w, 0)
			e.ledger.endWatches(status.Error(codes.Unavailable, "the ledger's database can't be reached"))
			synctest.Wait()
			require.Equal(t, 2, e.ledger.watchOpens())
			e.ledger.deposit("alice", 500)
			requireBalance(t, w, 500)
		})
	})

	t.Run("relays the watch through the ledger's shutdown", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice")
			w := watch(t, e, "alice")
			requireBalance(t, w, 0)

			e.startReplica("ledger-2:9090")
			e.routeTo("ledger-2:9090")
			e.stopReplica("ledger-1:9090")
			synctest.Wait()

			e.ledger.deposit("alice", 500)
			requireBalance(t, w, 500)
			e.ledger.deposit("alice", 250)
			requireBalance(t, w, 750)
			require.GreaterOrEqual(t, e.ledger.watchOpens(), 2)
		})
	})

	t.Run("ends with LEDGER_UNAVAILABLE when the ledger's watch ends with Unavailable before its first message", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice")
			e.ledger.fail(status.Error(codes.Unavailable, "the ledger's database can't be reached"))
			_, err := watch(t, e, "alice").Recv()
			requireReason(t, err, "LEDGER_UNAVAILABLE")
			require.Equal(t, 1, e.ledger.watchOpens())
		})
	})

	t.Run("ends with LEDGER_UNAVAILABLE when the ledger's watch ends with Unavailable after a message and then before its next first message", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice")
			w := watch(t, e, "alice")
			requireBalance(t, w, 0)
			e.ledger.fail(status.Error(codes.Unavailable, "the ledger's database can't be reached"))
			e.ledger.endWatches(status.Error(codes.Unavailable, "the ledger's database can't be reached"))
			_, err := w.Recv()
			requireReason(t, err, "LEDGER_UNAVAILABLE")
			require.Equal(t, 2, e.ledger.watchOpens())
		})
	})

	ends := []struct {
		name string
		err  error
		want func(t *testing.T, err error)
	}{
		{"Internal", status.Error(codes.Internal, "the ledger failed to answer the request"), func(t *testing.T, err error) {
			requireReason(t, err, "LEDGER_UNAVAILABLE")
		}},
		{"DeadlineExceeded", status.Error(codes.DeadlineExceeded, "the ledger's database didn't answer in time"), func(t *testing.T, err error) {
			requireReason(t, err, "LEDGER_UNAVAILABLE")
		}},
		{"NotFound", status.Error(codes.NotFound, "account alice: not found"), func(t *testing.T, err error) {
			requireCode(t, err, codes.NotFound)
		}},
	}
	for _, end := range ends {
		t.Run("ends with a unary method's answer when the ledger's watch ends with "+end.name+" after a message", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				e.ledger.open("alice")
				w := watch(t, e, "alice")
				requireBalance(t, w, 0)
				e.ledger.endWatches(end.err)
				_, err := w.Recv()
				end.want(t, err)
				require.Equal(t, 1, e.ledger.watchOpens())
			})
		})
	}

	t.Run("ends the ledger's watch when the caller leaves", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice")
			ctx, cancel := context.WithCancel(e.as("alice"))
			w, err := e.wallet.WatchAccount(ctx, &walletpb.WatchAccountRequest{})
			require.NoError(t, err)
			requireBalance(t, w, 0)
			cancel()
			_, err = w.Recv()
			requireCode(t, err, codes.Canceled)
			synctest.Wait()
			require.Equal(t, 1, e.ledger.watchOpens())
		})
	})
}
