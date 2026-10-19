// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package billing issues a month of invoices and charges every pending one.
package billing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/z5labs/humus/example/billing/payments"
	"github.com/z5labs/humus/example/billing/postgres"
)

type Invoices interface {
	Issue(ctx context.Context, period time.Time) (issued int64, err error)
	Pending(ctx context.Context) ([]postgres.Invoice, error)
	SettleCharged(ctx context.Context, id uuid.UUID, chargeID string) (settled bool, err error)
	SettleDeclined(ctx context.Context, id uuid.UUID, declineCode string) (settled bool, err error)
	SettleFailed(ctx context.Context, id uuid.UUID, answer string) (settled bool, err error)
}

type Run struct {
	Invoices    Invoices
	Payments    *payments.Client
	Period      time.Time
	Concurrency int
}

func (r Run) Run(ctx context.Context) error {
	if r.Concurrency < 1 {
		return fmt.Errorf("billing with a concurrency of %d: it must be at least 1", r.Concurrency)
	}

	if _, err := r.Invoices.Issue(ctx, r.period()); err != nil {
		return err
	}

	invoices, err := r.Invoices.Pending(ctx)
	if err != nil {
		return err
	}

	var (
		mu      sync.Mutex
		started int
		why     []error
		halted  bool
	)
	halt := make(chan struct{})
	stopped := func() bool {
		select {
		case <-halt:
			return true
		case <-ctx.Done():
			return true
		default:
			return false
		}
	}

	work := make(chan postgres.Invoice)
	var wg sync.WaitGroup
	for range min(r.Concurrency, len(invoices)) {
		wg.Go(func() {
			for inv := range work {
				mu.Lock()
				if stopped() {
					mu.Unlock()
					continue
				}
				started++
				mu.Unlock()

				stop, err := r.charge(ctx, inv)
				if err == nil {
					continue
				}
				mu.Lock()
				why = append(why, err)
				if stop && !halted {
					halted = true
					close(halt)
				}
				mu.Unlock()
			}
		})
	}
hand:
	for _, inv := range invoices {
		if stopped() {
			break
		}
		select {
		case work <- inv:
		case <-halt:
			break hand
		case <-ctx.Done():
			break hand
		}
	}
	close(work)
	wg.Wait()

	left := len(invoices) - started + len(why)
	if left == 0 {
		return nil
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if halted {
		return fmt.Errorf("stopped early, leaving %d of %d invoices pending: %w", left, len(invoices), errors.Join(why...))
	}
	return fmt.Errorf("left %d of %d invoices pending: %w", left, len(invoices), errors.Join(why...))
}

func (r Run) period() time.Time {
	if !r.Period.IsZero() {
		return time.Date(r.Period.Year(), r.Period.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
}

func (r Run) charge(ctx context.Context, inv postgres.Invoice) (stop bool, err error) {
	res, err := r.Payments.Charge(ctx, payments.Charge{
		ID:          inv.ID,
		AccountID:   inv.AccountID,
		AmountCents: inv.AmountCents,
		Currency:    inv.Currency,
	})
	if err != nil {
		return errors.Is(err, payments.ErrStop), err
	}
	switch res.State {
	case payments.Charged:
		_, err = r.Invoices.SettleCharged(ctx, inv.ID, res.ChargeID)
	case payments.Declined:
		_, err = r.Invoices.SettleDeclined(ctx, inv.ID, res.DeclineCode)
	case payments.Failed:
		_, err = r.Invoices.SettleFailed(ctx, inv.ID, res.Answer)
	default:
		return false, fmt.Errorf("charging invoice %s: the payments client answered an unknown state %d", inv.ID, res.State)
	}
	return err != nil, err
}
