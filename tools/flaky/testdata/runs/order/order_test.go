// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package order

import "testing"

var dirty bool

func TestFailsAfterTestDirties(t *testing.T) {
	if dirty {
		t.Error("TestDirties ran before it")
	}
}

func TestDirties(t *testing.T) {
	dirty = true
}
