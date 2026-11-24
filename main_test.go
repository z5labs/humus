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

// humus.Main and humus.MainOn exit the process once they run an App, so
// the App they build is checked by the internal tests of mainApp. Their
// panics come before they run anything, so they are checked here.

func TestMain_panics_on_a_nil_init(t *testing.T) {
	require.PanicsWithValue(t, "humus: Main called with a nil init", func() {
		humus.Main(nil)
	})
}

func TestMainOn_panics_on_a_nil_init_or_acquire(t *testing.T) {
	acquire := func() (humus.Host, error) { return humus.OSHost(), nil }
	init := func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error) { return nil, nil }
	require.PanicsWithValue(t, "humus: MainOn called with a nil init", func() {
		humus.MainOn(acquire, nil)
	})
	require.PanicsWithValue(t, "humus: MainOn called with a nil acquire", func() {
		humus.MainOn(nil, init)
	})
}
