// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package twocpus

import (
	"runtime"
	"testing"
)

func TestFailsOnTwoCPUs(t *testing.T) {
	if n := runtime.NumCPU(); n == 2 {
		t.Errorf("runtime.NumCPU() is %d", n)
	}
}
