// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/example/wallet/ledgerpb"
	"github.com/z5labs/humus/example/wallet/server"
	"github.com/z5labs/humus/example/wallet/walletpb"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	issuer     = "https://idp.internal"
	audience   = "wallet"
	walletAddr = "wallet:9443"
	ledgerAddr = "ledger:9090"
)

type run struct {
	code   int
	cause  error
	stdout string
	stderr string
}

type fixture struct {
	h      *humustest.Host
	ca     *humustest.CA
	client *humustest.Certificate
	files  map[string][]byte
}

func newFixture(t *testing.T, files map[string][]byte) *fixture {
	t.Helper()
	ca := humustest.NewCA(t)
	client := ca.Issue(t, "cases")
	all := map[string][]byte{
		"key.pem":    pkcs8(t, ecKey(t, elliptic.P256())),
		"ca.pem":     caPEM(t, ca),
		"client.pem": pemOf(t, client),
	}
	maps.Copy(all, files)
	return &fixture{h: humustest.NewHost(t, humustest.WithFiles(all)), ca: ca, client: client, files: all}
}

func (f *fixture) args(args []string) []string {
	args = append([]string(nil), args...)
	for i, arg := range args {
		if _, ok := f.files[arg]; ok {
			args[i] = f.h.Path(arg)
		}
	}
	return args
}

func (f *fixture) begin(t *testing.T, build telemetry.Builder, args ...string) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	return humustest.Start(t, Init,
		humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(build)),
		humustest.WithArgs(f.args(args)...),
	)
}

func (f *fixture) start(t *testing.T, build telemetry.Builder, args ...string) run {
	t.Helper()
	p, status := f.begin(t, build, args...)
	st := <-status
	return run{code: st.Code(), cause: st.Cause(), stdout: p.Stdout(), stderr: p.Stderr()}
}

func (f *fixture) output(t *testing.T, args ...string) string {
	t.Helper()
	got := f.start(t, telemetry.NoopBuilder, args...)
	require.Zero(t, got.code, "cause: %v", got.cause)
	require.True(t, strings.HasSuffix(got.stdout, "\n"))
	return strings.TrimSuffix(got.stdout, "\n")
}

func (f *fixture) serve(t *testing.T, addr string, tlsConfig *tls.Config, register func(grpc.ServiceRegistrar)) {
	t.Helper()
	_, status := grpcservertest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, grpcserver.Settings{Addr: addr, TLS: tlsConfig})
		if err != nil {
			return nil, err
		}
		register(svc)
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	t.Cleanup(func() { <-status })
	select {
	case <-f.h.Listening(addr):
	case st := <-status:
		t.Fatalf("the server at %s exited %d: %v", addr, st.Code(), st.Cause())
	}
}

func (f *fixture) startWallet(t *testing.T, w *fakeWallet) {
	t.Helper()
	jwks := f.output(t, "jwks", "-key", "key.pem")
	w.verifier = verifier(t, jwks)
	f.serve(t, walletAddr, &tls.Config{
		Certificates: []tls.Certificate{f.ca.Issue(t, "wallet").TLS()},
		MinVersion:   tls.VersionTLS12,
	}, func(r grpc.ServiceRegistrar) { walletpb.RegisterWalletServer(r, w) })
}

func (f *fixture) startLedger(t *testing.T, l *fakeLedger) {
	t.Helper()
	f.serve(t, ledgerAddr, &tls.Config{
		Certificates: []tls.Certificate{f.ca.Issue(t, "ledger").TLS()},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    f.ca.Pool(),
		MinVersion:   tls.VersionTLS12,
	}, func(r grpc.ServiceRegistrar) { ledgerpb.RegisterLedgerServer(r, l) })
}

func walletArgs(command, subject string, extra ...string) []string {
	args := []string{command, "-addr", walletAddr, "-ca", "ca.pem", "-key", "key.pem", "-sub", subject, "-iss", issuer, "-aud", audience}
	return append(args, extra...)
}

func depositArgs(extra ...string) []string {
	args := []string{"deposit", "-addr", ledgerAddr, "-ca", "ca.pem", "-cert", "client.pem"}
	return append(args, extra...)
}

type call struct {
	method  string
	subject string
	scopes  []string
}

type watchScript struct {
	balances []int64
	end      error
}

type fakeWallet struct {
	walletpb.UnimplementedWalletServer

	verifier *jwt.Verifier[server.Claims]

	mu       sync.Mutex
	accounts map[string]int64
	calls    []call
	sends    []*walletpb.SendRequest
	sendErr  error
	scripts  []watchScript
	opened   []time.Time
	watching chan struct{}
}

func newFakeWallet() *fakeWallet {
	return &fakeWallet{accounts: map[string]int64{}, watching: make(chan struct{}, 10)}
}

func (w *fakeWallet) authorize(ctx context.Context, method, scope string) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", status.Error(codes.Unauthenticated, "no bearer token")
	}
	claims, err := w.verifier.Verify(ctx, strings.TrimPrefix(values[0], "Bearer "))
	if err != nil {
		return "", status.Error(codes.Unauthenticated, err.Error())
	}
	w.mu.Lock()
	w.calls = append(w.calls, call{method: method, subject: claims.Subject, scopes: strings.Fields(claims.Scope)})
	w.mu.Unlock()
	if !claims.HasScope(scope) {
		return "", status.Error(codes.PermissionDenied, "missing scope "+scope)
	}
	return claims.Subject, nil
}

func (w *fakeWallet) OpenAccount(ctx context.Context, _ *walletpb.OpenAccountRequest) (*walletpb.OpenAccountResponse, error) {
	sub, err := w.authorize(ctx, "OpenAccount", server.ScopeWrite)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.accounts[sub] += 0
	return &walletpb.OpenAccountResponse{Account: &walletpb.Account{Id: sub, BalanceCents: w.accounts[sub]}}, nil
}

func (w *fakeWallet) GetAccount(ctx context.Context, _ *walletpb.GetAccountRequest) (*walletpb.GetAccountResponse, error) {
	sub, err := w.authorize(ctx, "GetAccount", server.ScopeRead)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	balance, ok := w.accounts[sub]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such account")
	}
	return &walletpb.GetAccountResponse{Account: &walletpb.Account{Id: sub, BalanceCents: balance}}, nil
}

func (w *fakeWallet) Send(ctx context.Context, req *walletpb.SendRequest) (*walletpb.SendResponse, error) {
	sub, err := w.authorize(ctx, "Send", server.ScopeWrite)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sends = append(w.sends, req)
	if w.sendErr != nil {
		return nil, w.sendErr
	}
	w.accounts[sub] -= req.GetAmountCents()
	w.accounts[req.GetToAccountId()] += req.GetAmountCents()
	return &walletpb.SendResponse{Transfer: &walletpb.Transfer{
		Id:            sub + "/" + req.GetRequestId(),
		FromAccountId: sub,
		ToAccountId:   req.GetToAccountId(),
		AmountCents:   req.GetAmountCents(),
	}}, nil
}

func (w *fakeWallet) WatchAccount(_ *walletpb.WatchAccountRequest, stream grpc.ServerStreamingServer[walletpb.WatchAccountResponse]) error {
	sub, err := w.authorize(stream.Context(), "WatchAccount", server.ScopeRead)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.opened = append(w.opened, time.Now())
	var script watchScript
	if len(w.scripts) > 0 {
		script, w.scripts = w.scripts[0], w.scripts[1:]
	}
	w.mu.Unlock()

	for _, balance := range script.balances {
		if err := stream.Send(&walletpb.WatchAccountResponse{Account: &walletpb.Account{Id: sub, BalanceCents: balance}}); err != nil {
			return err
		}
	}
	w.watching <- struct{}{}
	if script.end != nil {
		return script.end
	}
	<-stream.Context().Done()
	return status.FromContextError(stream.Context().Err()).Err()
}

func (w *fakeWallet) recorded() []call {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]call(nil), w.calls...)
}

type fakeLedger struct {
	ledgerpb.UnimplementedLedgerServer

	mu       sync.Mutex
	deposits []*ledgerpb.DepositRequest
	peers    [][]byte
}

func (l *fakeLedger) Deposit(ctx context.Context, req *ledgerpb.DepositRequest) (*ledgerpb.DepositResponse, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "no client certificate")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deposits = append(l.deposits, req)
	l.peers = append(l.peers, info.State.PeerCertificates[0].Raw)
	return &ledgerpb.DepositResponse{Transfer: &ledgerpb.Transfer{
		Id:            req.GetTransferId(),
		FromAccountId: "external",
		ToAccountId:   req.GetAccountId(),
		AmountCents:   req.GetAmountCents(),
	}}, nil
}

func pkcs8(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func pemOf(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var out []byte
	for _, der := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return append(out, pkcs8(t, cert.PrivateKey.(crypto.Signer))...)
}

func caPEM(t *testing.T, ca *humustest.CA) []byte {
	t.Helper()
	c := ca.Issue(t, "unused").TLS()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[len(c.Certificate)-1]})
}

func ecKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	require.NoError(t, err)
	return key
}

func sec1WithParameters(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	params := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}})
	return append(params, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})...)
}

func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	require.NoError(t, err)
	return key
}

func ed25519Key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return key
}

type staticKeySet string

func (s staticKeySet) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/jwk-set+json"}},
		Body:       io.NopCloser(strings.NewReader(string(s))),
		Request:    req,
	}, nil
}

func verifier(t *testing.T, jwks string) *jwt.Verifier[server.Claims] {
	t.Helper()
	u, err := url.Parse("https://keys.internal/.well-known/jwks.json")
	require.NoError(t, err)
	keys, err := jwt.NewRemoteKeySet(u, &http.Client{Transport: staticKeySet(jwks)})
	require.NoError(t, err)
	settings := jwt.DefaultSettings()
	settings.Issuer = issuer
	settings.Audience = audience
	settings.Algorithms = []jwt.Algorithm{jwt.RS256, jwt.ES256, jwt.ES384, jwt.ES512, jwt.EdDSA}
	v, err := jwt.NewVerifier[server.Claims](&humus.Setup{Telemetry: telemetry.Noop()}, settings, keys)
	require.NoError(t, err)
	return v
}

func tokenArgs(subject string, scopes ...string) []string {
	args := []string{"token", "-key", "key.pem", "-sub", subject, "-iss", issuer, "-aud", audience}
	for _, scope := range scopes {
		args = append(args, "-scope", scope)
	}
	return args
}

func TestInit_a_token_from_token_is_verified_against_the_key_set_from_jwks(t *testing.T) {
	testCases := map[string]func(t *testing.T) []byte{
		"ECDSA P-256, PKCS #8": func(t *testing.T) []byte { return pkcs8(t, ecKey(t, elliptic.P256())) },
		"ECDSA P-256, SEC 1 after its parameters": func(t *testing.T) []byte {
			return sec1WithParameters(t, ecKey(t, elliptic.P256()))
		},
		"ECDSA P-384, PKCS #8": func(t *testing.T) []byte { return pkcs8(t, ecKey(t, elliptic.P384())) },
		"ECDSA P-521, PKCS #8": func(t *testing.T) []byte { return pkcs8(t, ecKey(t, elliptic.P521())) },
		"RSA, PKCS #1": func(t *testing.T) []byte {
			return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey(t, 2048))})
		},
		"RSA, PKCS #8":     func(t *testing.T) []byte { return pkcs8(t, rsaKey(t, 2048)) },
		"Ed25519, PKCS #8": func(t *testing.T) []byte { return pkcs8(t, ed25519Key(t)) },
	}
	for name, keyPEM := range testCases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, map[string][]byte{"key.pem": keyPEM(t)})
				jwks := f.output(t, "jwks", "-key", "key.pem")
				token := f.output(t, tokenArgs("alice", server.ScopeWrite, server.ScopeRead)...)

				claims, err := verifier(t, jwks).Verify(context.Background(), token)
				require.NoError(t, err)
				require.Equal(t, "alice", claims.Subject)
				require.Equal(t, "wallet:write wallet:read", claims.Scope)
				require.True(t, claims.HasScope(server.ScopeWrite))
			})
		})
	}
}

func TestInit_a_key_set_made_from_another_key_rejects_the_token(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signing := newFixture(t, nil)
		other := newFixture(t, nil)
		token := signing.output(t, tokenArgs("alice", server.ScopeWrite)...)
		jwks := other.output(t, "jwks", "-key", "key.pem")

		_, err := verifier(t, jwks).Verify(context.Background(), token)
		require.ErrorIs(t, err, auth.ErrUnauthenticated)
	})
}

func TestInit_jwks_prints_the_public_half_with_the_key_s_thumbprint_as_its_kid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		key := ecKey(t, elliptic.P256())
		f := newFixture(t, map[string][]byte{"key.pem": pkcs8(t, key)})
		out := f.output(t, "jwks", "-key", "key.pem")

		var set struct {
			Keys []map[string]string `json:"keys"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &set))
		require.Len(t, set.Keys, 1)

		coordinate := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
		public, err := key.PublicKey.Bytes()
		require.NoError(t, err)
		x, y := coordinate(public[1:33]), coordinate(public[33:])
		canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":%q,"y":%q}`, x, y)
		sum := sha256.Sum256([]byte(canonical))

		require.Equal(t, map[string]string{
			"kty": "EC",
			"crv": "P-256",
			"x":   x,
			"y":   y,
			"kid": coordinate(sum[:]),
			"alg": "ES256",
			"use": "sig",
		}, set.Keys[0])
	})
}

func TestInit_token_names_the_key_s_kid_and_lasts_its_ttl(t *testing.T) {
	testCases := map[string]struct {
		args []string
		want time.Duration
	}{
		"by default":   {want: time.Hour},
		"with -ttl 5m": {args: []string{"-ttl", "5m"}, want: 5 * time.Minute},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, map[string][]byte{"key.pem": pkcs8(t, ed25519Key(t))})
				jwks := f.output(t, "jwks", "-key", "key.pem")
				token := f.output(t, append(tokenArgs("bob"), tc.args...)...)

				var set jose.JSONWebKeySet
				require.NoError(t, json.Unmarshal([]byte(jwks), &set))
				require.Len(t, set.Keys, 1)

				parsed, err := josejwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.EdDSA})
				require.NoError(t, err)
				require.Len(t, parsed.Headers, 1)
				require.Equal(t, set.Keys[0].KeyID, parsed.Headers[0].KeyID)
				require.Equal(t, "JWT", parsed.Headers[0].ExtraHeaders[jose.HeaderType])

				var claims struct {
					josejwt.Claims
					Scope *string `json:"scope"`
				}
				require.NoError(t, parsed.Claims(set.Keys[0].Key, &claims))
				require.Nil(t, claims.Scope)
				require.WithinDuration(t, time.Now(), claims.IssuedAt.Time(), 0)
				require.WithinDuration(t, time.Now(), claims.NotBefore.Time(), 0)
				require.WithinDuration(t, time.Now().Add(tc.want), claims.Expiry.Time(), 0)

				v := verifier(t, jwks)
				_, err = v.Verify(context.Background(), token)
				require.NoError(t, err)
				time.Sleep(tc.want + jwt.DefaultSettings().Leeway + time.Second)
				_, err = v.Verify(context.Background(), token)
				require.ErrorIs(t, err, auth.ErrUnauthenticated)
			})
		})
	}
}

func TestInit_open_balance_and_send_call_the_wallet_as_the_subject_with_the_scope_each_needs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		w := newFakeWallet()
		f.startWallet(t, w)

		require.Equal(t, "balance 0", f.output(t, walletArgs("open", "alice")...))
		w.mu.Lock()
		w.accounts["alice"] = 2500
		w.mu.Unlock()
		require.Equal(t, "balance 2500", f.output(t, walletArgs("balance", "alice")...))
		require.Equal(t, "transfer alice/r-1: 1000 from alice to bob",
			f.output(t, walletArgs("send", "alice", "-to", "bob", "-amount", "1000", "-id", "r-1")...))
		require.Equal(t, "balance 1500", f.output(t, walletArgs("balance", "alice")...))

		require.Equal(t, []call{
			{method: "OpenAccount", subject: "alice", scopes: []string{server.ScopeWrite}},
			{method: "GetAccount", subject: "alice", scopes: []string{server.ScopeRead}},
			{method: "Send", subject: "alice", scopes: []string{server.ScopeWrite}},
			{method: "GetAccount", subject: "alice", scopes: []string{server.ScopeRead}},
		}, w.recorded())
	})
}

func TestInit_send_without_an_id_sends_a_new_one_each_time(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		w := newFakeWallet()
		f.startWallet(t, w)

		for range 2 {
			f.output(t, walletArgs("send", "alice", "-to", "bob", "-amount", "1")...)
		}

		w.mu.Lock()
		defer w.mu.Unlock()
		require.Len(t, w.sends, 2)
		require.NotEmpty(t, w.sends[0].GetRequestId())
		require.NotEmpty(t, w.sends[1].GetRequestId())
		require.NotEqual(t, w.sends[0].GetRequestId(), w.sends[1].GetRequestId())
	})
}

func TestInit_exits_1_with_what_the_wallet_answered(t *testing.T) {
	testCases := map[string]struct {
		args    []string
		sendErr error
		want    string
	}{
		"balance of an account that isn't open": {
			args: walletArgs("balance", "carol"),
			want: "no such account",
		},
		"a send the wallet refuses": {
			args:    walletArgs("send", "alice", "-to", "bob", "-amount", "1"),
			sendErr: status.Error(codes.FailedPrecondition, "alice's balance is short"),
			want:    "alice's balance is short",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var got run
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, nil)
				w := newFakeWallet()
				w.sendErr = tc.sendErr
				f.startWallet(t, w)
				got = f.start(t, telemetry.NoopBuilder, tc.args...)
			})
			require.Equal(t, 1, got.code, "cause: %v", got.cause)
			require.Empty(t, got.stdout)
			require.ErrorContains(t, got.cause, tc.want)
		})
	}
}

func TestInit_a_token_signed_with_another_key_is_Unauthenticated(t *testing.T) {
	var got run
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, map[string][]byte{"other.pem": pkcs8(t, ecKey(t, elliptic.P256()))})
		w := newFakeWallet()
		f.startWallet(t, w)
		args := walletArgs("balance", "alice")
		for i, arg := range args {
			if arg == "key.pem" {
				args[i] = "other.pem"
			}
		}
		got = f.start(t, telemetry.NoopBuilder, args...)
	})
	require.Equal(t, 1, got.code, "cause: %v", got.cause)
	require.ErrorContains(t, got.cause, "code = Unauthenticated")
}

func TestInit_watch_prints_each_balance_and_watches_again_when_its_watch_ends_Unavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		w := newFakeWallet()
		w.scripts = []watchScript{
			{balances: []int64{2500, 1500}, end: status.Error(codes.Unavailable, "server is shutting down")},
			{end: status.Error(codes.Unavailable, "the ledger is unavailable")},
			{balances: []int64{1500}},
		}
		f.startWallet(t, w)

		p, exit := f.begin(t, telemetry.NoopBuilder, walletArgs("watch", "alice")...)
		for range 3 {
			<-w.watching
		}
		synctest.Wait()
		require.Equal(t, "balance 2500\nbalance 1500\nbalance 1500\n", p.Stdout())

		w.mu.Lock()
		opened := append([]time.Time(nil), w.opened...)
		w.mu.Unlock()
		require.Len(t, opened, 3)
		require.GreaterOrEqual(t, opened[1].Sub(opened[0]), rewatchDelay)
		require.GreaterOrEqual(t, opened[2].Sub(opened[1]), rewatchDelay)
		for _, c := range w.recorded() {
			require.Equal(t, call{method: "WatchAccount", subject: "alice", scopes: []string{server.ScopeRead}}, c)
		}

		select {
		case st := <-exit:
			t.Fatalf("walletctl watch exited early: %v", st.Cause())
		case <-p.Notified(syscall.SIGTERM):
		}
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func TestInit_watch_exits_1_when_its_watch_ends_any_other_way(t *testing.T) {
	var got run
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		w := newFakeWallet()
		w.scripts = []watchScript{
			{balances: []int64{2500}, end: status.Error(codes.NotFound, "no such account")},
		}
		f.startWallet(t, w)
		got = f.start(t, telemetry.NoopBuilder, walletArgs("watch", "alice")...)
	})
	require.Equal(t, 1, got.code, "cause: %v", got.cause)
	require.Equal(t, "balance 2500\n", got.stdout)
	require.ErrorContains(t, got.cause, "no such account")
}

func TestInit_deposit_calls_the_ledger_with_the_client_certificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		l := &fakeLedger{}
		f.startLedger(t, l)

		out := f.output(t, depositArgs("-account", "alice", "-amount", "2500", "-id", "d-1")...)
		require.Equal(t, "transfer d-1: 2500 from external to alice", out)
		f.output(t, depositArgs("-account", "alice", "-amount", "1")...)

		l.mu.Lock()
		defer l.mu.Unlock()
		require.Len(t, l.deposits, 2)
		require.Equal(t, "alice", l.deposits[0].GetAccountId())
		require.EqualValues(t, 2500, l.deposits[0].GetAmountCents())
		require.Equal(t, "d-1", l.deposits[0].GetTransferId())
		require.NotEmpty(t, l.deposits[1].GetTransferId())
		require.NotEqual(t, "d-1", l.deposits[1].GetTransferId())
		for _, raw := range l.peers {
			require.True(t, bytes.Equal(f.client.TLS().Certificate[0], raw))
		}
	})
}

func TestInit_server_name_is_the_name_the_server_s_certificate_is_verified_for(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		l := &fakeLedger{}
		f.serve(t, "localhost:9090", &tls.Config{
			Certificates: []tls.Certificate{f.ca.Issue(t, "ledger").TLS()},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    f.ca.Pool(),
			MinVersion:   tls.VersionTLS12,
		}, func(r grpc.ServiceRegistrar) { ledgerpb.RegisterLedgerServer(r, l) })

		args := []string{"deposit", "-addr", "localhost:9090", "-ca", "ca.pem", "-cert", "client.pem", "-account", "alice", "-amount", "1"}
		got := f.start(t, telemetry.NoopBuilder, args...)
		require.Equal(t, 1, got.code, "cause: %v", got.cause)

		f.output(t, append(args, "-server-name", "ledger")...)
	})
}

func TestInit_exits_ExitConfig_on_a_bad_setting(t *testing.T) {
	testCases := map[string]struct {
		files map[string][]byte
		args  []string
		want  []string
	}{
		"no command": {
			want: []string{"config command: not found"},
		},
		"an unknown command": {
			args: []string{"verify"},
			want: []string{`config command: "verify" isn't one of jwks, token, open, balance, send, watch or deposit`},
		},
		"jwks with no key": {
			args: []string{"jwks"},
			want: []string{"config -key: not found"},
		},
		"jwks with a flag of token's": {
			args: []string{"jwks", "-key", "key.pem", "-sub", "alice"},
			want: []string{"config arguments: flag provided but not defined: -sub"},
		},
		"an argument after the flags": {
			args: []string{"jwks", "-key", "key.pem", "extra"},
			want: []string{`config arguments: unexpected argument "extra"`},
		},
		"token with nothing": {
			args: []string{"token"},
			want: []string{
				"config -key: not found",
				"config -sub: not found",
				"config -iss: not found",
				"config -aud: not found",
			},
		},
		"token with empty values": {
			args: []string{"token", "-key", "key.pem", "-sub", "", "-iss", "", "-aud", "", "-scope", "", "-ttl", "0s"},
			want: []string{
				"config -sub: must not be empty",
				"config -iss:",
				"config -aud:",
				`config -scope: "" is empty or has whitespace`,
				"config -ttl: must be positive",
			},
		},
		"a scope with a space": {
			args: tokenArgs("alice", "wallet:read wallet:write"),
			want: []string{`config -scope: "wallet:read wallet:write" is empty or has whitespace`},
		},
		"a ttl that isn't a duration": {
			args: append(tokenArgs("alice"), "-ttl", "an hour"),
			want: []string{"config -ttl:"},
		},
		"open with nothing": {
			args: []string{"open"},
			want: []string{
				"config -addr: not found",
				"config -ca: not found",
				"config -key: not found",
				"config -sub: not found",
				"config -iss: not found",
				"config -aud: not found",
			},
		},
		"balance with a scope": {
			args: walletArgs("balance", "alice", "-scope", server.ScopeWrite),
			want: []string{"config arguments: flag provided but not defined: -scope"},
		},
		"send with no recipient or amount": {
			args: walletArgs("send", "alice"),
			want: []string{
				"config -to: not found",
				"config -amount: not found",
			},
		},
		"send with an amount that isn't positive": {
			args: walletArgs("send", "alice", "-to", "bob", "-amount", "0"),
			want: []string{"config -amount: must be positive"},
		},
		"send with an amount that isn't a number": {
			args: walletArgs("send", "alice", "-to", "bob", "-amount", "10.50"),
			want: []string{"config -amount: must be a whole number of cents"},
		},
		"send with an empty id": {
			args: walletArgs("send", "alice", "-to", "bob", "-amount", "1", "-id", ""),
			want: []string{"config -id: must not be empty"},
		},
		"watch with an address that isn't host:port": {
			args: []string{"watch", "-addr", "wallet", "-ca", "ca.pem", "-key", "key.pem", "-sub", "alice", "-iss", issuer, "-aud", audience},
			want: []string{"config -addr:"},
		},
		"deposit with nothing": {
			args: []string{"deposit"},
			want: []string{
				"config -addr: not found",
				"config -ca: not found",
				"config -cert: not found",
				"config -account: not found",
				"config -amount: not found",
			},
		},
		"deposit with a key": {
			args: depositArgs("-account", "alice", "-amount", "1", "-key", "key.pem"),
			want: []string{"config arguments: flag provided but not defined: -key"},
		},
		"a CA file with no certificate": {
			args: depositArgs("-account", "alice", "-amount", "1", "-ca", "key.pem"),
			want: []string{"config -ca: no PEM certificate in the file"},
		},
		"a client certificate file with no certificate": {
			args: depositArgs("-account", "alice", "-amount", "1", "-cert", "key.pem"),
			want: []string{"config -cert:"},
		},
		"a key file that doesn't exist": {
			args: []string{"jwks", "-key", "missing.pem"},
			want: []string{"config -key:", "no such file"},
		},
		"a key file with no private key": {
			files: map[string][]byte{"key.pem": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("public")})},
			args:  []string{"jwks", "-key", "key.pem"},
			want:  []string{"config -key: no PEM private key in the file"},
		},
		"a private key that doesn't parse": {
			files: map[string][]byte{"key.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a key")})},
			args:  []string{"jwks", "-key", "key.pem"},
			want:  []string{"config -key:"},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var got run
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, tc.files)
				got = f.start(t, telemetry.NoopBuilder, tc.args...)
			})
			require.Equal(t, humus.ExitConfig, got.code, "cause: %v", got.cause)
			require.Empty(t, got.stdout)
			for _, want := range tc.want {
				require.ErrorContains(t, got.cause, want)
			}
		})
	}
}

func TestInit_exits_ExitConfig_on_a_key_it_cant_sign_with(t *testing.T) {
	testCases := map[string]struct {
		key  func(t *testing.T) []byte
		want string
	}{
		"two private keys": {
			key:  func(t *testing.T) []byte { return append(pkcs8(t, ed25519Key(t)), pkcs8(t, ed25519Key(t))...) },
			want: "config -key: more than one private key in the file",
		},
		"an RSA key under 2048 bits": {
			key:  func(t *testing.T) []byte { return pkcs8(t, rsaKey(t, 1024)) },
			want: "config -key: the RSA key has 1024 bits, fewer than 2048",
		},
		"an ECDSA key on another curve": {
			key:  func(t *testing.T) []byte { return pkcs8(t, ecKey(t, elliptic.P224())) },
			want: "config -key: the curve P-224 isn't P-256, P-384 or P-521",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var got run
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, map[string][]byte{"key.pem": tc.key(t)})
				got = f.start(t, telemetry.NoopBuilder, "jwks", "-key", "key.pem")
			})
			require.Equal(t, humus.ExitConfig, got.code, "cause: %v", got.cause)
			require.ErrorContains(t, got.cause, tc.want)
		})
	}
}

func TestBuildTelemetry_writes_the_logs_to_stderr_and_leaves_stdout_to_the_output(t *testing.T) {
	var got run
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, nil)
		got = f.start(t, buildTelemetry, "jwks")
	})
	require.Equal(t, humus.ExitConfig, got.code, "cause: %v", got.cause)
	require.Empty(t, got.stdout)
	require.Contains(t, got.stderr, `"severity_text":"ERROR"`)
	require.Contains(t, got.stderr, "config -key: not found")
}
