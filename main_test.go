// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
)

// humus.Main exits the process once it runs an App, so the App it builds is
// checked by the internal tests of mainApp. Its panics come before it runs
// anything, so they are checked here.

func TestMain_panics_on_a_nil_init(t *testing.T) {
	require.PanicsWithValue(t, "humus: Main called with a nil init", func() {
		humus.Main(nil)
	})
}

func TestMain_panics_on_a_nil_Option(t *testing.T) {
	init := func(context.Context, *humus.Setup) ([]humus.Service, error) { return nil, nil }
	require.PanicsWithValue(t, "humus: Main called with a nil Option", func() {
		humus.Main(init, humus.WithShutdownGracePeriod(0), nil)
	})
}
