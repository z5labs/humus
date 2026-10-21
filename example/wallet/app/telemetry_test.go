// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"

	"github.com/z5labs/humus/example/wallet/ledgerpb"
	"github.com/z5labs/humus/example/wallet/walletpb"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	walletScope = "github.com/z5labs/humus/example/wallet/server"
	ledgerScope = "github.com/z5labs/humus/example/wallet/ledger/server"
)

type observed struct {
	*harness
	walletTel *humustest.Telemetry
	ledgerTel *humustest.Telemetry
}

func startObserved(t *testing.T) observed {
	t.Helper()
	walletTel, ledgerTel := humustest.NewTelemetry(), humustest.NewTelemetry()
	h := startHarnessWith(t, inSynctest(t), walletTel.Builder(), ledgerTel.Builder())
	return observed{harness: h, walletTel: walletTel, ledgerTel: ledgerTel}
}

type caller struct {
	subject string
	token   string
}

func (h *harness) caller(t *testing.T, scopes ...string) caller {
	t.Helper()
	subject := rand.Text()
	token, err := h.target.Token(subject, scopes...)
	require.NoError(t, err)
	return caller{subject: subject, token: token}
}

func (h *harness) funded(t *testing.T, amountCents int64, scopes ...string) caller {
	t.Helper()
	c := h.caller(t, append([]string{"wallet:read", "wallet:write"}, scopes...)...)
	_, err := walletpb.NewWalletClient(h.target.Wallet).OpenAccount(withToken(t.Context(), c.token), &walletpb.OpenAccountRequest{})
	require.NoError(t, err)
	if amountCents > 0 {
		_, err = ledgerpb.NewLedgerClient(h.target.Ledger).Deposit(t.Context(), &ledgerpb.DepositRequest{TransferId: rand.Text(), AccountId: c.subject, AmountCents: amountCents})
		require.NoError(t, err)
	}
	return c
}

func (h *harness) send(ctx context.Context, from, to caller, requestID string, amountCents int64) error {
	_, err := walletpb.NewWalletClient(h.target.Wallet).Send(withToken(ctx, from.token), &walletpb.SendRequest{
		RequestId:   requestID,
		ToAccountId: to.subject,
		AmountCents: amountCents,
	})
	return err
}

func spansNamed(spans []sdktrace.ReadOnlySpan, kind trace.SpanKind, name string, traceID trace.TraceID) []sdktrace.ReadOnlySpan {
	var got []sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.SpanKind() == kind && span.Name() == name && span.SpanContext().TraceID() == traceID {
			got = append(got, span)
		}
	}
	return got
}

func serverSpanNamed(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var got []sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.SpanKind() == trace.SpanKindServer && span.Name() == name {
			got = append(got, span)
		}
	}
	require.Len(t, got, 1, "%s's server spans", name)
	return got[0]
}

func descends(span, ancestor sdktrace.ReadOnlySpan, spans []sdktrace.ReadOnlySpan) bool {
	byID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byID[s.SpanContext().SpanID()] = s
	}
	for parent := span.Parent(); parent.IsValid(); {
		if parent.SpanID() == ancestor.SpanContext().SpanID() {
			return true
		}
		next, ok := byID[parent.SpanID()]
		if !ok {
			return false
		}
		parent = next.Parent()
	}
	return false
}

func requireOneTrace(t *testing.T, o observed, method string) (attempts, ledgerSpans []sdktrace.ReadOnlySpan) {
	t.Helper()
	walletSpans := o.walletTel.Spans()
	call := serverSpanNamed(t, walletSpans, "wallet.v1.Wallet/"+method)
	traceID := call.SpanContext().TraceID()

	attempts = spansNamed(walletSpans, trace.SpanKindClient, "ledger.v1.Ledger/Transfer", traceID)
	require.NotEmpty(t, attempts, "the wallet has no attempt span in the %s's trace", method)
	for _, attempt := range attempts {
		require.True(t, descends(attempt, call, walletSpans), "an attempt is not under the wallet's %s", method)
	}

	ledgerSpans = spansNamed(o.ledgerTel.Spans(), trace.SpanKindServer, "ledger.v1.Ledger/Transfer", traceID)
	require.Len(t, ledgerSpans, len(attempts), "the ledger's server spans in the %s's trace", method)
	var parents []trace.SpanID
	for _, span := range ledgerSpans {
		require.True(t, slices.ContainsFunc(attempts, func(attempt sdktrace.ReadOnlySpan) bool {
			return attempt.SpanContext().SpanID() == span.Parent().SpanID()
		}), "a ledger server span is under none of the wallet's attempts")
		require.NotContains(t, parents, span.Parent().SpanID(), "two ledger server spans are under one attempt")
		parents = append(parents, span.Parent().SpanID())
	}
	return attempts, ledgerSpans
}

func TestInit_traces_a_send_across_the_wallet_and_the_ledger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := startObserved(t)
		alice := o.funded(t, 500)
		bob := o.funded(t, 0)
		require.NoError(t, o.send(t.Context(), alice, bob, rand.Text(), 200))
		o.stop(t)

		attempts, _ := requireOneTrace(t, o, "Send")
		require.Len(t, attempts, 1)
	})
}

func TestInit_traces_each_attempt_at_a_send_the_ledger_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := startObserved(t)
		alice := o.funded(t, 500, trustScope)
		bob := o.funded(t, 0)
		err := o.send(withScenario(t, t.Context(), abort(ledgerPoint)), alice, bob, rand.Text(), 200)
		requireCode(t, err, codes.Unavailable)
		o.stop(t)

		attempts, _ := requireOneTrace(t, o, "Send")
		require.Len(t, attempts, grpcclient.DefaultSettings().MaxAttempts)
	})
}

func warnings(records []sdklog.Record, scope string) []sdklog.Record {
	var got []sdklog.Record
	for _, r := range records {
		if r.Severity() == log.SeverityWarn && r.InstrumentationScope().Name == scope {
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

func TestInit_writes_a_warning_in_each_service_when_the_ledger_fails_a_send(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := startObserved(t)
		alice := o.funded(t, 500, trustScope)
		bob := o.funded(t, 0)
		require.NoError(t, o.send(t.Context(), alice, bob, rand.Text(), 100))
		requireCode(t, o.send(t.Context(), alice, bob, rand.Text(), 10_000), codes.FailedPrecondition)
		require.Empty(t, warnings(o.walletTel.Logs(), walletScope), "the wallet warned of a send the ledger answered")
		require.Empty(t, warnings(o.ledgerTel.Logs(), ledgerScope), "the ledger warned of a transfer it answered")

		err := o.send(withScenario(t, t.Context(), abort(ledgerPoint)), alice, bob, rand.Text(), 100)
		requireCode(t, err, codes.Unavailable)
		o.stop(t)

		wallet := warnings(o.walletTel.Logs(), walletScope)
		require.Len(t, wallet, 1)
		require.Equal(t, "wallet.v1.Wallet/Send", recordAttr(wallet[0], "rpc.method").AsString())
		require.NotEmpty(t, recordAttr(wallet[0], "exception.message").AsString())

		ledger := warnings(o.ledgerTel.Logs(), ledgerScope)
		require.Len(t, ledger, grpcclient.DefaultSettings().MaxAttempts)
		for _, r := range ledger {
			require.Equal(t, "ledger.v1.Ledger/Transfer", recordAttr(r, "rpc.method").AsString())
			require.Contains(t, recordAttr(r, "exception.message").AsString(), ledgerPoint)
		}
	})
}

func sums(t *testing.T, tel *humustest.Telemetry, name string, key attribute.Key) map[string]int64 {
	t.Helper()
	rm, err := tel.Metrics(t.Context())
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
				v, _ := dp.Attributes.Value(key)
				got[v.AsString()] += dp.Value
			}
		}
	}
	return got
}

func TestInit_counts_the_ledgers_transfers_by_outcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := startObserved(t)
		alice := o.funded(t, 500)
		bob := o.funded(t, 0)
		requestID := rand.Text()
		require.NoError(t, o.send(t.Context(), alice, bob, requestID, 100))
		require.NoError(t, o.send(t.Context(), alice, bob, requestID, 100))
		require.NoError(t, o.send(t.Context(), alice, bob, rand.Text(), 100))
		requireCode(t, o.send(t.Context(), alice, bob, rand.Text(), 10_000), codes.FailedPrecondition)

		require.Equal(t, map[string]int64{"posted": 2, "replayed": 1, "refused": 1}, sums(t, o.ledgerTel, "ledger.transfers", "ledger.transfer.outcome"))
		o.stop(t)
	})
}

func TestInit_counts_the_watches_open_on_the_ledger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := startObserved(t)
		alice := o.funded(t, 500)
		ctx, cancel := context.WithCancel(withToken(t.Context(), alice.token))
		watch, err := walletpb.NewWalletClient(o.target.Wallet).WatchAccount(ctx, &walletpb.WatchAccountRequest{})
		require.NoError(t, err)
		_, err = watch.Recv()
		require.NoError(t, err)
		require.Equal(t, map[string]int64{"": 1}, sums(t, o.ledgerTel, "ledger.watches", ""))

		cancel()
		_, err = watch.Recv()
		requireCode(t, err, codes.Canceled)
		synctest.Wait()
		require.Equal(t, map[string]int64{"": 0}, sums(t, o.ledgerTel, "ledger.watches", ""))
		o.stop(t)
	})
}

func TestInit_counts_the_ledgers_watches_the_wallet_opens_again(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		walletTel := humustest.NewTelemetry()
		h := startHarnessWith(t, inSynctest(t), walletTel.Builder(), telemetry.NoopBuilder)
		alice := h.funded(t, 0)
		watch, err := walletpb.NewWalletClient(h.target.Wallet).WatchAccount(withToken(t.Context(), alice.token), &walletpb.WatchAccountRequest{})
		require.NoError(t, err)
		_, err = watch.Recv()
		require.NoError(t, err)
		require.Empty(t, sums(t, walletTel, "wallet.watches.reopened", ""))

		require.NoError(t, h.target.RolloutLedger(t.Context()))
		_, err = ledgerpb.NewLedgerClient(h.target.Ledger).Deposit(t.Context(), &ledgerpb.DepositRequest{TransferId: rand.Text(), AccountId: alice.subject, AmountCents: 250})
		require.NoError(t, err)
		got, err := watch.Recv()
		require.NoError(t, err)
		require.EqualValues(t, 250, got.GetAccount().GetBalanceCents())

		require.Positive(t, sums(t, walletTel, "wallet.watches.reopened", "")[""])
		h.stop(t)
	})
}

func spanStrings(spans []sdktrace.ReadOnlySpan) []string {
	var out []string
	for _, span := range spans {
		out = append(out, span.Name(), span.Status().Description)
		for _, kv := range span.Attributes() {
			out = append(out, kv.Value.String())
		}
		for _, ev := range span.Events() {
			out = append(out, ev.Name)
			for _, kv := range ev.Attributes {
				out = append(out, kv.Value.String())
			}
		}
	}
	return out
}

func logStrings(records []sdklog.Record) []string {
	var out []string
	for _, r := range records {
		out = append(out, r.Body().String())
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			out = append(out, kv.Value.String())
			return true
		})
	}
	return out
}

func TestInit_puts_no_token_on_a_span_or_a_record(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := startObserved(t)
		alice := o.funded(t, 500, trustScope)
		bob := o.funded(t, 0)
		forged := caller{subject: alice.subject, token: alice.token[:strings.LastIndex(alice.token, ".")+1] + "c2lnbmF0dXJl"}

		require.NoError(t, o.send(t.Context(), alice, bob, rand.Text(), 100))
		requireCode(t, o.send(t.Context(), alice, bob, rand.Text(), 10_000), codes.FailedPrecondition)
		requireCode(t, o.send(withScenario(t, t.Context(), abort(ledgerPoint)), alice, bob, rand.Text(), 100), codes.Unavailable)
		requireCode(t, o.send(t.Context(), forged, bob, rand.Text(), 100), codes.Unauthenticated)
		watch, err := walletpb.NewWalletClient(o.target.Wallet).WatchAccount(withToken(t.Context(), alice.token), &walletpb.WatchAccountRequest{})
		require.NoError(t, err)
		_, err = watch.Recv()
		require.NoError(t, err)
		o.stop(t)

		var recorded []string
		for _, tel := range []*humustest.Telemetry{o.walletTel, o.ledgerTel} {
			recorded = append(recorded, spanStrings(tel.Spans())...)
			recorded = append(recorded, logStrings(tel.Logs())...)
		}
		require.NotEmpty(t, warnings(o.walletTel.Logs(), walletScope))
		require.NotEmpty(t, warnings(o.ledgerTel.Logs(), ledgerScope))
		parts := strings.Split(alice.token, ".")
		secrets := map[string]string{"token": alice.token, "forged token": forged.token, "claims": parts[1], "signature": parts[2]}
		for _, v := range recorded {
			for name, secret := range secrets {
				require.NotContains(t, v, secret, "the %s was recorded", name)
			}
		}
	})
}

func TestBuildTelemetry_names_the_service_wallet(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"by default":           {env: map[string]string{}, want: "wallet"},
		"unless it is renamed": {env: map[string]string{"OTEL_SERVICE_NAME": "wallet-canary"}, want: "wallet-canary"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_LOGS_EXPORTER": "none"}
			maps.Copy(env, tc.env)
			var stdout bytes.Buffer
			sdk, err := BuildTelemetry(t.Context(), bedrock.Process{Environment: bedrock.NewStaticEnvironment(env), Stdout: &stdout}, nil)
			require.NoError(t, err)

			_, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "probe")
			span.End()
			require.NoError(t, sdk.Shutdown(context.Background()))

			require.Contains(t, stdout.String(), `"Key":"service.name","Value":{"Type":"STRING","Value":"`+tc.want+`"}`)
		})
	}
}
