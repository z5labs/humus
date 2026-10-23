// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package server is the ledger.v1.Ledger service, on a Store.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/z5labs/humus/example/grpc/wallet/ledger/store"
	"github.com/z5labs/humus/example/grpc/wallet/ledgerpb"
	"github.com/z5labs/humus/telemetry"
)

const scope = "github.com/z5labs/humus/example/grpc/wallet/ledger/server"

const outcomeKey = attribute.Key("ledger.transfer.outcome")

const (
	outcomePosted   = "posted"
	outcomeReplayed = "replayed"
	outcomeRefused  = "refused"
)

type Store interface {
	OpenAccount(ctx context.Context, id string) (store.Account, error)
	Account(ctx context.Context, id string) (store.Account, error)
	Deposit(ctx context.Context, transferID, accountID string, amountCents int64) (store.Transfer, error)
	Transfer(ctx context.Context, transferID, fromAccountID, toAccountID string, amountCents int64) (store.Transfer, error)
}

type Server struct {
	ledgerpb.UnimplementedLedgerServer

	store         Store
	watchInterval time.Duration
	logger        log.Logger
	transfers     metric.Int64Counter
	watches       metric.Int64UpDownCounter
}

var ErrWatchInterval = errors.New("watch interval isn't positive")

func New(st Store, watchInterval time.Duration, tel telemetry.SDK) (*Server, error) {
	if watchInterval <= 0 {
		return nil, fmt.Errorf("%w: %s", ErrWatchInterval, watchInterval)
	}
	meter := tel.MeterProvider.Meter(scope)
	transfers, err := meter.Int64Counter("ledger.transfers",
		metric.WithDescription("Transfers the ledger answered, by outcome."),
		metric.WithUnit("{transfer}"))
	if err != nil {
		return nil, err
	}
	watches, err := meter.Int64UpDownCounter("ledger.watches",
		metric.WithDescription("Watches of an account open on the ledger."),
		metric.WithUnit("{watch}"))
	if err != nil {
		return nil, err
	}
	return &Server{
		store:         st,
		watchInterval: watchInterval,
		logger:        tel.LoggerProvider.Logger(scope),
		transfers:     transfers,
		watches:       watches,
	}, nil
}

func (s *Server) OpenAccount(ctx context.Context, req *ledgerpb.OpenAccountRequest) (*ledgerpb.OpenAccountResponse, error) {
	if err := required(field{"account_id", req.GetAccountId()}); err != nil {
		return nil, err
	}
	a, err := s.store.OpenAccount(ctx, req.GetAccountId())
	if err != nil {
		return nil, s.answer(ctx, err, nil)
	}
	return &ledgerpb.OpenAccountResponse{Account: account(a)}, nil
}

func (s *Server) GetAccount(ctx context.Context, req *ledgerpb.GetAccountRequest) (*ledgerpb.GetAccountResponse, error) {
	if err := required(field{"account_id", req.GetAccountId()}); err != nil {
		return nil, err
	}
	a, err := s.store.Account(ctx, req.GetAccountId())
	if err != nil {
		return nil, s.answer(ctx, err, nil)
	}
	return &ledgerpb.GetAccountResponse{Account: account(a)}, nil
}

func (s *Server) Deposit(ctx context.Context, req *ledgerpb.DepositRequest) (*ledgerpb.DepositResponse, error) {
	err := required(field{"transfer_id", req.GetTransferId()}, field{"account_id", req.GetAccountId()})
	if err != nil {
		return nil, err
	}
	t, err := s.store.Deposit(ctx, req.GetTransferId(), req.GetAccountId(), req.GetAmountCents())
	if err != nil {
		return nil, s.answer(ctx, err, []invalid{
			{store.ErrNotPositive, "amount_cents", "must be more than zero"},
			{store.ErrSameAccount, "account_id", "is the ledger's own " + store.External + " account, which deposits come from"},
		})
	}
	return &ledgerpb.DepositResponse{Transfer: transfer(t)}, nil
}

func (s *Server) Transfer(ctx context.Context, req *ledgerpb.TransferRequest) (*ledgerpb.TransferResponse, error) {
	err := required(
		field{"transfer_id", req.GetTransferId()},
		field{"from_account_id", req.GetFromAccountId()},
		field{"to_account_id", req.GetToAccountId()},
	)
	if err != nil {
		s.count(ctx, outcomeRefused)
		return nil, err
	}
	t, err := s.store.Transfer(ctx, req.GetTransferId(), req.GetFromAccountId(), req.GetToAccountId(), req.GetAmountCents())
	if err != nil {
		err = s.answer(ctx, err, []invalid{
			{store.ErrNotPositive, "amount_cents", "must be more than zero"},
			{store.ErrSameAccount, "to_account_id", "is the account the transfer is from"},
		})
		if refused(err) {
			s.count(ctx, outcomeRefused)
		}
		return nil, err
	}
	outcome := outcomePosted
	if t.Replayed {
		outcome = outcomeReplayed
	}
	s.count(ctx, outcome)
	return &ledgerpb.TransferResponse{Transfer: transfer(t)}, nil
}

func (s *Server) count(ctx context.Context, outcome string) {
	s.transfers.Add(ctx, 1, metric.WithAttributes(outcomeKey.String(outcome)))
}

func refused(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.NotFound, codes.FailedPrecondition, codes.AlreadyExists:
		return true
	}
	return false
}

func (s *Server) WatchAccount(req *ledgerpb.WatchAccountRequest, stream grpc.ServerStreamingServer[ledgerpb.WatchAccountResponse]) error {
	if err := required(field{"account_id", req.GetAccountId()}); err != nil {
		return err
	}
	ctx := stream.Context()
	s.watches.Add(ctx, 1)
	defer s.watches.Add(context.WithoutCancel(ctx), -1)
	sent, err := s.store.Account(ctx, req.GetAccountId())
	if err != nil {
		return s.answer(ctx, err, nil)
	}
	if err := stream.Send(&ledgerpb.WatchAccountResponse{Account: account(sent)}); err != nil {
		return err
	}

	ticker := time.NewTicker(s.watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-ticker.C:
		}
		a, err := s.store.Account(ctx, req.GetAccountId())
		if err != nil {
			return s.answer(ctx, err, nil)
		}
		if a.BalanceCents == sent.BalanceCents {
			continue
		}
		if err := stream.Send(&ledgerpb.WatchAccountResponse{Account: account(a)}); err != nil {
			return err
		}
		sent = a
	}
}

type field struct {
	name  string
	value string
}

func required(fields ...field) error {
	var violations []*errdetails.BadRequest_FieldViolation
	for _, f := range fields {
		if f.value == "" {
			violations = append(violations, &errdetails.BadRequest_FieldViolation{Field: f.name, Description: "is required"})
		}
	}
	if len(violations) == 0 {
		return nil
	}
	return withDetails(codes.InvalidArgument, "the request is missing a required field", &errdetails.BadRequest{FieldViolations: violations})
}

type invalid struct {
	err         error
	field       string
	description string
}

func (s *Server) answer(ctx context.Context, err error, invalids []invalid) error {
	answered := answer(err, invalids)
	switch status.Code(answered) {
	case codes.Unavailable, codes.Internal:
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
	r.SetBody(attribute.StringValue("the ledger's transaction failed"))
	r.AddAttributes(semconv.RPCMethod(strings.TrimPrefix(method, "/")), semconv.ExceptionMessage(err.Error()))
	s.logger.Emit(context.WithoutCancel(ctx), r)
}

func answer(err error, invalids []invalid) error {
	var short *store.ShortBalanceError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.As(err, &short):
		return withDetails(codes.FailedPrecondition, err.Error(), &errdetails.PreconditionFailure{
			Violations: []*errdetails.PreconditionFailure_Violation{{
				Type:        "BALANCE",
				Subject:     short.AccountID,
				Description: short.Error(),
			}},
		})
	case errors.Is(err, store.ErrReusedID):
		return status.Error(codes.AlreadyExists, err.Error())
	}
	for _, inv := range invalids {
		if errors.Is(err, inv.err) {
			return withDetails(codes.InvalidArgument, err.Error(), &errdetails.BadRequest{
				FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: inv.field, Description: inv.description}},
			})
		}
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case errors.Is(err, store.ErrUnavailable):
		return status.Error(codes.Unavailable, "the ledger's database can't be reached")
	}
	return status.Error(codes.Internal, "the ledger failed to answer the request")
}

func withDetails(code codes.Code, msg string, details ...protoadapt.MessageV1) error {
	st, err := status.New(code, msg).WithDetails(details...)
	if err != nil {
		return status.Error(code, msg)
	}
	return st.Err()
}

func account(a store.Account) *ledgerpb.Account {
	return &ledgerpb.Account{
		Id:                a.ID,
		BalanceCents:      a.BalanceCents,
		BalanceChangeTime: timestamppb.New(a.BalanceChangedAt),
	}
}

func transfer(t store.Transfer) *ledgerpb.Transfer {
	return &ledgerpb.Transfer{
		Id:            t.ID,
		FromAccountId: t.FromAccountID,
		ToAccountId:   t.ToAccountID,
		AmountCents:   t.AmountCents,
		CreateTime:    timestamppb.New(t.CreatedAt),
	}
}
