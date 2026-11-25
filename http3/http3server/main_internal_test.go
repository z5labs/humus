// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
)

// Main itself exits the process, so these tests check mainOptions, the
// function Main builds the options it passes to humus.Main with, as the
// root package's tests check mainApp.

// applied returns the App that opts make of an empty App, in order.
func applied(t *testing.T, opts []humus.Option) humus.App[humus.Host] {
	t.Helper()
	var a humus.App[humus.Host]
	require.NoError(t, a.Apply(opts...))
	return a
}

// drainTakes runs d inside a synctest bubble, with a context that is never
// done, and returns how long it waited.
func drainTakes(t *testing.T, d humus.Drain) time.Duration {
	t.Helper()
	var took time.Duration
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		require.NoError(t, d(context.Background(), &humus.Setup{}, nil))
		took = time.Since(start)
	})
	return took
}

func TestMainOptions(t *testing.T) {
	t.Run("are Defaults alone when the end user passes none", func(t *testing.T) {
		opts := mainOptions()
		require.Len(t, opts, len(Defaults()))

		a := applied(t, opts)
		require.Equal(t, 10*time.Second, a.ShutdownGracePeriod)
		require.NotNil(t, a.Drain)
		require.Equal(t, 5*time.Second, drainTakes(t, a.Drain))
	})

	t.Run("put Defaults before the end user's options", func(t *testing.T) {
		opts := mainOptions(humus.WithShutdownGracePeriod(30 * time.Second))
		require.Len(t, opts, len(Defaults())+1)

		first := applied(t, opts[:len(Defaults())])
		require.Equal(t, 10*time.Second, first.ShutdownGracePeriod, "the first option is Defaults")
		require.Equal(t, 5*time.Second, drainTakes(t, first.Drain), "the first option is Defaults")

		a := applied(t, opts)
		require.Equal(t, 30*time.Second, a.ShutdownGracePeriod, "the end user's option wins")
		require.NotNil(t, a.Drain)
		require.Equal(t, 5*time.Second, drainTakes(t, a.Drain), "the drain is still Defaults'")
	})

	t.Run("let the end user remove the drain", func(t *testing.T) {
		a := applied(t, mainOptions(humus.WithDrain(nil)))
		require.Nil(t, a.Drain)
		require.Equal(t, 10*time.Second, a.ShutdownGracePeriod)
	})

	t.Run("keep every option the end user passes, in order", func(t *testing.T) {
		opts := mainOptions(
			humus.WithShutdownGracePeriod(30*time.Second),
			humus.WithShutdownGracePeriod(15*time.Second),
		)
		require.Len(t, opts, len(Defaults())+2)
		require.Equal(t, 15*time.Second, applied(t, opts).ShutdownGracePeriod, "the last option wins")
	})
}
