//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"testing"

	"github.com/z5labs/humus/example/http/inlineedit/internal/chrometest"
)

func TestInit_htmx_edits_a_row_in_place_in_Chrome_and_ends_its_feed_at_shutdown(t *testing.T) {
	chrometest.Run(t, Init, "HX-Request")
}
