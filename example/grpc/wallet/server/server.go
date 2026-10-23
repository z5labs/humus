// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package server is the wallet.v1.Wallet service, on the ledger's client.
package server

import (
	"context"
	"errors"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/example/grpc/wallet/ledgerpb"
	"github.com/z5labs/humus/example/grpc/wallet/walletpb"
	"github.com/z5labs/humus/telemetry"
)

const scope = "github.com/z5labs/humus/example/grpc/wallet/server"

const (
	ScopeRead  = "wallet:read"
	ScopeWrite = "wallet:write"
)

const (
	reasonLedgerUnavailable = "LEDGER_UNAVAILABLE"
	reasonLedgerShed        = "LEDGER_SHED"
)

type Claims struct {
	Subject string `json:"sub"`
	Scope   string `json:"scope"`
}

func (c Claims) HasScope(scope string) bool {
	return slices.Contains(strings.Fields(c.Scope), scope)
}

type Server struct {
	walletpb.UnimplementedWalletServer

	ledger   ledgerpb.LedgerClient
	bearer   auth.Scheme[Claims]
	logger   log.Logger
	reopened metric.Int64Counter
}

func New(ledger ledgerpb.LedgerClient, bearer auth.Scheme[Claims], tel telemetry.SDK) (*Server, error) {
	reopened, err := tel.MeterProvider.Meter(scope).Int64Counter("wallet.watches.reopened",
		metric.WithDescription("Watches of the ledger the wallet opened again."),
		metric.WithUnit("{watch}"))
	if err != nil {
		return nil, err
	}
	return &Server{
		ledger:   ledger,
		bearer:   bearer,
		logger:   tel.LoggerProvider.Logger(scope),
		reopened: reopened,
	}, nil
}

func (s *Server) OpenAccount(ctx context.Context, _ *walletpb.OpenAccountRequest) (*walletpb.OpenAccountResponse, error) {
	id, err := s.subject(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := s.ledger.OpenAccount(ctx, &ledgerpb.OpenAccountRequest{AccountId: id})
	if err != nil {
		return nil, s.answer(ctx, err)
	}
	return &walletpb.OpenAccountResponse{Account: account(resp.GetAccount())}, nil
}

func (s *Server) GetAccount(ctx context.Context, _ *walletpb.GetAccountRequest) (*walletpb.GetAccountResponse, error) {
	id, err := s.subject(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := s.ledger.GetAccount(ctx, &ledgerpb.GetAccountRequest{AccountId: id})
	if err != nil {
		return nil, s.answer(ctx, err)
	}
	return &walletpb.GetAccountResponse{Account: account(resp.GetAccount())}, nil
}

func (s *Server) Send(ctx context.Context, req *walletpb.SendRequest) (*walletpb.SendResponse, error) {
	id, err := s.subject(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetRequestId() == "" {
		return nil, withDetails(codes.InvalidArgument, "the request is missing a required field", &errdetails.BadRequest{
			FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "request_id", Description: "is required"}},
		})
	}
	resp, err := s.ledger.Transfer(ctx, &ledgerpb.TransferRequest{
		TransferId:    transferID(id, req.GetRequestId()),
		FromAccountId: id,
		ToAccountId:   req.GetToAccountId(),
		AmountCents:   req.GetAmountCents(),
	})
	if err != nil {
		return nil, s.answer(ctx, err)
	}
	return &walletpb.SendResponse{Transfer: transfer(resp.GetTransfer())}, nil
}

func (s *Server) WatchAccount(_ *walletpb.WatchAccountRequest, stream grpc.ServerStreamingServer[walletpb.WatchAccountResponse]) error {
	ctx := stream.Context()
	id, err := s.subject(ctx)
	if err != nil {
		return err
	}
	var last *ledgerpb.Account
	for {
		received, err := s.relay(ctx, id, stream, &last)
		if sent, ok := errors.AsType[*sendError](err); ok {
			return sent.err
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case received && status.Code(err) == codes.Unavailable && ctx.Err() == nil:
			s.reopened.Add(ctx, 1)
			continue
		}
		return s.answer(ctx, err)
	}
}

type sendError struct{ err error }

func (e *sendError) Error() string { return e.err.Error() }

func (s *Server) relay(ctx context.Context, id string, stream grpc.ServerStreamingServer[walletpb.WatchAccountResponse], last **ledgerpb.Account) (received bool, err error) {
	watch, err := s.ledger.WatchAccount(ctx, &ledgerpb.WatchAccountRequest{AccountId: id})
	if err != nil {
		return false, err
	}
	for {
		resp, err := watch.Recv()
		if err != nil {
			return received, err
		}
		received = true
		if proto.Equal(resp.GetAccount(), *last) {
			continue
		}
		if err := stream.Send(&walletpb.WatchAccountResponse{Account: account(resp.GetAccount())}); err != nil {
			return received, &sendError{err: err}
		}
		*last = resp.GetAccount()
	}
}

func (s *Server) subject(ctx context.Context) (string, error) {
	claims, ok := s.bearer.From(ctx)
	if !ok || claims.Subject == "" {
		return "", status.Error(codes.Unauthenticated, "the request has no bearer token with a subject")
	}
	return claims.Subject, nil
}

func transferID(subject, requestID string) string {
	return url.PathEscape(subject) + "/" + requestID
}

func (s *Server) answer(ctx context.Context, err error) error {
	answered := answer(ctx, err)
	if status.Code(answered) == codes.Unavailable {
		s.warn(ctx, err)
	}
	return answered
}

func (s *Server) warn(ctx context.Context, err error) {
	method, _ := grpc.Method(ctx)
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(log.SeverityWarn)
	r.SetSeverityText("WARN")
	r.SetBody(attribute.StringValue("the wallet answered Unavailable for the ledger"))
	r.AddAttributes(semconv.RPCMethod(strings.TrimPrefix(method, "/")), semconv.ExceptionMessage(err.Error()))
	s.logger.Emit(context.WithoutCancel(ctx), r)
}

func answer(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return status.FromContextError(ctxErr).Err()
	}
	if rejected, ok := errors.AsType[*circuit.RejectedError](err); ok {
		return withDetails(codes.Unavailable, "the ledger is shedding calls; try again later",
			errorInfo(reasonLedgerShed),
			&errdetails.RetryInfo{RetryDelay: durationpb.New(rejected.RetryAfter)},
		)
	}
	switch status.Code(err) {
	case codes.NotFound, codes.FailedPrecondition, codes.InvalidArgument, codes.AlreadyExists:
		return status.Convert(err).Err()
	}
	return withDetails(codes.Unavailable, "the ledger can't be reached", errorInfo(reasonLedgerUnavailable))
}

func errorInfo(reason string) *errdetails.ErrorInfo {
	return &errdetails.ErrorInfo{Reason: reason, Domain: walletpb.Wallet_ServiceDesc.ServiceName}
}

func withDetails(code codes.Code, msg string, details ...protoadapt.MessageV1) error {
	st, err := status.New(code, msg).WithDetails(details...)
	if err != nil {
		return status.Error(code, msg)
	}
	return st.Err()
}

func account(a *ledgerpb.Account) *walletpb.Account {
	return &walletpb.Account{
		Id:                a.GetId(),
		BalanceCents:      a.GetBalanceCents(),
		BalanceChangeTime: a.GetBalanceChangeTime(),
	}
}

func transfer(t *ledgerpb.Transfer) *walletpb.Transfer {
	return &walletpb.Transfer{
		Id:            t.GetId(),
		FromAccountId: t.GetFromAccountId(),
		ToAccountId:   t.GetToAccountId(),
		AmountCents:   t.GetAmountCents(),
		CreateTime:    t.GetCreateTime(),
	}
}
