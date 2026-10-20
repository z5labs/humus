// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package postgres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScramVerifierWithSalt_matches_an_independent_derivation(t *testing.T) {
	salt := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	verifier, err := scramVerifierWithSalt("pencil", salt, 4096)

	require.NoError(t, err)
	require.Equal(t, "SCRAM-SHA-256$4096:AAECAwQFBgcICQoLDA0ODw==$zHCdol2044/ZyWzPLi7oxApCkamKw9Z+E4U/QApd/5Y=:dd5peBOitVnLNFu7VmwP+HiDaaw4OUCv396eVCWhYiE=", verifier)
}

func TestScramVerifier_is_salted_afresh_and_never_holds_the_password(t *testing.T) {
	password := "correct-horse-battery-staple"

	first, err := scramVerifier(password)
	require.NoError(t, err)
	second, err := scramVerifier(password)
	require.NoError(t, err)

	require.NotEqual(t, first, second)
	for _, v := range []string{first, second} {
		require.True(t, strings.HasPrefix(v, "SCRAM-SHA-256$4096:"), v)
		require.NotContains(t, v, password)
		require.NotContains(t, v, "'")
	}
}

func TestQuoteLiteral_doubles_single_quotes(t *testing.T) {
	require.Equal(t, `'it''s'`, quoteLiteral("it's"))
}
