// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build conformance

package humustest_test

import (
	"testing"

	"github.com/z5labs/humus"
)

// TestConformance_loopback runs the conformance scenarios on real loopback
// sockets, so it runs only with -tags conformance. It needs 127.0.0.2 on
// the loopback interface, as Linux has.
func TestConformance_loopback(t *testing.T) {
	testConformance(t, func(*testing.T) humus.Host {
		return humus.OSHost()
	})
}
