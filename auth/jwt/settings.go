// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jwt

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Algorithm is a JWS signature algorithm that a token may be signed with.
// There is no none, and no HMAC: a key set holds public keys.
type Algorithm string

// The algorithms a token may be signed with: RSASSA-PKCS1-v1_5, RSASSA-PSS
// and ECDSA, each with SHA-256, SHA-384 and SHA-512, and EdDSA, which is
// Ed25519.
const (
	RS256, RS384, RS512 Algorithm = "RS256", "RS384", "RS512"
	PS256, PS384, PS512 Algorithm = "PS256", "PS384", "PS512"
	ES256, ES384, ES512 Algorithm = "ES256", "ES384", "ES512"
	EdDSA               Algorithm = "EdDSA"
)

// algorithms is every Algorithm the package knows, in the order of its
// constants.
var algorithms = []Algorithm{RS256, RS384, RS512, PS256, PS384, PS512, ES256, ES384, ES512, EdDSA}

// maxLeeway is the most clock skew Leeway allows.
const maxLeeway = 5 * time.Minute

// Settings configures a Verifier. Every field is a plain value, and a
// field's zero value means zero, never "use the default": start from
// DefaultSettings and change the fields you source. Issuer and Audience
// have no default, so they are always among them.
type Settings struct {
	// Issuer is the iss every token must have.
	//
	// Default: none. Valid: not empty. Check: CheckIssuer.
	Issuer string

	// Audience is what every token's aud must hold: the service.
	//
	// Default: none. Valid: not empty. Check: CheckAudience.
	Audience string

	// Algorithms are the algorithms a token may be signed with.
	//
	// Default: RS256, PS256, ES256 and EdDSA. Valid: not empty, each one of
	// the constants, none twice. Check: CheckAlgorithms.
	Algorithms []Algorithm

	// Leeway is the clock skew allowed on exp, nbf and iat.
	//
	// Default: 1m. Valid: 0 to 5m. Check: CheckLeeway.
	Leeway time.Duration

	// MinRefreshInterval is the least time between two calls of the
	// KeySet.
	//
	// Default: 1m. Valid: positive. Check: CheckMinRefreshInterval.
	MinRefreshInterval time.Duration

	// MaxKeyAge is how long the keys a call returned are used before the
	// KeySet is called again.
	//
	// Default: 1h. Valid: positive, and at least MinRefreshInterval.
	// Check: CheckMaxKeyAge, and the order in NewVerifier.
	MaxKeyAge time.Duration
}

// DefaultSettings returns the Verifier's defaults. Issuer and Audience have
// none, so they are empty, which CheckIssuer and CheckAudience reject: the
// end user always sources them.
func DefaultSettings() Settings {
	return Settings{
		Issuer:             "",
		Audience:           "",
		Algorithms:         []Algorithm{RS256, PS256, ES256, EdDSA},
		Leeway:             time.Minute,
		MinRefreshInterval: time.Minute,
		MaxKeyAge:          time.Hour,
	}
}

// The causes the checks report. None of them names a field: whoever calls a
// check names the field, or the variable that sourced it.
var (
	errEmpty       = errors.New("must not be empty")
	errNotPositive = errors.New("must be positive")
)

// CheckIssuer rejects an empty issuer: every token's iss is compared with
// it, and a token with no iss would match an empty one.
func CheckIssuer(iss string) error {
	if iss == "" {
		return errEmpty
	}
	return nil
}

// CheckAudience rejects an empty audience: a token is for the service only
// when its aud holds the service's name.
func CheckAudience(aud string) error {
	if aud == "" {
		return errEmpty
	}
	return nil
}

// CheckAlgorithms rejects an empty list, an algorithm that isn't one of the
// package's constants, such as none or HS256, and an algorithm listed
// twice.
func CheckAlgorithms(algs []Algorithm) error {
	if len(algs) == 0 {
		return errEmpty
	}
	var errs []error
	seen := make(map[Algorithm]bool, len(algs))
	for _, a := range algs {
		switch {
		case !slices.Contains(algorithms, a):
			errs = append(errs, fmt.Errorf("holds %q, which isn't one of the algorithms a public key verifies", a))
		case seen[a]:
			errs = append(errs, fmt.Errorf("holds %s twice", a))
		}
		seen[a] = true
	}
	return errors.Join(errs...)
}

// CheckLeeway accepts 0 to 5m: a clock skew larger than that would accept
// a token long after it expired.
func CheckLeeway(d time.Duration) error {
	if d < 0 || d > maxLeeway {
		return fmt.Errorf("must be from 0s to %s, got %s", maxLeeway, d)
	}
	return nil
}

// CheckMinRefreshInterval rejects a duration that is not positive: with no
// least time between two calls, a client sending tokens with made-up kids
// would make the service call its KeySet on every request.
func CheckMinRefreshInterval(d time.Duration) error {
	return positive(d)
}

// CheckMaxKeyAge rejects a duration that is not positive. NewVerifier also
// rejects a MaxKeyAge below MinRefreshInterval, which takes both fields to
// tell.
func CheckMaxKeyAge(d time.Duration) error {
	return positive(d)
}

// positive rejects a duration that is not positive.
func positive(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// checkSettings runs every check on settings, and the order of MaxKeyAge
// and MinRefreshInterval, and returns every failure, each named after its
// field, joined.
func checkSettings(settings Settings) []error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
		}
	}
	check("Settings.Issuer", CheckIssuer(settings.Issuer))
	check("Settings.Audience", CheckAudience(settings.Audience))
	check("Settings.Algorithms", CheckAlgorithms(settings.Algorithms))
	check("Settings.Leeway", CheckLeeway(settings.Leeway))
	minErr := CheckMinRefreshInterval(settings.MinRefreshInterval)
	maxErr := CheckMaxKeyAge(settings.MaxKeyAge)
	check("Settings.MinRefreshInterval", minErr)
	check("Settings.MaxKeyAge", maxErr)
	if minErr == nil && maxErr == nil && settings.MaxKeyAge < settings.MinRefreshInterval {
		check("Settings.MaxKeyAge", fmt.Errorf("must be at least Settings.MinRefreshInterval, %s, got %s", settings.MinRefreshInterval, settings.MaxKeyAge))
	}
	return errs
}
