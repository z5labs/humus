// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/z5labs/humus/example/grpc/wallet/server"
	"github.com/z5labs/humus/example/grpc/wallet/walletpb"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/humustest"
)

const getAccountPoint = "grpc:ledger.v1.Ledger/GetAccount"

func startTrusting(t *testing.T, scope string, tel *humustest.Telemetry) *wallet {
	t.Helper()
	f := newFixture(t, nil)
	if scope != "" {
		f.env["FAULT_TRUST_SCOPE"] = scope
	}
	f.tel = tel.Builder()
	f.startKeys(t)
	l, _ := f.startLedger(t)
	w := startWallet(t, f)
	w.ledger = l
	return w
}

func withScenario(t *testing.T, ctx context.Context, rules ...fit.Rule) context.Context {
	t.Helper()
	codec, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	return metadata.AppendToOutgoingContext(ctx, "baggage", b.String())
}

func abort(point string) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Abort: true}}
}

func (w *wallet) rehearse(t *testing.T, token string, rules ...fit.Rule) error {
	t.Helper()
	ctx := t.Context()
	if token != "" {
		ctx = withToken(ctx, token)
	}
	_, err := w.client(t).GetAccount(withScenario(t, ctx, rules...), &walletpb.GetAccountRequest{})
	return err
}

func events(spans []sdktrace.ReadOnlySpan, name string) [][]attribute.KeyValue {
	var got [][]attribute.KeyValue
	for _, span := range spans {
		for _, ev := range span.Events() {
			if ev.Name == name {
				got = append(got, ev.Attributes)
			}
		}
	}
	return got
}

func attr(attrs []attribute.KeyValue, key attribute.Key) attribute.Value {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

func TestInit_trusts_a_scenario_from_a_token_with_the_trust_scope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		w := startTrusting(t, trustScope, tel)
		w.ledger.deposit("alice", 100)
		token := w.f.idp.token(t, "alice", server.ScopeRead, trustScope)

		st := requireCode(t, w.rehearse(t, token, abort(getAccountPoint)), codes.Unavailable)
		var reasons []string
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				reasons = append(reasons, info.GetReason())
			}
		}
		require.Equal(t, []string{"LEDGER_UNAVAILABLE"}, reasons, st.Message())
		require.Zero(t, w.ledger.called(), "the ledger was called")

		_, err := w.client(t).GetAccount(withToken(t.Context(), token), &walletpb.GetAccountRequest{})
		require.NoError(t, err)

		w.stop(t)
		evals := events(tel.Spans(), "humus.fit.evaluation")
		require.Len(t, evals, grpcclient.DefaultSettings().MaxAttempts)
		for _, ev := range evals {
			require.Equal(t, getAccountPoint, attr(ev, "humus.fit.point").AsString())
			require.Equal(t, getAccountPoint, attr(ev, "humus.fit.rule").AsString())
			require.True(t, attr(ev, "humus.fit.abort").AsBool())
		}
		require.Empty(t, events(tel.Spans(), "humus.fit.strip"))
	})
}

func TestInit_forwards_a_trusted_scenario_to_the_ledger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		w := startTrusting(t, trustScope, tel)
		w.ledger.deposit("alice", 100)
		token := w.f.idp.token(t, "alice", server.ScopeRead, trustScope)

		require.NoError(t, w.rehearse(t, token, abort(ledgerPoint)))
		require.Equal(t, 1, w.ledger.called())
		require.Equal(t, 1, w.ledger.scenarios())

		w.stop(t)
		evals := events(tel.Spans(), "humus.fit.evaluation")
		require.Len(t, evals, 1)
		require.Equal(t, getAccountPoint, attr(evals[0], "humus.fit.point").AsString())
		require.Empty(t, attr(evals[0], "humus.fit.rule").AsString(), "a rule matched the ledger's call")
		require.Empty(t, events(tel.Spans(), "humus.fit.strip"))
	})
}

func TestInit_strips_a_scenario_it_does_not_trust(t *testing.T) {
	testCases := map[string]struct {
		scope  string
		scopes []string
		code   codes.Code
	}{
		"from a token without the trust scope":                            {scope: trustScope, scopes: []string{server.ScopeRead}, code: codes.OK},
		"from a token, with the trust scope empty":                        {scopes: []string{server.ScopeRead, trustScope}, code: codes.OK},
		"from a call without a token":                                     {scope: trustScope, code: codes.Unauthenticated},
		"from a token with a scope that only starts with the trust scope": {scope: trustScope, scopes: []string{server.ScopeRead, trustScope + "s"}, code: codes.OK},
	}
	for desc, tc := range testCases {
		t.Run(desc, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := humustest.NewTelemetry()
				w := startTrusting(t, tc.scope, tel)
				w.ledger.deposit("alice", 100)
				token := ""
				if tc.scopes != nil {
					token = w.f.idp.token(t, "alice", tc.scopes...)
				}

				err := w.rehearse(t, token, abort(getAccountPoint), abort(ledgerPoint))
				if tc.code == codes.OK {
					require.NoError(t, err)
					require.Equal(t, 1, w.ledger.called())
				} else {
					requireCode(t, err, tc.code)
					require.Zero(t, w.ledger.called())
				}
				require.Zero(t, w.ledger.scenarios(), "the ledger got the scenario")

				w.stop(t)
				require.Len(t, events(tel.Spans(), "humus.fit.strip"), 1)
				require.Empty(t, events(tel.Spans(), "humus.fit.evaluation"))
			})
		})
	}
}
