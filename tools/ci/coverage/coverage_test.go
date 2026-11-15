// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package coverage_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/tools/ci/coverage"
)

func TestGate(t *testing.T) {
	testCases := []struct {
		name     string
		packages []string
		profile  string
		passed   bool
		out      string
	}{
		{
			name:     "a package at exactly 80% passes",
			packages: []string{"example.com/m/a"},
			profile:  "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 4 1\nexample.com/m/a/a.go:3.1,4.1 1 0\n",
			passed:   true,
			out:      "example.com/m/a: 80.0% of statements (4 of 5)\n",
		},
		{
			name:     "a package below 80% fails",
			packages: []string{"example.com/m/a"},
			profile:  "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 79 1\nexample.com/m/a/a.go:3.1,4.1 21 0\n",
			out:      "::error title=Coverage below 80%::example.com/m/a: 79.0% of statements (79 of 100)\n",
		},
		{
			name:     "79.96% does not round up to 80%",
			packages: []string{"example.com/m/a"},
			profile:  "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1999 1\nexample.com/m/a/a.go:3.1,4.1 501 0\n",
			out:      "::error title=Coverage below 80%::example.com/m/a: 80.0% of statements (1999 of 2500)\n",
		},
		{
			name:     "a package with no statements passes",
			packages: []string{"example.com/m/doc"},
			profile:  "mode: atomic\n",
			passed:   true,
			out:      "example.com/m/doc: no statements\n",
		},
		{
			name:    "an empty package list gates nothing and reports no package",
			profile: "mode: atomic\nexample.com/m/example/x/x.go:1.1,2.1 9 0\n",
			passed:  true,
		},
		{
			name:     "a blank line in the package list is no package",
			packages: []string{"", "example.com/m/a", ""},
			profile:  "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1 1\n",
			passed:   true,
			out:      "example.com/m/a: 100.0% of statements (1 of 1)\n",
		},
		{
			name:     "a package with statements and no tests fails",
			packages: []string{"example.com/m/a"},
			profile:  "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 3 0\n",
			out:      "::error title=Coverage below 80%::example.com/m/a: 0.0% of statements (0 of 3)\n",
		},
		{
			name:     "a block listed twice counts once, as covered if either covered it",
			packages: []string{"example.com/m/a"},
			profile:  "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 4 0\nexample.com/m/a/a.go:1.1,2.1 4 2\n",
			passed:   true,
			out:      "example.com/m/a: 100.0% of statements (4 of 4)\n",
		},
		{
			name:     "a package left out of the list is not gated",
			packages: []string{"example.com/m/a"},
			profile:  "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1 1\nexample.com/m/example/x/x.go:1.1,2.1 9 0\n",
			passed:   true,
			out:      "example.com/m/a: 100.0% of statements (1 of 1)\n",
		},
		{
			name:     "a subpackage is gated apart from its parent",
			packages: []string{"example.com/m/a", "example.com/m/a/b"},
			profile:  "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1 1\nexample.com/m/a/b/b.go:1.1,2.1 1 0\n",
			out:      "example.com/m/a: 100.0% of statements (1 of 1)\n::error title=Coverage below 80%::example.com/m/a/b: 0.0% of statements (0 of 1)\n",
		},
		{
			name:     "profiles merged from two modules are each gated",
			packages: []string{"example.com/m/a", "example.com/m/n/c"},
			profile:  "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1 1\nmode: atomic\nexample.com/m/n/c/c.go:1.1,2.1 1 0\n",
			out:      "example.com/m/a: 100.0% of statements (1 of 1)\n::error title=Coverage below 80%::example.com/m/n/c: 0.0% of statements (0 of 1)\n",
		},
		{
			name:     "a blank line in the profile is no block",
			packages: []string{"example.com/m/a"},
			profile:  "mode: atomic\n\nexample.com/m/a/a.go:1.1,2.1 1 1\n",
			passed:   true,
			out:      "example.com/m/a: 100.0% of statements (1 of 1)\n",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			passed, err := coverage.Gate(&out, 80, tc.packages, strings.NewReader(tc.profile))
			require.NoError(t, err)
			require.Equal(t, tc.passed, passed)
			require.Equal(t, tc.out, out.String())
		})
	}
}

func TestGate_refuses_what_is_not_a_cover_profile(t *testing.T) {
	for name, profile := range map[string]string{
		"too few fields":      "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1\n",
		"no position":         "mode: atomic\nexample.com/m/a/a.go 1 1\n",
		"statements":          "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 one 1\n",
		"count":               "mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1 one\n",
		"a line too long":     "mode: atomic\n" + strings.Repeat("x", 2<<20) + "\n",
		"a reader that fails": "",
	} {
		t.Run(name, func(t *testing.T) {
			var r io.Reader = strings.NewReader(profile)
			if profile == "" {
				r = failingReader{}
			}
			var out strings.Builder
			passed, err := coverage.Gate(&out, 80, []string{"example.com/m/a"}, r)
			require.Error(t, err)
			require.False(t, passed)
			require.Empty(t, out.String())
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
