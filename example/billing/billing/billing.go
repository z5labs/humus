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

	work := make(chan postgres.Invoice)
	pending := make(chan error, len(invoices))
	var wg sync.WaitGroup
	for range min(r.Concurrency, len(invoices)) {
		wg.Go(func() {
			for inv := range work {
				if err := r.charge(ctx, inv); err != nil {
					pending <- err
				}
			}
		})
	}
	for _, inv := range invoices {
		work <- inv
	}
	close(work)
	wg.Wait()
	close(pending)

	var why []error
	for err := range pending {
		why = append(why, err)
	}
	if len(why) == 0 {
		return nil
	}
	return fmt.Errorf("left %d of %d invoices pending: %w", len(why), len(invoices), errors.Join(why...))
}

func (r Run) period() time.Time {
	if !r.Period.IsZero() {
		return time.Date(r.Period.Year(), r.Period.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
}

func (r Run) charge(ctx context.Context, inv postgres.Invoice) error {
	res, err := r.Payments.Charge(ctx, payments.Charge{
		ID:          inv.ID,
		AccountID:   inv.AccountID,
		AmountCents: inv.AmountCents,
		Currency:    inv.Currency,
	})
	if err != nil {
		return err
	}
	switch res.State {
	case payments.Charged:
		_, err = r.Invoices.SettleCharged(ctx, inv.ID, res.ChargeID)
	case payments.Declined:
		_, err = r.Invoices.SettleDeclined(ctx, inv.ID, res.DeclineCode)
	case payments.Failed:
		_, err = r.Invoices.SettleFailed(ctx, inv.ID, res.Answer)
	default:
		err = fmt.Errorf("charging invoice %s: the payments client answered an unknown state %d", inv.ID, res.State)
	}
	return err
}
