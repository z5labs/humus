// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jwt_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/telemetry"
)

// fields are the names of Settings' fields, none of which a check's error
// may name.
var fields = []string{"Issuer", "Audience", "Algorithms", "Leeway", "MinRefreshInterval", "MaxKeyAge"}

// requireUnnamed fails when err names a field.
func requireUnnamed(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	for _, f := range fields {
		require.NotContains(t, err.Error(), f)
	}
}

func TestChecks(t *testing.T) {
	t.Parallel()

	t.Run("CheckIssuer", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, jwt.CheckIssuer(issuer))
		requireUnnamed(t, jwt.CheckIssuer(""))
	})
	t.Run("CheckAudience", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, jwt.CheckAudience(audience))
		requireUnnamed(t, jwt.CheckAudience(""))
	})
	t.Run("CheckAlgorithms", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, jwt.CheckAlgorithms([]jwt.Algorithm{
			jwt.RS256, jwt.RS384, jwt.RS512, jwt.PS256, jwt.PS384, jwt.PS512,
			jwt.ES256, jwt.ES384, jwt.ES512, jwt.EdDSA,
		}))
		requireUnnamed(t, jwt.CheckAlgorithms(nil))
		requireUnnamed(t, jwt.CheckAlgorithms([]jwt.Algorithm{"none"}))
		requireUnnamed(t, jwt.CheckAlgorithms([]jwt.Algorithm{"HS256"}))
		err := jwt.CheckAlgorithms([]jwt.Algorithm{jwt.ES256, jwt.ES256})
		requireUnnamed(t, err)
		require.Contains(t, err.Error(), "twice")
	})
	t.Run("CheckLeeway", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, jwt.CheckLeeway(0))
		require.NoError(t, jwt.CheckLeeway(5*time.Minute))
		requireUnnamed(t, jwt.CheckLeeway(-time.Second))
		requireUnnamed(t, jwt.CheckLeeway(5*time.Minute+time.Second))
	})
	t.Run("CheckMinRefreshInterval", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, jwt.CheckMinRefreshInterval(time.Second))
		requireUnnamed(t, jwt.CheckMinRefreshInterval(0))
	})
	t.Run("CheckMaxKeyAge", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, jwt.CheckMaxKeyAge(time.Hour))
		requireUnnamed(t, jwt.CheckMaxKeyAge(-time.Hour))
	})
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	d := jwt.DefaultSettings()
	require.Empty(t, d.Issuer)
	require.Empty(t, d.Audience)
	require.Equal(t, []jwt.Algorithm{jwt.RS256, jwt.PS256, jwt.ES256, jwt.EdDSA}, d.Algorithms)
	require.Equal(t, time.Minute, d.Leeway)
	require.Equal(t, time.Minute, d.MinRefreshInterval)
	require.Equal(t, time.Hour, d.MaxKeyAge)

	s := &humus.Setup{Telemetry: telemetry.Noop()}
	_, err := jwt.NewVerifier[Claims](s, settings(), &keySet{})
	require.NoError(t, err, "DefaultSettings with Issuer and Audience set")
}

func TestNewVerifier_mistakes(t *testing.T) {
	t.Parallel()

	s := &humus.Setup{Telemetry: telemetry.Noop()}

	t.Run("names every bad field, together", func(t *testing.T) {
		t.Parallel()
		bad := settings()
		bad.Issuer = ""
		bad.Leeway = time.Hour
		_, err := jwt.NewVerifier[Claims](s, bad, &keySet{})
		require.ErrorContains(t, err, "Settings.Issuer must not be empty")
		require.ErrorContains(t, err, "Settings.Leeway must be from 0s to 5m0s, got 1h0m0s")
		require.True(t, strings.HasPrefix(err.Error(), "jwt: NewVerifier: "), err.Error())
	})
	t.Run("a MaxKeyAge below MinRefreshInterval", func(t *testing.T) {
		t.Parallel()
		bad := settings()
		bad.MinRefreshInterval = 2 * time.Minute
		bad.MaxKeyAge = time.Minute
		_, err := jwt.NewVerifier[Claims](s, bad, &keySet{})
		require.ErrorContains(t, err, "Settings.MaxKeyAge must be at least Settings.MinRefreshInterval, 2m0s, got 1m0s")
	})
	t.Run("a nil KeySet", func(t *testing.T) {
		t.Parallel()
		_, err := jwt.NewVerifier[Claims](s, settings(), nil)
		require.ErrorContains(t, err, "the KeySet must not be nil")
	})
	t.Run("every check, with a nil KeySet", func(t *testing.T) {
		t.Parallel()
		_, err := jwt.NewVerifier[Claims](s, jwt.Settings{Leeway: -time.Second}, nil)
		for _, f := range fields {
			require.ErrorContains(t, err, "Settings."+f+" ")
		}
		require.ErrorContains(t, err, "the KeySet must not be nil")
	})
	t.Run("a Setup without a LoggerProvider", func(t *testing.T) {
		t.Parallel()
		_, err := jwt.NewVerifier[Claims](&humus.Setup{}, settings(), &keySet{})
		require.ErrorContains(t, err, "LoggerProvider must not be nil")
		_, err = jwt.NewVerifier[Claims](nil, settings(), &keySet{})
		require.ErrorContains(t, err, "the Setup must not be nil")
	})
}

func TestNewVerifier_calls_nothing(t *testing.T) {
	t.Parallel()

	keys := &keySet{}
	_, _ = newVerifier(t, keys)
	require.Zero(t, keys.count(), "NewVerifier called the KeySet")
}
