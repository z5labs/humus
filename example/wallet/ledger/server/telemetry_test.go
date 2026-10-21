// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"

	"github.com/z5labs/humus/example/wallet/ledgerpb"
	"github.com/z5labs/humus/humustest"
)

type observed struct {
	ledgerpb.LedgerClient
	tel *humustest.Telemetry
}

func serveObserved(t *testing.T, st *fakeStore) observed {
	t.Helper()
	tel := humustest.NewTelemetry()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sdk.Shutdown(context.Background())) })
	return observed{LedgerClient: serveWith(t, st, sdk), tel: tel}
}

func (o observed) sum(t *testing.T, name string) map[string]int64 {
	t.Helper()
	rm, err := o.tel.Metrics(t.Context())
	require.NoError(t, err)
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s is a %T", name, m.Data)
			for _, dp := range sum.DataPoints {
				outcome, _ := dp.Attributes.Value("ledger.transfer.outcome")
				got[outcome.AsString()] += dp.Value
			}
		}
	}
	return got
}

func warnings(records []sdklog.Record) []sdklog.Record {
	var got []sdklog.Record
	for _, r := range records {
		if r.Severity() == log.SeverityWarn {
			got = append(got, r)
		}
	}
	return got
}

func recordAttr(r sdklog.Record, key attribute.Key) attribute.Value {
	var v attribute.Value
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Key == key {
			v = kv.Value
			return false
		}
		return true
	})
	return v
}

func TestTransfer_counts_each_transfer_by_its_outcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		c := serveObserved(t, st)
		open(t, c, "alice", "bob")
		deposit(t, c, "d1", "alice", 500)

		send := func(id string, amount int64) error {
			_, err := c.Transfer(t.Context(), &ledgerpb.TransferRequest{TransferId: id, FromAccountId: "alice", ToAccountId: "bob", AmountCents: amount})
			return err
		}
		require.NoError(t, send("t1", 200))
		require.NoError(t, send("t2", 100))
		require.NoError(t, send("t1", 200))
		requireCode(t, send("t3", 1000), codes.FailedPrecondition)
		requireCode(t, send("t1", 300), codes.AlreadyExists)
		requireCode(t, send("", 100), codes.InvalidArgument)
		st.fail(errUnreachable)
		requireUnavailable(t, send("t4", 100))

		require.Equal(t, map[string]int64{"posted": 2, "replayed": 1, "refused": 3}, c.sum(t, "ledger.transfers"))
	})
}

func TestWatchAccount_counts_the_watches_open(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := serveObserved(t, newFakeStore())
		open(t, c, "alice", "bob")

		ctx, cancel := context.WithCancel(t.Context())
		first := startWatch(t, c, ctx, "alice")
		second := startWatch(t, c, t.Context(), "bob")
		<-first.msgs
		<-second.msgs
		require.Equal(t, map[string]int64{"": 2}, c.sum(t, "ledger.watches"))

		cancel()
		requireCode(t, <-first.end, codes.Canceled)
		synctest.Wait()
		require.Equal(t, map[string]int64{"": 1}, c.sum(t, "ledger.watches"))
	})
}

func TestTransfer_writes_a_warning_when_its_transaction_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore()
		c := serveObserved(t, st)
		open(t, c, "alice", "bob")

		_, err := c.Transfer(t.Context(), &ledgerpb.TransferRequest{TransferId: "t1", FromAccountId: "alice", ToAccountId: "bob", AmountCents: 100})
		requireCode(t, err, codes.FailedPrecondition)
		require.Empty(t, warnings(c.tel.Logs()), "a refused transfer is no failure")

		st.fail(errUnreachable)
		_, err = c.Transfer(t.Context(), &ledgerpb.TransferRequest{TransferId: "t2", FromAccountId: "alice", ToAccountId: "bob", AmountCents: 100})
		requireUnavailable(t, err)

		got := warnings(c.tel.Logs())
		require.Len(t, got, 1)
		require.Equal(t, "ledger.v1.Ledger/Transfer", recordAttr(got[0], "rpc.method").AsString())
		require.Contains(t, recordAttr(got[0], "exception.message").AsString(), errUnreachable.Error())
	})
}
