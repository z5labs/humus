// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package a

import "testing"

func TestA(t *testing.T) {
	if A() != "a" {
		t.Fail()
	}
}

func TestAToo(t *testing.T) {}

func BenchmarkA(b *testing.B) {}
