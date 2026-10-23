// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"github.com/z5labs/humus/example/grpc/wallet/ledger/store"
	"github.com/z5labs/humus/example/grpc/wallet/ledgerpb"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
)

var abortStore = fit.Rule{Point: storePoint, Fault: fit.Fault{Abort: true}}

func withScenario(t *testing.T, ctx context.Context, rules ...fit.Rule) context.Context {
	t.Helper()
	codec, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	return metadata.AppendToOutgoingContext(ctx, "baggage", b.String())
}

func startRecording(t *testing.T, st *fakeStore, tel *humustest.Telemetry) *ledger {
	t.Helper()
	f := newFixture(t, nil)
	f.tel = tel.Builder()
	return startOn(t, f, onFake(st))
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

func TestInit_trusts_a_scenario_from_a_caller_whose_certificate_the_internal_CA_issued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		l := startRecording(t, newFakeStore(false), tel)
		c := l.wallet(t)

		_, err := c.GetAccount(withScenario(t, t.Context(), abortStore), &ledgerpb.GetAccountRequest{AccountId: store.External})
		requireCode(t, err, codes.Unavailable)

		_, err = c.GetAccount(t.Context(), &ledgerpb.GetAccountRequest{AccountId: store.External})
		require.NoError(t, err)

		l.stop(t)
		evals := events(tel.Spans(), "humus.fit.evaluation")
		require.Len(t, evals, 1)
		require.Equal(t, storePoint, attr(evals[0], "humus.fit.point").AsString())
		require.Equal(t, storePoint, attr(evals[0], "humus.fit.rule").AsString())
		require.True(t, attr(evals[0], "humus.fit.abort").AsBool())
		require.Empty(t, events(tel.Spans(), "humus.fit.strip"))
	})
}

func TestInit_strips_a_scenario_from_a_caller_without_a_client_certificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		st := newFakeStore(false)
		l := startRecording(t, st, tel)
		c := l.client(t, nil)

		_, err := c.GetAccount(withScenario(t, t.Context(), abortStore), &ledgerpb.GetAccountRequest{AccountId: store.External})
		requireCode(t, err, codes.Unauthenticated)
		require.Zero(t, st.called())

		l.stop(t)
		require.Len(t, events(tel.Spans(), "humus.fit.strip"), 1)
		require.Empty(t, events(tel.Spans(), "humus.fit.evaluation"))
	})
}

func TestIssuedByTheCA_trusts_only_a_peer_with_a_verified_chain(t *testing.T) {
	cert := humustest.NewCA(t).Issue(t, "wallet").TLS()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)
	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 50000}
	testCases := map[string]struct {
		ctx  context.Context
		want bool
	}{
		"no peer":                  {ctx: t.Context()},
		"a peer with no auth info": {ctx: peer.NewContext(t.Context(), &peer.Peer{Addr: addr})},
		"a TLS peer without a client certificate": {ctx: peer.NewContext(t.Context(), &peer.Peer{Addr: addr, AuthInfo: credentials.TLSInfo{}})},
		"a TLS peer whose certificate wasn't verified": {ctx: peer.NewContext(t.Context(), &peer.Peer{Addr: addr, AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}},
		}})},
		"a TLS peer with a verified chain": {ctx: peer.NewContext(t.Context(), &peer.Peer{Addr: addr, AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}},
		}}), want: true},
	}
	for desc, tc := range testCases {
		t.Run(desc, func(t *testing.T) {
			require.Equal(t, tc.want, issuedByTheCA(tc.ctx, ledgerpb.Ledger_GetAccount_FullMethodName))
		})
	}
}
