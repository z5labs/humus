// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package fourprocs

import (
	"runtime"
	"testing"
)

func TestPasses(t *testing.T) {}

func TestBlocksAtFourProcs(t *testing.T) {
	if runtime.GOMAXPROCS(0) == 4 {
		t.Log("blocking")
		select {}
	}
}
