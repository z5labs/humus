// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package lingers

import "testing"

// TestMain blocks once the tests have run, so the run is still going after
// its timeout.
func TestMain(m *testing.M) {
	m.Run()
	select {}
}

func TestPasses(t *testing.T) {}
