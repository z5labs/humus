// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus/example/wallet/ledger/server"
	"github.com/z5labs/humus/example/wallet/ledger/store"
	"github.com/z5labs/humus/example/wallet/ledgerpb"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const watchInterval = time.Second

var errUnreachable = fmt.Errorf("%w: dial tcp 10.0.0.7:5432: connect: connection refused", store.ErrUnavailable)

type fakeStore struct {
	mu        sync.Mutex
	accounts  map[string]store.Account
	transfers map[string]store.Transfer
	err       error
	reads     int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accounts:  map[string]store.Account{store.External: {ID: store.External, BalanceChangedAt: time.Now()}},
		transfers: map[string]store.Transfer{},
	}
}

func (f *fakeStore) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeStore) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeStore) OpenAccount(_ context.Context, id string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Account{}, fmt.Errorf("opening account %s: %w", id, f.err)
	}
	a, ok := f.accounts[id]
	if !ok {
		a = store.Account{ID: id, BalanceChangedAt: time.Now()}
		f.accounts[id] = a
	}
	return a, nil
}

func (f *fakeStore) Account(_ context.Context, id string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return store.Account{}, fmt.Errorf("reading account %s: %w", id, f.err)
	}
	a, ok := f.accounts[id]
	if !ok {
		return store.Account{}, fmt.Errorf("reading account %s: %w", id, store.ErrNotFound)
	}
	return a, nil
}

func (f *fakeStore) Deposit(_ context.Context, transferID, accountID string, amountCents int64) (store.Transfer, error) {
	return f.move(store.Transfer{ID: transferID, FromAccountID: store.External, ToAccountID: accountID, AmountCents: amountCents}, false)
}

func (f *fakeStore) Transfer(_ context.Context, transferID, fromAccountID, toAccountID string, amountCents int64) (store.Transfer, error) {
	return f.move(store.Transfer{ID: transferID, FromAccountID: fromAccountID, ToAccountID: toAccountID, AmountCents: amountCents}, true)
}

func (f *fakeStore) move(want store.Transfer, checkBalance bool) (store.Transfer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if want.AmountCents <= 0 {
		return store.Transfer{}, store.ErrNotPositive
	}
	if want.FromAccountID == want.ToAccountID {
		return store.Transfer{}, store.ErrSameAccount
	}
	if f.err != nil {
		return store.Transfer{}, f.err
	}
	if t, ok := f.transfers[want.ID]; ok {
		if t.FromAccountID != want.FromAccountID || t.ToAccountID != want.ToAccountID || t.AmountCents != want.AmountCents {
			return store.Transfer{}, store.ErrReusedID
		}
		t.Replayed = true
		return t, nil
	}
	from, ok := f.accounts[want.FromAccountID]
	if !ok {
		return store.Transfer{}, fmt.Errorf("account %s: %w", want.FromAccountID, store.ErrNotFound)
	}
	to, ok := f.accounts[want.ToAccountID]
	if !ok {
		return store.Transfer{}, fmt.Errorf("account %s: %w", want.ToAccountID, store.ErrNotFound)
	}
	if checkBalance && from.BalanceCents < want.AmountCents {
		return store.Transfer{}, &store.ShortBalanceError{AccountID: from.ID, BalanceCents: from.BalanceCents, AmountCents: want.AmountCents}
	}
	want.CreatedAt = time.Now()
	from.BalanceCents -= want.AmountCents
	from.BalanceChangedAt = want.CreatedAt
	to.BalanceCents += want.AmountCents
	to.BalanceChangedAt = want.CreatedAt
	f.accounts[from.ID] = from
	f.accounts[to.ID] = to
	f.transfers[want.ID] = want
	return want, nil
}

func serve(t *testing.T, st server.Store) ledgerpb.LedgerClient {
	t.Helper()
	return serveWith(t, st, telemetry.Noop())
}

func serveWith(t *testing.T, st server.Store, tel telemetry.SDK) ledgerpb.LedgerClient {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	l, err := h.Listen("tcp", "ledger:9090")
	require.NoError(t, err)

	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{ca.Issue(t, "ledger").TLS()},
	})))
	svc, err := server.New(st, watchInterval, tel)
	require.NoError(t, err)
	ledgerpb.RegisterLedgerServer(srv, svc)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		srv.Stop()
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("Serve: %v", err)
		}
	})
	return ledgerpb.NewLedgerClient(grpcservertest.NewClient(t, h, "ledger:9090", ca.Pool()))
}

func requireCode(t *testing.T, err error, want codes.Code) *status.Status {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "not a status: %v", err)
	require.Equal(t, want, st.Code(), st.Message())
	return st
}

func requireBadRequest(t *testing.T, err error, fields ...string) {
	t.Helper()
	st := requireCode(t, err, codes.InvalidArgument)
	var got []string
	for _, d := range st.Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			for _, v := range br.GetFieldViolations() {
				require.NotEmpty(t, v.GetDescription())
				got = append(got, v.GetField())
			}
		}
	}
	require.Equal(t, fields, got)
}

func requireUnavailable(t *testing.T, err error) {
	t.Helper()
	st := requireCode(t, err, codes.Unavailable)
	require.NotContains(t, st.Message(), "10.0.0.7")
}

func deposit(t *testing.T, c ledgerpb.LedgerClient, transferID, accountID string, amountCents int64) {
	t.Helper()
	_, err := c.Deposit(t.Context(), &ledgerpb.DepositRequest{TransferId: transferID, AccountId: accountID, AmountCents: amountCents})
	require.NoError(t, err)
}

func open(t *testing.T, c ledgerpb.LedgerClient, ids ...string) {
	t.Helper()
	for _, id := range ids {
		_, err := c.OpenAccount(t.Context(), &ledgerpb.OpenAccountRequest{AccountId: id})
		require.NoError(t, err)
	}
}

func TestNew_refuses_a_watch_interval_that_isnt_positive(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		_, err := server.New(newFakeStore(), interval, telemetry.Noop())
		require.ErrorIs(t, err, server.ErrWatchInterval, interval)
	}
}

func TestOpenAccount(t *testing.T) {
	t.Run("opens an account with a zero balance", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := serve(t, newFakeStore())
			resp, err := c.OpenAccount(t.Context(), &ledgerpb.OpenAccountRequest{AccountId: "alice"})
			require.NoError(t, err)
			require.Equal(t, "alice", resp.GetAccount().GetId())
			require.Zero(t, resp.GetAccount().GetBalanceCents())
			require.True(t, resp.GetAccount().GetBalanceChangeTime().AsTime().Equal(time.Now()))
		})
	})

	t.Run("answers an account already open as it is", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := serve(t, newFakeStore())
			open(t, c, "alice")
			deposit(t, c, "d1", "alice", 500)
			resp, err := c.OpenAccount(t.Context(), &ledgerpb.OpenAccountRequest{AccountId: "alice"})
			require.NoError(t, err)
			require.Equal(t, int64(500), resp.GetAccount().GetBalanceCents())
		})
	})

	t.Run("is InvalidArgument without an account_id", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := serve(t, newFakeStore())
			_, err := c.OpenAccount(t.Context(), &ledgerpb.OpenAccountRequest{})
			requireBadRequest(t, err, "account_id")
		})
	})

	t.Run("is Unavailable when the database can't be reached", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newFakeStore()
			st.fail(errUnreachable)
			_, err := serve(t, st).OpenAccount(t.Context(), &ledgerpb.OpenAccountRequest{AccountId: "alice"})
			requireUnavailable(t, err)
		})
	})
}

func TestGetAccount(t *testing.T) {
	t.Run("answers the account", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := serve(t, newFakeStore())
			open(t, c, "alice")
			time.Sleep(time.Minute)
			deposit(t, c, "d1", "alice", 250)
			resp, err := c.GetAccount(t.Context(), &ledgerpb.GetAccountRequest{AccountId: "alice"})
			require.NoError(t, err)
			require.Equal(t, "alice", resp.GetAccount().GetId())
			require.Equal(t, int64(250), resp.GetAccount().GetBalanceCents())
			require.True(t, resp.GetAccount().GetBalanceChangeTime().AsTime().Equal(time.Now()))
		})
	})

	t.Run("is NotFound for no such account", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, err := serve(t, newFakeStore()).GetAccount(t.Context(), &ledgerpb.GetAccountRequest{AccountId: "nobody"})
			st := requireCode(t, err, codes.NotFound)
			require.Contains(t, st.Message(), "nobody")
		})
	})

	t.Run("is InvalidArgument without an account_id", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, err := serve(t, newFakeStore()).GetAccount(t.Context(), &ledgerpb.GetAccountRequest{})
			requireBadRequest(t, err, "account_id")
		})
	})

	t.Run("is Unavailable when the database can't be reached", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newFakeStore()
			st.fail(errUnreachable)
			_, err := serve(t, st).GetAccount(t.Context(), &ledgerpb.GetAccountRequest{AccountId: "alice"})
			requireUnavailable(t, err)
		})
	})

	t.Run("is Internal for an error the store doesn't name", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			secret := rand.Text()
			st := newFakeStore()
			st.fail(errors.New("password " + secret + " rejected"))
			_, err := serve(t, st).GetAccount(t.Context(), &ledgerpb.GetAccountRequest{AccountId: "alice"})
			s := requireCode(t, err, codes.Internal)
			require.NotContains(t, s.Message(), secret)
		})
	})
}

func TestDeposit(t *testing.T) {
	t.Run("answers the deposit, as a transfer from the external account", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := serve(t, newFakeStore())
			open(t, c, "alice")
			resp, err := c.Deposit(t.Context(), &ledgerpb.DepositRequest{TransferId: "d1", AccountId: "alice", AmountCents: 700})
			require.NoError(t, err)
			tr := resp.GetTransfer()
			require.Equal(t, "d1", tr.GetId())
			require.Equal(t, store.External, tr.GetFromAccountId())
			require.Equal(t, "alice", tr.GetToAccountId())
			require.Equal(t, int64(700), tr.GetAmountCents())
			require.True(t, tr.GetCreateTime().AsTime().Equal(time.Now()))
		})
	})

	t.Run("is NotFound for no such account", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, err := serve(t, newFakeStore()).Deposit(t.Context(), &ledgerpb.DepositRequest{TransferId: "d1", AccountId: "nobody", AmountCents: 1})
			st := requireCode(t, err, codes.NotFound)
			require.Contains(t, st.Message(), "nobody")
		})
	})

	t.Run("is AlreadyExists for a transfer's ID reused for another deposit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := serve(t, newFakeStore())
			open(t, c, "alice")
			deposit(t, c, "d1", "alice", 100)
			_, err := c.Deposit(t.Context(), &ledgerpb.DepositRequest{TransferId: "d1", AccountId: "alice", AmountCents: 200})
			requireCode(t, err, codes.AlreadyExists)
		})
	})

	for _, tc := range []struct {
		name   string
		req    *ledgerpb.DepositRequest
		fields []string
	}{
		{name: "an amount of zero", req: &ledgerpb.DepositRequest{TransferId: "d1", AccountId: "alice"}, fields: []string{"amount_cents"}},
		{name: "a negative amount", req: &ledgerpb.DepositRequest{TransferId: "d1", AccountId: "alice", AmountCents: -5}, fields: []string{"amount_cents"}},
		{name: "a deposit into the external account", req: &ledgerpb.DepositRequest{TransferId: "d1", AccountId: store.External, AmountCents: 5}, fields: []string{"account_id"}},
		{name: "no IDs", req: &ledgerpb.DepositRequest{AmountCents: 5}, fields: []string{"transfer_id", "account_id"}},
	} {
		t.Run("is InvalidArgument for "+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := serve(t, newFakeStore())
				open(t, c, "alice")
				_, err := c.Deposit(t.Context(), tc.req)
				requireBadRequest(t, err, tc.fields...)
			})
		})
	}

	t.Run("is Unavailable when the database can't be reached", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newFakeStore()
			c := serve(t, st)
			open(t, c, "alice")
			st.fail(errUnreachable)
			_, err := c.Deposit(t.Context(), &ledgerpb.DepositRequest{TransferId: "d1", AccountId: "alice", AmountCents: 1})
			requireUnavailable(t, err)
		})
	})
}

func TestTransfer(t *testing.T) {
	t.Run("answers the transfer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := serve(t, newFakeStore())
			open(t, c, "alice", "bob")
			deposit(t, c, "d1", "alice", 1000)
			resp, err := c.Transfer(t.Context(), &ledgerpb.TransferRequest{TransferId: "t1", FromAccountId: "alice", ToAccountId: "bob", AmountCents: 400})
			require.NoError(t, err)
			tr := resp.GetTransfer()
			require.Equal(t, "t1", tr.GetId())
			require.Equal(t, "alice", tr.GetFromAccountId())
			require.Equal(t, "bob", tr.GetToAccountId())
			require.Equal(t, int64(400), tr.GetAmountCents())
		})
	})

	t.Run("is FailedPrecondition, naming the account, when its balance is short", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := serve(t, newFakeStore())
			open(t, c, "alice", "bob")
			deposit(t, c, "d1", "alice", 100)
			_, err := c.Transfer(t.Context(), &ledgerpb.TransferRequest{TransferId: "t1", FromAccountId: "alice", ToAccountId: "bob", AmountCents: 150})
			st := requireCode(t, err, codes.FailedPrecondition)
			var subjects []string
			for _, d := range st.Details() {
				if pf, ok := d.(*errdetails.PreconditionFailure); ok {
					for _, v := range pf.GetViolations() {
						require.NotEmpty(t, v.GetType())
						require.NotEmpty(t, v.GetDescription())
						subjects = append(subjects, v.GetSubject())
					}
				}
			}
			require.Equal(t, []string{"alice"}, subjects)
		})
	})

	for _, tc := range []struct {
		name string
		from string
		to   string
	}{
		{name: "the source", from: "nobody", to: "bob"},
		{name: "the destination", from: "alice", to: "nobody"},
	} {
		t.Run("is NotFound for no such account as "+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := serve(t, newFakeStore())
				open(t, c, "alice", "bob")
				deposit(t, c, "d1", "alice", 100)
				_, err := c.Transfer(t.Context(), &ledgerpb.TransferRequest{TransferId: "t1", FromAccountId: tc.from, ToAccountId: tc.to, AmountCents: 50})
				st := requireCode(t, err, codes.NotFound)
				require.Contains(t, st.Message(), "account nobody")
			})
		})
	}

	t.Run("is AlreadyExists for a transfer's ID reused for another transfer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := serve(t, newFakeStore())
			open(t, c, "alice", "bob")
			deposit(t, c, "d1", "alice", 100)
			_, err := c.Transfer(t.Context(), &ledgerpb.TransferRequest{TransferId: "t1", FromAccountId: "alice", ToAccountId: "bob", AmountCents: 50})
			require.NoError(t, err)
			_, err = c.Transfer(t.Context(), &ledgerpb.TransferRequest{TransferId: "t1", FromAccountId: "alice", ToAccountId: "bob", AmountCents: 20})
			requireCode(t, err, codes.AlreadyExists)
		})
	})

	for _, tc := range []struct {
		name   string
		req    *ledgerpb.TransferRequest
		fields []string
	}{
		{name: "an amount of zero", req: &ledgerpb.TransferRequest{TransferId: "t1", FromAccountId: "alice", ToAccountId: "bob"}, fields: []string{"amount_cents"}},
		{name: "a negative amount", req: &ledgerpb.TransferRequest{TransferId: "t1", FromAccountId: "alice", ToAccountId: "bob", AmountCents: -1}, fields: []string{"amount_cents"}},
		{name: "an account sent to itself", req: &ledgerpb.TransferRequest{TransferId: "t1", FromAccountId: "alice", ToAccountId: "alice", AmountCents: 1}, fields: []string{"to_account_id"}},
		{name: "no IDs", req: &ledgerpb.TransferRequest{AmountCents: 1}, fields: []string{"transfer_id", "from_account_id", "to_account_id"}},
	} {
		t.Run("is InvalidArgument for "+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := serve(t, newFakeStore())
				open(t, c, "alice", "bob")
				deposit(t, c, "d1", "alice", 100)
				_, err := c.Transfer(t.Context(), tc.req)
				requireBadRequest(t, err, tc.fields...)
			})
		})
	}

	t.Run("is Unavailable when the database can't be reached", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newFakeStore()
			c := serve(t, st)
			open(t, c, "alice", "bob")
			deposit(t, c, "d1", "alice", 100)
			st.fail(errUnreachable)
			_, err := c.Transfer(t.Context(), &ledgerpb.TransferRequest{TransferId: "t1", FromAccountId: "alice", ToAccountId: "bob", AmountCents: 50})
			requireUnavailable(t, err)
		})
	})
}

type watch struct {
	msgs chan *ledgerpb.Account
	end  chan error
}

func startWatch(t *testing.T, c ledgerpb.LedgerClient, ctx context.Context, id string) watch {
	t.Helper()
	stream, err := c.WatchAccount(ctx, &ledgerpb.WatchAccountRequest{AccountId: id})
	require.NoError(t, err)
	w := watch{msgs: make(chan *ledgerpb.Account, 16), end: make(chan error, 1)}
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				w.end <- err
				return
			}
			w.msgs <- resp.GetAccount()
		}
	}()
	return w
}

func (w watch) received() []int64 {
	var balances []int64
	for {
		select {
		case a := <-w.msgs:
			balances = append(balances, a.GetBalanceCents())
		default:
			return balances
		}
	}
}

func TestWatchAccount(t *testing.T) {
	t.Run("sends the account at once, then each balance it settles on", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := serve(t, newFakeStore())
			open(t, c, "alice")
			w := startWatch(t, c, t.Context(), "alice")
			synctest.Wait()
			require.Equal(t, []int64{0}, w.received())

			deposit(t, c, "d1", "alice", 100)
			deposit(t, c, "d2", "alice", 50)
			synctest.Wait()
			require.Empty(t, w.received(), "nothing is sent before the next read")

			time.Sleep(watchInterval)
			synctest.Wait()
			require.Equal(t, []int64{150}, w.received())

			time.Sleep(5 * watchInterval)
			synctest.Wait()
			require.Empty(t, w.received(), "an unchanged balance isn't sent again")

			deposit(t, c, "d3", "alice", 25)
			time.Sleep(watchInterval)
			synctest.Wait()
			require.Equal(t, []int64{175}, w.received())
			require.Empty(t, w.end)
		})
	})

	t.Run("reads the account every watch interval", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newFakeStore()
			c := serve(t, st)
			open(t, c, "alice")
			startWatch(t, c, t.Context(), "alice")
			synctest.Wait()
			require.Equal(t, 1, st.readCount())

			time.Sleep(watchInterval - time.Nanosecond)
			synctest.Wait()
			require.Equal(t, 1, st.readCount())

			time.Sleep(time.Nanosecond)
			synctest.Wait()
			require.Equal(t, 2, st.readCount())

			time.Sleep(3 * watchInterval)
			synctest.Wait()
			require.Equal(t, 5, st.readCount())
		})
	})

	t.Run("is NotFound for no such account", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := startWatch(t, serve(t, newFakeStore()), t.Context(), "nobody")
			err := <-w.end
			requireCode(t, err, codes.NotFound)
			require.Empty(t, w.received())
		})
	})

	t.Run("is InvalidArgument without an account_id", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := startWatch(t, serve(t, newFakeStore()), t.Context(), "")
			requireBadRequest(t, <-w.end, "account_id")
		})
	})

	t.Run("is Unavailable when the database can't be reached to open it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newFakeStore()
			c := serve(t, st)
			open(t, c, "alice")
			st.fail(errUnreachable)
			w := startWatch(t, c, t.Context(), "alice")
			requireUnavailable(t, <-w.end)
			require.Empty(t, w.received())
		})
	})

	t.Run("ends with Unavailable when the database can't be reached on a later read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newFakeStore()
			c := serve(t, st)
			open(t, c, "alice")
			w := startWatch(t, c, t.Context(), "alice")
			synctest.Wait()
			require.Equal(t, []int64{0}, w.received())

			st.fail(errUnreachable)
			time.Sleep(watchInterval)
			synctest.Wait()
			requireUnavailable(t, <-w.end)
		})
	})

	t.Run("stops reading once the caller ends it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newFakeStore()
			c := serve(t, st)
			open(t, c, "alice")
			ctx, cancel := context.WithCancel(t.Context())
			w := startWatch(t, c, ctx, "alice")
			synctest.Wait()
			cancel()
			requireCode(t, <-w.end, codes.Canceled)
			synctest.Wait()
			reads := st.readCount()

			time.Sleep(5 * watchInterval)
			synctest.Wait()
			require.Equal(t, reads, st.readCount())
		})
	})
}
