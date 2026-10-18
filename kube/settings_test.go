// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/kube"
)

// fieldNames matches words a check's error must not contain, in any case:
// a check reports what is wrong, and whoever calls it names the field or
// the variable.
var fieldNames = regexp.MustCompile(`(?i)\b(?:settings|name|namespace|workers|resync|retry ?(?:base|max)? ?delay|` +
	`leader ?election|lease|identity|lease ?duration|renew ?deadline|retry ?period)\b`)

// requireUnnamed fails the test if err names a field of Settings or
// LeaderElection.
func requireUnnamed(t *testing.T, err error) {
	t.Helper()
	require.NotRegexp(t, fieldNames, err.Error(), "the error names a field")
}

// checkCase is one value given to a check: it is valid, or its error
// contains cause.
type checkCase[T any] struct {
	name  string
	value T
	valid bool
	cause string
}

func runCheck[T any](t *testing.T, check func(T) error, testCases []checkCase[T]) {
	t.Helper()
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := check(tc.value)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.cause)
			requireUnnamed(t, err)
		})
	}
}

func TestCheckName(t *testing.T) {
	t.Parallel()

	runCheck(t, kube.CheckName, []checkCase[string]{
		{name: "a single label", value: "memcached", valid: true},
		{name: "a dotted name", value: "memcached.cache.example.com", valid: true},
		{name: "a hyphen", value: "memcached-operator", valid: true},
		{name: "digits", value: "0x1", valid: true},
		{name: "one character", value: "m", valid: true},
		{name: "253 characters", value: strings.Repeat("a", 253), valid: true},
		{name: "the empty string", value: "", cause: "must not be empty"},
		{name: "254 characters", value: strings.Repeat("a", 254), cause: "must be at most 253 characters"},
		{name: "an uppercase letter", value: "Memcached", cause: "must be a DNS-1123 subdomain"},
		{name: "an underscore", value: "memcached_operator", cause: "must be a DNS-1123 subdomain"},
		{name: "a leading hyphen", value: "-memcached", cause: "must be a DNS-1123 subdomain"},
		{name: "a trailing dot", value: "memcached.", cause: "must be a DNS-1123 subdomain"},
		{name: "a slash", value: "cache/memcached", cause: "must be a DNS-1123 subdomain"},
		{name: "a space", value: "memcached operator", cause: "must be a DNS-1123 subdomain"},
	})
}

func TestCheckNamespace(t *testing.T) {
	t.Parallel()

	runCheck(t, kube.CheckNamespace, []checkCase[string]{
		{name: "a namespace", value: "default", valid: true},
		{name: "a hyphen", value: "kube-system", valid: true},
		{name: "digits", value: "team1", valid: true},
		{name: "63 characters", value: strings.Repeat("a", 63), valid: true},
		{name: "the empty string", value: "", cause: "must not be empty"},
		{name: "64 characters", value: strings.Repeat("a", 64), cause: "must be at most 63 characters"},
		{name: "a dot", value: "cache.example", cause: "must be a DNS-1123 label"},
		{name: "an uppercase letter", value: "Default", cause: "must be a DNS-1123 label"},
		{name: "a trailing hyphen", value: "default-", cause: "must be a DNS-1123 label"},
		{name: "an underscore", value: "kube_system", cause: "must be a DNS-1123 label"},
	})
}

func TestCheckWatchNamespace(t *testing.T) {
	t.Parallel()

	runCheck(t, kube.CheckWatchNamespace, []checkCase[string]{
		{name: "the empty string, every namespace", value: "", valid: true},
		{name: "a namespace", value: "default", valid: true},
		{name: "63 characters", value: strings.Repeat("a", 63), valid: true},
		{name: "64 characters", value: strings.Repeat("a", 64), cause: "must be at most 63 characters"},
		{name: "a dot", value: "cache.example", cause: "must be a DNS-1123 label"},
		{name: "an uppercase letter", value: "Default", cause: "must be a DNS-1123 label"},
		{name: "a space", value: " ", cause: "must be a DNS-1123 label"},
	})
}

func TestCheckIdentity(t *testing.T) {
	t.Parallel()

	runCheck(t, kube.CheckIdentity, []checkCase[string]{
		{name: "a pod's name", value: "memcached-operator-7d9f8b6c5-x2k4p", valid: true},
		{name: "any text", value: "Replica One", valid: true},
		{name: "the empty string", value: "", cause: "must not be empty"},
	})
}

func TestCheckWorkers(t *testing.T) {
	t.Parallel()

	runCheck(t, kube.CheckWorkers, []checkCase[int]{
		{name: "one", value: 1, valid: true},
		{name: "many", value: 64, valid: true},
		{name: "the largest int", value: math.MaxInt, valid: true},
		{name: "zero", value: 0, cause: "must be at least 1"},
		{name: "negative", value: -1, cause: "must be at least 1"},
		{name: "the smallest int", value: math.MinInt, cause: "must be at least 1"},
	})
}

// durationCases are the cases of every check that accepts any positive
// duration.
var durationCases = []checkCase[time.Duration]{
	{name: "a nanosecond", value: time.Nanosecond, valid: true},
	{name: "a millisecond", value: time.Millisecond, valid: true},
	{name: "an hour", value: time.Hour, valid: true},
	{name: "the longest duration", value: math.MaxInt64, valid: true},
	{name: "zero", value: 0, cause: "must be positive"},
	{name: "negative", value: -time.Second, cause: "must be positive"},
	{name: "the shortest duration", value: math.MinInt64, cause: "must be positive"},
}

func TestCheckResync(t *testing.T) {
	t.Parallel()

	runCheck(t, kube.CheckResync, durationCases)
}

func TestCheckRetryDelay(t *testing.T) {
	t.Parallel()

	runCheck(t, kube.CheckRetryDelay, durationCases)
}

func TestCheckRetryPeriod(t *testing.T) {
	t.Parallel()

	runCheck(t, kube.CheckRetryPeriod, durationCases)
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	require.Equal(t, kube.Settings{
		Name:           "",
		Namespace:      "",
		Workers:        1,
		Resync:         10 * time.Hour,
		RetryBaseDelay: 5 * time.Millisecond,
		RetryMaxDelay:  1000 * time.Second,
		LeaderElection: kube.LeaderElection{
			Namespace:     "",
			Identity:      "",
			LeaseDuration: 15 * time.Second,
			RenewDeadline: 10 * time.Second,
			RetryPeriod:   2 * time.Second,
		},
	}, kube.DefaultSettings())
}

func TestDefaultSettings_checks(t *testing.T) {
	t.Parallel()

	d := kube.DefaultSettings()

	testCases := []struct {
		name  string
		err   error
		valid bool
	}{
		{name: "Name", err: kube.CheckName(d.Name)},
		{name: "Namespace", err: kube.CheckWatchNamespace(d.Namespace), valid: true},
		{name: "Workers", err: kube.CheckWorkers(d.Workers), valid: true},
		{name: "Resync", err: kube.CheckResync(d.Resync), valid: true},
		{name: "RetryBaseDelay", err: kube.CheckRetryDelay(d.RetryBaseDelay), valid: true},
		{name: "RetryMaxDelay", err: kube.CheckRetryDelay(d.RetryMaxDelay), valid: true},
		{name: "LeaderElection.Namespace", err: kube.CheckNamespace(d.LeaderElection.Namespace)},
		{name: "LeaderElection.Identity", err: kube.CheckIdentity(d.LeaderElection.Identity)},
		{name: "LeaderElection.RetryPeriod", err: kube.CheckRetryPeriod(d.LeaderElection.RetryPeriod), valid: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.valid {
				require.NoError(t, tc.err)
				return
			}
			require.Error(t, tc.err, "a required field has no default, so its check fails")
			requireUnnamed(t, tc.err)
		})
	}
}

// TestDefaultSettings_keeps_the_bounds_between_fields checks the defaults
// against the bounds NewController checks between fields.
func TestDefaultSettings_keeps_the_bounds_between_fields(t *testing.T) {
	t.Parallel()

	d := kube.DefaultSettings()

	require.GreaterOrEqual(t, d.RetryMaxDelay, d.RetryBaseDelay)
	require.Greater(t, d.LeaderElection.LeaseDuration, d.LeaderElection.RenewDeadline)
	require.Greater(t, float64(d.LeaderElection.RenewDeadline), 1.2*float64(d.LeaderElection.RetryPeriod))
}
