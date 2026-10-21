// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the wallet's black-box cases, run against a deployment of the wallet and the ledger.
package cases

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/z5labs/humus/example/wallet/ledgerpb"
	"github.com/z5labs/humus/example/wallet/walletpb"
	"github.com/z5labs/humus/fault/jsonscenario"
)

const (
	scopeRead  = "wallet:read"
	scopeWrite = "wallet:write"

	pollEvery = 500 * time.Millisecond

	shuttingDown = "server is shutting down"

	transferPoint       = "grpc:ledger.v1.Ledger/Transfer"
	ledgerDatabasePoint = "postgresql:ledger"
	slowLedger          = 2 * time.Second

	watchersInARollout = 8
	sentInARollout     = 100

	ledgerUnavailable = "LEDGER_UNAVAILABLE"
	ledgerShed        = "LEDGER_SHED"
)

type Target struct {
	Wallet grpc.ClientConnInterface

	Ledger                   grpc.ClientConnInterface
	LedgerWithoutCertificate grpc.ClientConnInterface

	Token func(subject string, scopes ...string) (string, error)

	RolloutWallet func(ctx context.Context) error
	RolloutLedger func(ctx context.Context) error

	ScenarioKey string
	TrustScope  string
	Timeout     time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var Parallel = []Case{
	{Name: "AnAccountsLife", Run: AnAccountsLife},
	{Name: "Sending", Run: Sending},
	{Name: "SendingAgain", Run: SendingAgain},
	{Name: "NotEnoughMoney", Run: NotEnoughMoney},
	{Name: "SendsAtOnce", Run: SendsAtOnce},
	{Name: "WhoMayDoWhat", Run: WhoMayDoWhat},
	{Name: "AWatch", Run: AWatch},
	{Name: "ASlowLedger", Run: ASlowLedger},
	{Name: "AnUntrustedScenario", Run: AnUntrustedScenario},
}

var Alone = []Case{
	{Name: "TheLedgerIsDown", Run: TheLedgerIsDown},
	{Name: "TheLedgersDatabaseIsDown", Run: TheLedgersDatabaseIsDown},
	{Name: "ARolloutOfTheWallet", Run: ARolloutOfTheWallet},
	{Name: "ARolloutOfTheLedger", Run: ARolloutOfTheLedger},
	{Name: "TheLedgersBreakerOpens", Run: TheLedgersBreakerOpens},
}

func AnAccountsLife(t testing.TB, target Target) {
	ctx := bounded(t, target)
	h := newHolder(t, target, scopeRead, scopeWrite)

	opened := h.openAccount(ctx, t)
	require.Equal(t, h.subject, opened.GetId())
	require.Zero(t, opened.GetBalanceCents())

	again := h.openAccount(ctx, t)
	require.Equal(t, h.subject, again.GetId())
	require.Zero(t, again.GetBalanceCents())

	deposit(ctx, t, target, h.subject, 1250)
	require.EqualValues(t, 1250, h.balance(ctx, t))
}

func Sending(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newFunded(ctx, t, target, 1000)
	bob := newFunded(ctx, t, target, 0)

	sent := alice.send(ctx, t, bob, rand.Text(), 300)
	require.NotEmpty(t, sent.GetId())
	require.Equal(t, alice.subject, sent.GetFromAccountId())
	require.Equal(t, bob.subject, sent.GetToAccountId())
	require.EqualValues(t, 300, sent.GetAmountCents())

	require.EqualValues(t, 700, alice.balance(ctx, t))
	require.EqualValues(t, 300, bob.balance(ctx, t))
}

func SendingAgain(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newFunded(ctx, t, target, 1000)
	bob := newFunded(ctx, t, target, 0)
	requestID := rand.Text()

	first := alice.send(ctx, t, bob, requestID, 400)
	again := alice.send(ctx, t, bob, requestID, 400)
	require.True(t, proto.Equal(first, again), "the same request ID answers with the same transfer: %v, then %v", first, again)
	require.EqualValues(t, 600, alice.balance(ctx, t))
	require.EqualValues(t, 400, bob.balance(ctx, t))

	_, err := alice.trySend(ctx, bob, requestID, 500)
	requireCode(t, err, codes.AlreadyExists)
	require.EqualValues(t, 600, alice.balance(ctx, t))
	require.EqualValues(t, 400, bob.balance(ctx, t))
}

func NotEnoughMoney(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newFunded(ctx, t, target, 100)
	bob := newFunded(ctx, t, target, 0)

	_, err := alice.trySend(ctx, bob, rand.Text(), 101)
	st := requireCode(t, err, codes.FailedPrecondition)
	var subjects []string
	for _, d := range st.Details() {
		if failure, ok := d.(*errdetails.PreconditionFailure); ok {
			for _, v := range failure.GetViolations() {
				subjects = append(subjects, v.GetSubject())
			}
		}
	}
	require.Equal(t, []string{alice.subject}, subjects, "the PreconditionFailure names the account: %s", st.Message())

	require.EqualValues(t, 100, alice.balance(ctx, t))
	require.Zero(t, bob.balance(ctx, t))
}

func SendsAtOnce(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newFunded(ctx, t, target, 500)
	bob := newFunded(ctx, t, target, 0)

	const sends = 10
	answers := make(chan error, sends)
	for range sends {
		go func() {
			_, err := alice.trySend(ctx, bob, rand.Text(), 100)
			answers <- err
		}()
	}
	var sent, short int
	for range sends {
		err := <-answers
		switch status.Code(err) {
		case codes.OK:
			sent++
		case codes.FailedPrecondition:
			short++
		default:
			require.NoError(t, err)
		}
	}
	require.Equal(t, 5, sent, "sends that succeeded")
	require.Equal(t, 5, short, "sends that were FailedPrecondition")

	require.Zero(t, alice.balance(ctx, t))
	require.EqualValues(t, 500, bob.balance(ctx, t))
}

func WhoMayDoWhat(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newFunded(ctx, t, target, 100)
	bob := newFunded(ctx, t, target, 0)

	reader := alice.withScopes(t, target, scopeRead)
	_, err := reader.trySend(ctx, bob, rand.Text(), 10)
	requireCode(t, err, codes.PermissionDenied)

	anonymous := holder{target: target, subject: alice.subject}
	_, err = anonymous.trySend(ctx, bob, rand.Text(), 10)
	requireCode(t, err, codes.Unauthenticated)
	_, err = anonymous.wallet().GetAccount(ctx, &walletpb.GetAccountRequest{})
	requireCode(t, err, codes.Unauthenticated)

	require.NotNil(t, target.LedgerWithoutCertificate, "Target.LedgerWithoutCertificate")
	_, err = ledgerpb.NewLedgerClient(target.LedgerWithoutCertificate).GetAccount(ctx, &ledgerpb.GetAccountRequest{AccountId: alice.subject})
	requireCode(t, err, codes.Unauthenticated)

	require.EqualValues(t, 100, alice.balance(ctx, t))
	require.Zero(t, bob.balance(ctx, t))
}

func AWatch(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newFunded(ctx, t, target, 1000)
	bob := newFunded(ctx, t, target, 0)

	watch := alice.watch(ctx, t)
	require.EqualValues(t, 1000, recvBalance(t, watch))

	alice.send(ctx, t, bob, rand.Text(), 150)
	require.EqualValues(t, 850, recvBalance(t, watch))

	alice.send(ctx, t, bob, rand.Text(), 250)
	require.EqualValues(t, 600, recvBalance(t, watch))
}

func ASlowLedger(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newRehearser(ctx, t, target, 1000)
	bob := newFunded(ctx, t, target, 0)

	start := time.Now()
	sent := alice.rehearsing(t, delay(transferPoint, slowLedger)).send(ctx, t, bob, rand.Text(), 300)
	require.GreaterOrEqual(t, time.Since(start), slowLedger, "the send was delayed")
	require.EqualValues(t, 300, sent.GetAmountCents())

	require.EqualValues(t, 700, alice.balance(ctx, t))
	require.EqualValues(t, 300, bob.balance(ctx, t))
}

func AnUntrustedScenario(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newFunded(ctx, t, target, 1000)
	bob := newFunded(ctx, t, target, 0)

	sent := alice.rehearsing(t, abort(transferPoint)).send(ctx, t, bob, rand.Text(), 300)
	require.EqualValues(t, 300, sent.GetAmountCents())

	require.EqualValues(t, 700, alice.balance(ctx, t))
	require.EqualValues(t, 300, bob.balance(ctx, t))
}

func TheLedgerIsDown(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newRehearser(ctx, t, target, 1000)
	bob := newFunded(ctx, t, target, 0)
	requestID := rand.Text()

	_, err := alice.rehearsing(t, abort(transferPoint)).trySend(ctx, bob, requestID, 300)
	requireReason(t, err, ledgerUnavailable)
	require.EqualValues(t, 1000, alice.balance(ctx, t))
	require.Zero(t, bob.balance(ctx, t))

	alice.sendOnceAdmitted(ctx, t, bob, requestID, 300)
	require.EqualValues(t, 700, alice.balance(ctx, t))
	require.EqualValues(t, 300, bob.balance(ctx, t))
}

func TheLedgersDatabaseIsDown(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newRehearser(ctx, t, target, 1000)
	bob := newFunded(ctx, t, target, 0)

	_, err := alice.rehearsing(t, abort(ledgerDatabasePoint)).trySend(ctx, bob, rand.Text(), 300)
	requireCode(t, err, codes.Unavailable)

	require.EqualValues(t, 1000, alice.balance(ctx, t))
	require.Zero(t, bob.balance(ctx, t))
}

func TheLedgersBreakerOpens(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newRehearser(ctx, t, target, 1000)
	bob := newFunded(ctx, t, target, 0)
	requestID := rand.Text()

	failing := alice.rehearsing(t, abort(transferPoint))
	for {
		_, err := failing.trySend(ctx, bob, requestID, 300)
		st := requireCode(t, err, codes.Unavailable)
		if reason(st) == ledgerShed {
			require.NotNil(t, retryInfo(st), "a LEDGER_SHED answer carries a RetryInfo: %s", st.Message())
			break
		}
		require.Equal(t, ledgerUnavailable, reason(st), "%s", st.Message())
		pause(ctx, t, pollEvery)
	}

	alice.sendOnceAdmitted(ctx, t, bob, requestID, 300)
	require.EqualValues(t, 700, alice.balance(ctx, t))
	require.EqualValues(t, 300, bob.balance(ctx, t))
}

func ARolloutOfTheWallet(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newFunded(ctx, t, target, 1000)
	bob := newFunded(ctx, t, target, 0)

	watch := alice.watch(ctx, t)
	require.EqualValues(t, 1000, recvBalance(t, watch))

	requestID := rand.Text()
	during := make(chan error, 1)
	go func() {
		_, err := alice.trySend(ctx, bob, requestID, 300)
		during <- err
	}()
	require.NotNil(t, target.RolloutWallet, "Target.RolloutWallet")
	require.NoError(t, target.RolloutWallet(ctx), "rolling the wallet out")

	for {
		resp, err := watch.Recv()
		if err != nil {
			st := requireCode(t, err, codes.Unavailable)
			require.Contains(t, st.Message(), shuttingDown)
			break
		}
		require.EqualValues(t, 700, resp.GetAccount().GetBalanceCents(), "a balance the watch sent before it ended")
	}

	if err := <-during; err != nil {
		requireCode(t, err, codes.Unavailable)
		alice.sendOnceAnswered(ctx, t, bob, requestID, 300)
	}
	require.EqualValues(t, 700, alice.balance(ctx, t))
	require.EqualValues(t, 300, bob.balance(ctx, t))

	require.EqualValues(t, 700, recvBalance(t, alice.watchOnceAnswered(ctx, t)))
}

func ARolloutOfTheLedger(t testing.TB, target Target) {
	ctx := bounded(t, target)
	bob := newFunded(ctx, t, target, 0)

	type watcher struct {
		holder
		watch grpc.ServerStreamingClient[walletpb.WatchAccountResponse]
		want  int64
	}
	watchers := make([]*watcher, watchersInARollout)
	for i := range watchers {
		w := &watcher{holder: newFunded(ctx, t, target, 100_000), want: 100_000}
		w.watch = w.holder.watch(ctx, t)
		require.Equal(t, w.want, recvBalance(t, w.watch))
		watchers[i] = w
	}

	sendFromEach := func() {
		t.Helper()
		for _, w := range watchers {
			w.send(ctx, t, bob, rand.Text(), sentInARollout)
			w.want -= sentInARollout
		}
		for _, w := range watchers {
			require.Equal(t, w.want, recvBalance(t, w.watch), "%s's watch", w.subject)
		}
	}

	require.NotNil(t, target.RolloutLedger, "Target.RolloutLedger")
	rolledOut := make(chan error, 1)
	go func() { rolledOut <- target.RolloutLedger(ctx) }()

	var rounds int
	for done := false; !done; {
		sendFromEach()
		rounds++
		select {
		case err := <-rolledOut:
			require.NoError(t, err, "rolling the ledger out")
			done = true
		default:
		}
	}
	sendFromEach()
	rounds++

	for _, w := range watchers {
		require.Equal(t, w.want, w.balance(ctx, t), "%s's balance", w.subject)
	}
	require.EqualValues(t, int64(rounds)*watchersInARollout*sentInARollout, bob.balance(ctx, t))
}

type holder struct {
	target  Target
	subject string
	token   string
	baggage string
}

func newHolder(t testing.TB, target Target, scopes ...string) holder {
	t.Helper()
	return holder{target: target, subject: rand.Text()}.withScopes(t, target, scopes...)
}

func newFunded(ctx context.Context, t testing.TB, target Target, amountCents int64) holder {
	t.Helper()
	h := newHolder(t, target, scopeRead, scopeWrite)
	require.Zero(t, h.openAccount(ctx, t).GetBalanceCents())
	if amountCents > 0 {
		deposit(ctx, t, target, h.subject, amountCents)
	}
	return h
}

func newRehearser(ctx context.Context, t testing.TB, target Target, amountCents int64) holder {
	t.Helper()
	require.NotEmpty(t, target.TrustScope, "Target.TrustScope")
	return newFunded(ctx, t, target, amountCents).withScopes(t, target, scopeRead, scopeWrite, target.TrustScope)
}

func (h holder) rehearsing(t testing.TB, rules ...fit.Rule) holder {
	t.Helper()
	require.NotEmpty(t, h.target.ScenarioKey, "Target.ScenarioKey")
	codec, err := jsonscenario.New(h.target.ScenarioKey)
	require.NoError(t, err, "Target.ScenarioKey")
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err, "encoding the scenario")
	h.baggage = b.String()
	return h
}

func (h holder) withScopes(t testing.TB, target Target, scopes ...string) holder {
	t.Helper()
	require.NotNil(t, target.Token, "Target.Token")
	tok, err := target.Token(h.subject, scopes...)
	require.NoError(t, err, "signing a token for %s", h.subject)
	h.token = tok
	return h
}

func (h holder) wallet() walletpb.WalletClient {
	return walletpb.NewWalletClient(h.target.Wallet)
}

func (h holder) with(ctx context.Context) context.Context {
	if h.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+h.token)
	}
	if h.baggage != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "baggage", h.baggage)
	}
	return ctx
}

func (h holder) openAccount(ctx context.Context, t testing.TB) *walletpb.Account {
	t.Helper()
	require.NotNil(t, h.target.Wallet, "Target.Wallet")
	resp, err := h.wallet().OpenAccount(h.with(ctx), &walletpb.OpenAccountRequest{})
	require.NoError(t, err, "opening %s's account", h.subject)
	return resp.GetAccount()
}

func (h holder) balance(ctx context.Context, t testing.TB) int64 {
	t.Helper()
	resp, err := h.wallet().GetAccount(h.with(ctx), &walletpb.GetAccountRequest{})
	require.NoError(t, err, "reading %s's account", h.subject)
	require.Equal(t, h.subject, resp.GetAccount().GetId())
	return resp.GetAccount().GetBalanceCents()
}

func (h holder) trySend(ctx context.Context, to holder, requestID string, amountCents int64) (*walletpb.Transfer, error) {
	resp, err := h.wallet().Send(h.with(ctx), &walletpb.SendRequest{
		RequestId:   requestID,
		ToAccountId: to.subject,
		AmountCents: amountCents,
	})
	return resp.GetTransfer(), err
}

func (h holder) send(ctx context.Context, t testing.TB, to holder, requestID string, amountCents int64) *walletpb.Transfer {
	t.Helper()
	sent, err := h.trySend(ctx, to, requestID, amountCents)
	require.NoError(t, err, "sending %d from %s to %s", amountCents, h.subject, to.subject)
	return sent
}

func (h holder) sendOnceAnswered(ctx context.Context, t testing.TB, to holder, requestID string, amountCents int64) *walletpb.Transfer {
	t.Helper()
	for {
		sent, err := h.trySend(ctx, to, requestID, amountCents)
		if status.Code(err) != codes.Unavailable {
			require.NoError(t, err, "sending %d from %s to %s again", amountCents, h.subject, to.subject)
			return sent
		}
		pause(ctx, t, pollEvery)
	}
}

func (h holder) sendOnceAdmitted(ctx context.Context, t testing.TB, to holder, requestID string, amountCents int64) *walletpb.Transfer {
	t.Helper()
	for {
		sent, err := h.trySend(ctx, to, requestID, amountCents)
		if status.Code(err) != codes.Unavailable {
			require.NoError(t, err, "sending %d from %s to %s once the ledger is admitted", amountCents, h.subject, to.subject)
			return sent
		}
		requireReason(t, err, ledgerShed)
		pause(ctx, t, pollEvery)
	}
}

func (h holder) watch(ctx context.Context, t testing.TB) grpc.ServerStreamingClient[walletpb.WatchAccountResponse] {
	t.Helper()
	watch, err := h.wallet().WatchAccount(h.with(ctx), &walletpb.WatchAccountRequest{})
	require.NoError(t, err, "watching %s's account", h.subject)
	return watch
}

func (h holder) watchOnceAnswered(ctx context.Context, t testing.TB) *bufferedWatch {
	t.Helper()
	for {
		watch, err := h.wallet().WatchAccount(h.with(ctx), &walletpb.WatchAccountRequest{})
		if err == nil {
			var first *walletpb.WatchAccountResponse
			first, err = watch.Recv()
			if err == nil {
				return &bufferedWatch{ServerStreamingClient: watch, first: first}
			}
		}
		require.Equal(t, codes.Unavailable, status.Code(err), "watching %s's account again: %v", h.subject, err)
		pause(ctx, t, pollEvery)
	}
}

type bufferedWatch struct {
	grpc.ServerStreamingClient[walletpb.WatchAccountResponse]
	first *walletpb.WatchAccountResponse
}

func (w *bufferedWatch) Recv() (*walletpb.WatchAccountResponse, error) {
	if first := w.first; first != nil {
		w.first = nil
		return first, nil
	}
	return w.ServerStreamingClient.Recv()
}

func recvBalance(t testing.TB, watch interface {
	Recv() (*walletpb.WatchAccountResponse, error)
}) int64 {
	t.Helper()
	resp, err := watch.Recv()
	require.NoError(t, err, "receiving from a watch")
	return resp.GetAccount().GetBalanceCents()
}

func deposit(ctx context.Context, t testing.TB, target Target, accountID string, amountCents int64) {
	t.Helper()
	require.NotNil(t, target.Ledger, "Target.Ledger")
	resp, err := ledgerpb.NewLedgerClient(target.Ledger).Deposit(ctx, &ledgerpb.DepositRequest{
		TransferId:  "deposit-" + rand.Text(),
		AccountId:   accountID,
		AmountCents: amountCents,
	})
	require.NoError(t, err, "depositing %d into %s", amountCents, accountID)
	require.Equal(t, accountID, resp.GetTransfer().GetToAccountId())
	require.Equal(t, amountCents, resp.GetTransfer().GetAmountCents())
}

func requireCode(t testing.TB, err error, want codes.Code) *status.Status {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "not a status: %v", err)
	require.Equal(t, want, st.Code(), "%s", st.Message())
	return st
}

func requireReason(t testing.TB, err error, want string) *status.Status {
	t.Helper()
	st := requireCode(t, err, codes.Unavailable)
	require.Equal(t, want, reason(st), "%s", st.Message())
	return st
}

func reason(st *status.Status) string {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}

func retryInfo(st *status.Status) *errdetails.RetryInfo {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.RetryInfo); ok {
			return info
		}
	}
	return nil
}

func abort(point string) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Abort: true}}
}

func delay(point string, d time.Duration) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Delay: d}}
}

func bounded(t testing.TB, target Target) context.Context {
	require.Positive(t, target.Timeout, "Target.Timeout")
	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout)
	t.Cleanup(cancel)
	return ctx
}

func pause(ctx context.Context, t testing.TB, d time.Duration) {
	t.Helper()
	select {
	case <-time.After(d):
	case <-ctx.Done():
		require.FailNowf(t, "timed out", "pausing for %s: %v", d, context.Cause(ctx))
	}
}
