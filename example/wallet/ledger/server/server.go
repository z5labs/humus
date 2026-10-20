// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package server is the ledger.v1.Ledger service, on a Store.
package server

import (
	"context"
	"errors"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/z5labs/humus/example/wallet/ledger/store"
	"github.com/z5labs/humus/example/wallet/ledgerpb"
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
}

func New(st Store, watchInterval time.Duration) *Server {
	return &Server{store: st, watchInterval: watchInterval}
}

func (s *Server) OpenAccount(ctx context.Context, req *ledgerpb.OpenAccountRequest) (*ledgerpb.OpenAccountResponse, error) {
	if err := required(field{"account_id", req.GetAccountId()}); err != nil {
		return nil, err
	}
	a, err := s.store.OpenAccount(ctx, req.GetAccountId())
	if err != nil {
		return nil, answer(err, nil)
	}
	return &ledgerpb.OpenAccountResponse{Account: account(a)}, nil
}

func (s *Server) GetAccount(ctx context.Context, req *ledgerpb.GetAccountRequest) (*ledgerpb.GetAccountResponse, error) {
	if err := required(field{"account_id", req.GetAccountId()}); err != nil {
		return nil, err
	}
	a, err := s.store.Account(ctx, req.GetAccountId())
	if err != nil {
		return nil, answer(err, nil)
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
		return nil, answer(err, []invalid{
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
		return nil, err
	}
	t, err := s.store.Transfer(ctx, req.GetTransferId(), req.GetFromAccountId(), req.GetToAccountId(), req.GetAmountCents())
	if err != nil {
		return nil, answer(err, []invalid{
			{store.ErrNotPositive, "amount_cents", "must be more than zero"},
			{store.ErrSameAccount, "to_account_id", "is the account the transfer is from"},
		})
	}
	return &ledgerpb.TransferResponse{Transfer: transfer(t)}, nil
}

func (s *Server) WatchAccount(req *ledgerpb.WatchAccountRequest, stream grpc.ServerStreamingServer[ledgerpb.WatchAccountResponse]) error {
	if err := required(field{"account_id", req.GetAccountId()}); err != nil {
		return err
	}
	ctx := stream.Context()
	sent, err := s.store.Account(ctx, req.GetAccountId())
	if err != nil {
		return answer(err, nil)
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
			return answer(err, nil)
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
