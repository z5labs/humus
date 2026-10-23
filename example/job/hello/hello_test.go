// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/z5labs/humus/humustest"
)

func TestInit_greets_the_name_it_is_given(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, status := humustest.Start(t, Init,
			humustest.WithEnv(map[string]string{"HELLO_NAME": "Gopher"}),
		)

		st := <-status
		if st.Code() != 0 {
			t.Fatalf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if !strings.Contains(p.Stdout(), "Hello, Gopher!\n") {
			t.Errorf("stdout = %q, want it to greet Gopher", p.Stdout())
		}
	})
}

func TestInit_fails_without_a_name(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init)

		st := <-status
		if st.Code() != 78 {
			t.Errorf("code = %d, want 78: %v", st.Code(), st.Cause())
		}
		if !strings.Contains(fmt.Sprint(st.Cause()), "HELLO_NAME") {
			t.Errorf("cause = %v, want it to name HELLO_NAME", st.Cause())
		}
	})
}
