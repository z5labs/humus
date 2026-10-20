// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package controller

import (
	"context"

	"github.com/z5labs/humus/example/databases/api/v1alpha1"
)

func (r Reconciler) Finalize(ctx context.Context, db database) error {
	if db.Spec.DeletionPolicy != v1alpha1.Delete {
		return nil
	}
	return r.Admin.Drop(ctx, db.Spec.Name)
}
