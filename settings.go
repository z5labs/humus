// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"errors"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
)

// settings is the configuration of the App lifecycle, read in step 1 of Run.
// It is the one place humus reads environment variables.
type settings struct {
	// gracePeriod bounds the whole shutdown: every Service's Shutdown and
	// every OnStop function share it.
	gracePeriod time.Duration

	// drainDelay is the pause between shutdown starting and Shutdown being
	// called on the Services.
	drainDelay time.Duration
}

// The causes of a lifecycle setting that fails its check.
var (
	errNotPositive = errors.New("must be positive")
	errNegative    = errors.New("must not be negative")
)

// readSettings reads the lifecycle's settings from env, defaulting each to
// the App's field.
//
// Each value is checked after its default, so a bad default fails the same
// way as a bad variable: with a *config.Error naming the variable. Both
// values are read even when one fails, and every failure is returned, joined.
func (a App) readSettings(ctx context.Context, env bedrock.Environment) (settings, error) {
	return config.Struct(func(r *config.Resolver) settings {
		return settings{
			gracePeriod: r.Get(
				config.Env(env, "HUMUS_SHUTDOWN_GRACE_PERIOD").
					Parse(time.ParseDuration).
					Default(a.ShutdownGracePeriod).
					Validate(checkGracePeriod),
			),
			drainDelay: r.Get(
				config.Env(env, "HUMUS_SHUTDOWN_DRAIN_DELAY").
					Parse(time.ParseDuration).
					Default(a.DrainDelay).
					Validate(checkDrainDelay),
			),
		}
	}).Read(ctx)
}

// checkGracePeriod rejects a grace period that is not positive.
func checkGracePeriod(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// checkDrainDelay rejects a negative drain delay.
func checkDrainDelay(d time.Duration) error {
	if d < 0 {
		return errNegative
	}
	return nil
}
