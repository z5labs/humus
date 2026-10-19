// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/example/orderstatus/projector"
	"github.com/z5labs/humus/example/orderstatus/store"
	"github.com/z5labs/humus/fault"
)

const storePoint = "postgresql:orders"

type faultyStore struct {
	store  projector.Store
	faults fault.Injector
}

func (f faultyStore) inject(ctx context.Context) error {
	got := f.faults.Eval(ctx, storePoint)
	if err := got.Wait(ctx); err != nil {
		return &fault.InjectedError{Point: storePoint, Err: err}
	}
	if got.Abort {
		return &fault.InjectedError{Point: storePoint}
	}
	return nil
}

func (f faultyStore) Applied(ctx context.Context, partition int32, offset int64) (bool, error) {
	if err := f.inject(ctx); err != nil {
		return false, err
	}
	return f.store.Applied(ctx, partition, offset)
}

func (f faultyStore) Events(ctx context.Context, orderID string) ([]store.Event, error) {
	if err := f.inject(ctx); err != nil {
		return nil, err
	}
	return f.store.Events(ctx, orderID)
}

func (f faultyStore) Apply(ctx context.Context, e store.Event, status store.Status) error {
	if err := f.inject(ctx); err != nil {
		return err
	}
	return f.store.Apply(ctx, e, status)
}
