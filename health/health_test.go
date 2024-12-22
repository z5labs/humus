// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package health

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

type monitorFunc func(context.Context) (bool, error)

func (f monitorFunc) Healthy(ctx context.Context) (bool, error) {
	return f(ctx)
}

func TestAndMonitor_Healthy(t *testing.T) {
	t.Run("will return unhealthy", func(t *testing.T) {
		t.Run("if at least one of the Monitors return unhealthy", func(t *testing.T) {
			var a Binary
			a.MarkHealthy()

			var b Binary

			var c Binary
			c.MarkHealthy()

			and := And(&a, &b, &c)

			healthy, err := and.Healthy(context.Background())
			if !assert.Nil(t, err) {
				return
			}
			if !assert.False(t, healthy) {
				return
			}
		})
	})

	t.Run("will return an error", func(t *testing.T) {
		t.Run("if at least one of the Monitors return an error", func(t *testing.T) {
			var a Binary
			a.MarkHealthy()

			healthErr := errors.New("failed to check health status")
			b := monitorFunc(func(ctx context.Context) (bool, error) {
				return false, healthErr
			})

			var c Binary
			c.MarkHealthy()

			and := And(&a, &b, &c)

			healthy, err := and.Healthy(context.Background())
			if !assert.ErrorIs(t, err, healthErr) {
				return
			}
			if !assert.False(t, healthy) {
				return
			}
		})
	})
}

func TestOrMonitor_Healthy(t *testing.T) {
	t.Run("will return unhealthy", func(t *testing.T) {
		t.Run("if all Monitors return unhealthy", func(t *testing.T) {
			var a Binary
			var b Binary
			var c Binary

			or := Or(&a, &b, &c)

			healthy, err := or.Healthy(context.Background())
			if !assert.Nil(t, err) {
				return
			}
			if !assert.False(t, healthy) {
				return
			}
		})
	})

	t.Run("will return an error", func(t *testing.T) {
		t.Run("if at least one of the Monitors return an error", func(t *testing.T) {
			var a Binary

			healthErr := errors.New("failed to check health status")
			b := monitorFunc(func(ctx context.Context) (bool, error) {
				return false, healthErr
			})

			var c Binary

			or := Or(&a, &b, &c)

			healthy, err := or.Healthy(context.Background())
			if !assert.ErrorIs(t, err, healthErr) {
				return
			}
			if !assert.False(t, healthy) {
				return
			}
		})
	})
}
