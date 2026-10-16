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

// defaultShutdownGracePeriod is the ShutdownGracePeriod of an App whose
// environment does not set HUMUS_SHUTDOWN_GRACE_PERIOD.
const defaultShutdownGracePeriod = 10 * time.Second

// Settings is the configuration of the App lifecycle.
type Settings struct {
	// ShutdownGracePeriod bounds the whole shutdown of an App: every
	// Service's Shutdown and every OnStop function share it. It is read
	// from HUMUS_SHUTDOWN_GRACE_PERIOD, as a time.ParseDuration string
	// such as "30s", and defaults to 10s.
	ShutdownGracePeriod time.Duration
}

// errNotPositive is the cause of a duration setting that is zero or less.
var errNotPositive = errors.New("must be positive")

// ReadSettings reads the lifecycle's Settings from env.
//
// A variable that is not set takes its default. A variable that is set but
// malformed, including a duration that is not positive, is an error with a
// *config.Error naming the variable in its chain, and App.Run exits
// ExitConfig on it.
func ReadSettings(ctx context.Context, env bedrock.Environment) (Settings, error) {
	return config.Struct(func(r *config.Resolver) Settings {
		return Settings{
			ShutdownGracePeriod: r.Get(
				config.Env(env, "HUMUS_SHUTDOWN_GRACE_PERIOD").
					Parse(parsePositiveDuration).
					Default(defaultShutdownGracePeriod),
			),
		}
	}).Read(ctx)
}

// parsePositiveDuration parses s with time.ParseDuration and rejects
// a duration that is not above zero.
func parsePositiveDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, errNotPositive
	}
	return d, nil
}
