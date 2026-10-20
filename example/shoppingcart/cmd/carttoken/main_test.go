// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	issuer   = "https://idp.internal"
	audience = "shoppingcart"
)

type run struct {
	code   int
	cause  error
	stdout string
	stderr string
}

func start(t *testing.T, files map[string][]byte, build telemetry.Builder, args ...string) run {
	t.Helper()
	h := humustest.NewHost(t, humustest.WithFiles(files))
	for i, arg := range args {
		if _, ok := files[arg]; ok {
			args[i] = h.Path(arg)
		}
	}
	p, status := humustest.Start(t, Init,
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(build)),
		humustest.WithArgs(args...),
	)
	st := <-status
	return run{code: st.Code(), cause: st.Cause(), stdout: p.Stdout(), stderr: p.Stderr()}
}

func output(t *testing.T, files map[string][]byte, args ...string) string {
	t.Helper()
	got := start(t, files, telemetry.NoopBuilder, args...)
	require.Zero(t, got.code, "cause: %v", got.cause)
	require.True(t, strings.HasSuffix(got.stdout, "\n"))
	return strings.TrimSuffix(got.stdout, "\n")
}

func pkcs8(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func ecKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	require.NoError(t, err)
	return key
}

func sec1WithParameters(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	params := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}})
	return append(params, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})...)
}

func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	require.NoError(t, err)
	return key
}

func ed25519Key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return key
}

type staticKeySet string

func (s staticKeySet) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/jwk-set+json"}},
		Body:       io.NopCloser(strings.NewReader(string(s))),
		Request:    req,
	}, nil
}

func verifier(t *testing.T, jwks string) *jwt.Verifier[endpoint.Claims] {
	t.Helper()
	u, err := url.Parse("https://keys.internal/.well-known/jwks.json")
	require.NoError(t, err)
	keys, err := jwt.NewRemoteKeySet(u, &http.Client{Transport: staticKeySet(jwks)})
	require.NoError(t, err)
	settings := jwt.DefaultSettings()
	settings.Issuer = issuer
	settings.Audience = audience
	settings.Algorithms = []jwt.Algorithm{jwt.RS256, jwt.ES256, jwt.ES384, jwt.ES512, jwt.EdDSA}
	v, err := jwt.NewVerifier[endpoint.Claims](&humus.Setup{Telemetry: telemetry.Noop()}, settings, keys)
	require.NoError(t, err)
	return v
}

func signArgs(subject string, scopes ...string) []string {
	args := []string{"sign", "-key", "key.pem", "-sub", subject, "-iss", issuer, "-aud", audience}
	for _, scope := range scopes {
		args = append(args, "-scope", scope)
	}
	return args
}

func TestInit_a_token_from_sign_is_verified_against_the_key_set_from_jwks(t *testing.T) {
	testCases := map[string]func(t *testing.T) []byte{
		"ECDSA P-256, PKCS #8": func(t *testing.T) []byte { return pkcs8(t, ecKey(t, elliptic.P256())) },
		"ECDSA P-256, SEC 1 after its parameters": func(t *testing.T) []byte {
			return sec1WithParameters(t, ecKey(t, elliptic.P256()))
		},
		"ECDSA P-384, PKCS #8": func(t *testing.T) []byte { return pkcs8(t, ecKey(t, elliptic.P384())) },
		"ECDSA P-521, PKCS #8": func(t *testing.T) []byte { return pkcs8(t, ecKey(t, elliptic.P521())) },
		"RSA, PKCS #1": func(t *testing.T) []byte {
			return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey(t, 2048))})
		},
		"RSA, PKCS #8":     func(t *testing.T) []byte { return pkcs8(t, rsaKey(t, 2048)) },
		"Ed25519, PKCS #8": func(t *testing.T) []byte { return pkcs8(t, ed25519Key(t)) },
	}
	for name, keyPEM := range testCases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				files := map[string][]byte{"key.pem": keyPEM(t)}
				jwks := output(t, files, "jwks", "-key", "key.pem")
				token := output(t, files, signArgs("alice", endpoint.ScopeWrite, "carts:read")...)

				claims, err := verifier(t, jwks).Verify(context.Background(), token)
				require.NoError(t, err)
				require.Equal(t, "alice", claims.Subject)
				require.Equal(t, "carts:write carts:read", claims.Scope)
				require.True(t, claims.HasScope(endpoint.ScopeWrite))
			})
		})
	}
}

func TestInit_a_key_set_made_from_another_key_rejects_the_token(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signing := map[string][]byte{"key.pem": pkcs8(t, ecKey(t, elliptic.P256()))}
		other := map[string][]byte{"key.pem": pkcs8(t, ecKey(t, elliptic.P256()))}
		token := output(t, signing, signArgs("alice", endpoint.ScopeWrite)...)
		jwks := output(t, other, "jwks", "-key", "key.pem")

		_, err := verifier(t, jwks).Verify(context.Background(), token)
		require.ErrorIs(t, err, auth.ErrUnauthenticated)
	})
}

func TestInit_jwks_prints_the_public_half_with_the_key_s_thumbprint_as_its_kid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		key := ecKey(t, elliptic.P256())
		files := map[string][]byte{"key.pem": pkcs8(t, key)}
		out := output(t, files, "jwks", "-key", "key.pem")

		var set struct {
			Keys []map[string]string `json:"keys"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &set))
		require.Len(t, set.Keys, 1)
		jwk := set.Keys[0]

		coordinate := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
		public, err := key.PublicKey.Bytes()
		require.NoError(t, err)
		x, y := coordinate(public[1:33]), coordinate(public[33:])
		canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":%q,"y":%q}`, x, y)
		sum := sha256.Sum256([]byte(canonical))

		require.Equal(t, map[string]string{
			"kty": "EC",
			"crv": "P-256",
			"x":   x,
			"y":   y,
			"kid": coordinate(sum[:]),
			"alg": "ES256",
			"use": "sig",
		}, jwk)
	})
}

func TestInit_sign_names_the_key_s_kid_and_lasts_its_ttl(t *testing.T) {
	testCases := map[string]struct {
		args []string
		want time.Duration
	}{
		"by default":   {want: time.Hour},
		"with -ttl 5m": {args: []string{"-ttl", "5m"}, want: 5 * time.Minute},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				files := map[string][]byte{"key.pem": pkcs8(t, ed25519Key(t))}
				jwks := output(t, files, "jwks", "-key", "key.pem")
				token := output(t, files, append(signArgs("bob"), tc.args...)...)

				var set jose.JSONWebKeySet
				require.NoError(t, json.Unmarshal([]byte(jwks), &set))
				require.Len(t, set.Keys, 1)

				parsed, err := josejwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.EdDSA})
				require.NoError(t, err)
				require.Len(t, parsed.Headers, 1)
				require.Equal(t, set.Keys[0].KeyID, parsed.Headers[0].KeyID)
				require.Equal(t, "JWT", parsed.Headers[0].ExtraHeaders[jose.HeaderType])

				var claims struct {
					josejwt.Claims
					Scope *string `json:"scope"`
				}
				require.NoError(t, parsed.Claims(set.Keys[0].Key, &claims))
				require.Nil(t, claims.Scope)
				require.WithinDuration(t, time.Now(), claims.IssuedAt.Time(), 0)
				require.WithinDuration(t, time.Now(), claims.NotBefore.Time(), 0)
				require.WithinDuration(t, time.Now().Add(tc.want), claims.Expiry.Time(), 0)

				v := verifier(t, jwks)
				_, err = v.Verify(context.Background(), token)
				require.NoError(t, err)
				time.Sleep(tc.want + jwt.DefaultSettings().Leeway + time.Second)
				_, err = v.Verify(context.Background(), token)
				require.ErrorIs(t, err, auth.ErrUnauthenticated)
			})
		})
	}
}

func TestInit_exits_ExitConfig_on_a_bad_setting(t *testing.T) {
	testCases := map[string]struct {
		files func(t *testing.T) map[string][]byte
		args  []string
		want  []string
	}{
		"no command": {
			want: []string{"config command: not found"},
		},
		"an unknown command": {
			args: []string{"verify"},
			want: []string{`config command: "verify" is neither jwks nor sign`},
		},
		"jwks with no key": {
			args: []string{"jwks"},
			want: []string{"config -key: not found"},
		},
		"jwks with a flag of sign's": {
			args: []string{"jwks", "-key", "key.pem", "-sub", "alice"},
			want: []string{"config arguments: flag provided but not defined: -sub"},
		},
		"an argument after the flags": {
			args: []string{"jwks", "-key", "key.pem", "extra"},
			want: []string{`config arguments: unexpected argument "extra"`},
		},
		"sign with nothing": {
			args: []string{"sign"},
			want: []string{
				"config -key: not found",
				"config -sub: not found",
				"config -iss: not found",
				"config -aud: not found",
			},
		},
		"sign with empty values": {
			args: []string{"sign", "-key", "key.pem", "-sub", "", "-iss", "", "-aud", "", "-scope", "", "-ttl", "0s"},
			want: []string{
				"config -sub: must not be empty",
				"config -iss:",
				"config -aud:",
				`config -scope: "" is empty or has a space`,
				"config -ttl: must be positive",
			},
		},
		"a scope with a space": {
			args: signArgs("alice", "carts:read carts:write"),
			want: []string{`config -scope: "carts:read carts:write" is empty or has a space`},
		},
		"a ttl that isn't a duration": {
			args: append(signArgs("alice"), "-ttl", "an hour"),
			want: []string{"config -ttl:"},
		},
		"a key file that doesn't exist": {
			files: func(*testing.T) map[string][]byte { return map[string][]byte{"other.pem": nil} },
			args:  []string{"jwks", "-key", "missing.pem"},
			want:  []string{"config -key:", "no such file"},
		},
		"a key file with no private key": {
			files: func(t *testing.T) map[string][]byte {
				return map[string][]byte{"key.pem": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("public")})}
			},
			args: []string{"jwks", "-key", "key.pem"},
			want: []string{"config -key: no PEM private key in the file"},
		},
		"a key file with two private keys": {
			files: func(t *testing.T) map[string][]byte {
				return map[string][]byte{"key.pem": append(pkcs8(t, ed25519Key(t)), pkcs8(t, ed25519Key(t))...)}
			},
			args: []string{"jwks", "-key", "key.pem"},
			want: []string{"config -key: more than one private key in the file"},
		},
		"a private key that doesn't parse": {
			files: func(t *testing.T) map[string][]byte {
				return map[string][]byte{"key.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a key")})}
			},
			args: []string{"jwks", "-key", "key.pem"},
			want: []string{"config -key:"},
		},
		"an RSA key under 2048 bits": {
			files: func(t *testing.T) map[string][]byte {
				return map[string][]byte{"key.pem": pkcs8(t, rsaKey(t, 1024))}
			},
			args: []string{"jwks", "-key", "key.pem"},
			want: []string{"config -key: the RSA key has 1024 bits, fewer than 2048"},
		},
		"an ECDSA key on another curve": {
			files: func(t *testing.T) map[string][]byte {
				return map[string][]byte{"key.pem": pkcs8(t, ecKey(t, elliptic.P224()))}
			},
			args: []string{"jwks", "-key", "key.pem"},
			want: []string{"config -key: the curve P-224 isn't P-256, P-384 or P-521"},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var got run
			synctest.Test(t, func(t *testing.T) {
				files := map[string][]byte{"key.pem": pkcs8(t, ed25519Key(t))}
				if tc.files != nil {
					files = tc.files(t)
				}
				got = start(t, files, telemetry.NoopBuilder, tc.args...)
			})
			require.Equal(t, humus.ExitConfig, got.code, "cause: %v", got.cause)
			require.Empty(t, got.stdout)
			for _, want := range tc.want {
				require.ErrorContains(t, got.cause, want)
			}
		})
	}
}

func TestBuildTelemetry_writes_the_logs_to_stderr_and_leaves_stdout_to_the_output(t *testing.T) {
	var got run
	synctest.Test(t, func(t *testing.T) {
		got = start(t, nil, buildTelemetry, "jwks")
	})
	require.Equal(t, humus.ExitConfig, got.code, "cause: %v", got.cause)
	require.Empty(t, got.stdout)
	require.Contains(t, got.stderr, `"severity_text":"ERROR"`)
	require.Contains(t, got.stderr, "config -key: not found")
}
