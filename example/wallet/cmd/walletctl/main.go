// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command walletctl makes the wallet's key set, signs its tokens, and calls the wallet and the ledger by hand.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/z5labs/bedrock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/wallet/ledgerpb"
	"github.com/z5labs/humus/example/wallet/walletpb"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

const rewatchDelay = time.Second

func main() {
	humus.Main(Init, humus.WithTelemetry(buildTelemetry))
}

func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "walletctl"
	return telemetry.New(ctx, s, dial, p.Stderr)
}

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	set, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}
	run, err := newRun(s, set)
	if err != nil {
		return nil, err
	}
	return []humus.Service{job.NewService(s, run)}, nil
}

func newRun(s *humus.Setup, set settings) (func(context.Context) error, error) {
	out := s.Process.Stdout
	switch set.Command {
	case jwksCommand:
		return func(context.Context) error {
			b, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{set.Key.Public()}})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(out, string(b))
			return err
		}, nil
	case tokenCommand:
		return func(context.Context) error {
			token, err := sign(set.Key, set.Token, time.Now())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(out, token)
			return err
		}, nil
	case depositCommand:
		conn, err := grpcclient.NewClient(s, set.Client)
		if err != nil {
			return nil, err
		}
		ledger := ledgerpb.NewLedgerClient(conn)
		return func(ctx context.Context) error {
			resp, err := ledger.Deposit(ctx, &ledgerpb.DepositRequest{
				TransferId:  set.ID,
				AccountId:   set.Account,
				AmountCents: set.AmountCents,
			})
			if err != nil {
				return err
			}
			t := resp.GetTransfer()
			return printTransfer(out, t.GetId(), t.GetFromAccountId(), t.GetToAccountId(), t.GetAmountCents())
		}, nil
	}

	conn, err := grpcclient.NewClient(s, set.Client, grpc.WithPerRPCCredentials(bearer{key: set.Key, token: set.Token}))
	if err != nil {
		return nil, err
	}
	wallet := walletpb.NewWalletClient(conn)
	switch set.Command {
	case openCommand:
		return func(ctx context.Context) error {
			resp, err := wallet.OpenAccount(ctx, &walletpb.OpenAccountRequest{})
			if err != nil {
				return err
			}
			return printBalance(out, resp.GetAccount())
		}, nil
	case balanceCommand:
		return func(ctx context.Context) error {
			resp, err := wallet.GetAccount(ctx, &walletpb.GetAccountRequest{})
			if err != nil {
				return err
			}
			return printBalance(out, resp.GetAccount())
		}, nil
	case sendCommand:
		return func(ctx context.Context) error {
			resp, err := wallet.Send(ctx, &walletpb.SendRequest{
				RequestId:   set.ID,
				ToAccountId: set.To,
				AmountCents: set.AmountCents,
			})
			if err != nil {
				return err
			}
			t := resp.GetTransfer()
			return printTransfer(out, t.GetId(), t.GetFromAccountId(), t.GetToAccountId(), t.GetAmountCents())
		}, nil
	}
	return func(ctx context.Context) error { return watch(ctx, wallet, out) }, nil
}

func watch(ctx context.Context, wallet walletpb.WalletClient, out io.Writer) error {
	for {
		err := watchOnce(ctx, wallet, out)
		switch {
		case ctx.Err() != nil:
			return nil
		case status.Code(err) != codes.Unavailable:
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(rewatchDelay):
		}
	}
}

func watchOnce(ctx context.Context, wallet walletpb.WalletClient, out io.Writer) error {
	stream, err := wallet.WatchAccount(ctx, &walletpb.WatchAccountRequest{})
	if err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return errors.New("the wallet ended the watch")
		}
		if err != nil {
			return err
		}
		if err := printBalance(out, resp.GetAccount()); err != nil {
			return err
		}
	}
}

func printBalance(out io.Writer, account *walletpb.Account) error {
	_, err := fmt.Fprintf(out, "balance %d\n", account.GetBalanceCents())
	return err
}

func printTransfer(out io.Writer, id, from, to string, amountCents int64) error {
	_, err := fmt.Fprintf(out, "transfer %s: %d from %s to %s\n", id, amountCents, from, to)
	return err
}

type tokenSettings struct {
	Subject  string
	Scopes   []string
	Issuer   string
	Audience string
	TTL      time.Duration
}

func sign(key jose.JSONWebKey, s tokenSettings, now time.Time) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.SignatureAlgorithm(key.Algorithm), Key: key},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", err
	}
	claims := struct {
		josejwt.Claims
		Scope string `json:"scope,omitempty"`
	}{
		Claims: josejwt.Claims{
			Issuer:    s.Issuer,
			Audience:  josejwt.Audience{s.Audience},
			Subject:   s.Subject,
			IssuedAt:  josejwt.NewNumericDate(now),
			NotBefore: josejwt.NewNumericDate(now),
			Expiry:    josejwt.NewNumericDate(now.Add(s.TTL)),
		},
		Scope: strings.Join(s.Scopes, " "),
	}
	return josejwt.Signed(signer).Claims(claims).Serialize()
}

type bearer struct {
	key   jose.JSONWebKey
	token tokenSettings
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token, err := sign(b.key, b.token, time.Now())
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

func (bearer) RequireTransportSecurity() bool {
	return true
}
