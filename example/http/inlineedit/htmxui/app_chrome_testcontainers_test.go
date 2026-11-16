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

var htmx = chrometest.UI{
	RequestHeader: "HX-Request",
	FormReady:     `!form.classList.contains("htmx-added")`,
}

const settleLate = `htmx.config.defaultSettleDelay = 1000`

func TestInit_htmx_edits_a_row_in_place_in_Chrome_and_ends_its_feed_at_shutdown(t *testing.T) {
	chrometest.Run(t, Init, htmx)
}

func TestInit_htmx_sends_a_PUT_when_Save_is_clicked_as_soon_as_the_form_is_visible_and_htmx_settles_late(t *testing.T) {
	ui := htmx
	ui.BeforeEdit = settleLate
	chrometest.Run(t, Init, ui)
}

func TestInit_htmx_sends_a_PUT_when_Save_is_clicked_as_soon_as_the_problems_are_shown_and_htmx_settles_late(t *testing.T) {
	ui := htmx
	ui.BeforeFirstSave = settleLate
	chrometest.Run(t, Init, ui)
}
