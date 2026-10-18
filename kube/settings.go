// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// LeaderElection is the Lease a controller's replicas elect their leader
// with. The Lease is named after Settings.Name. Every field is a plain
// value, and a field's zero value means zero, never "use the default":
// start from the LeaderElection in DefaultSettings and change the fields
// you source. Namespace and Identity have no default, so they are always
// among them.
//
// The durations' defaults are kube-controller-manager's.
type LeaderElection struct {
	// Namespace is the Lease's namespace, such as the pod's. humus imposes
	// no namespace, so it is required.
	//
	// Default: none (""). Valid: a DNS-1123 label. Check: CheckNamespace.
	Namespace string

	// Identity is this replica's name in the Lease, such as its pod's. It
	// is required.
	//
	// Default: none (""). Valid: not empty. Check: CheckIdentity.
	Identity string

	// LeaseDuration is how long a Lease lasts without being renewed: how
	// long the other replicas wait before they take over from a leader
	// that stopped renewing it.
	//
	// Default: 15s. Valid: longer than RenewDeadline. Check: none of its
	// own; NewController checks the bound.
	LeaseDuration time.Duration

	// RenewDeadline is how long the leader keeps trying to renew the Lease
	// before it gives up leading.
	//
	// Default: 10s. Valid: longer than 1.2 RetryPeriods. Check: none of its
	// own; NewController checks the bound.
	RenewDeadline time.Duration

	// RetryPeriod is how often a replica tries to take the Lease, and the
	// leader tries to renew it.
	//
	// Default: 2s. Valid: positive. Check: CheckRetryPeriod.
	RetryPeriod time.Duration
}

// Settings configures a Controller. Every field is a plain value, and a
// field's zero value means zero, never "use the default": start from
// DefaultSettings and change the fields you source. Name,
// LeaderElection.Namespace and LeaderElection.Identity have no default, so
// they are always among them.
type Settings struct {
	// Name is the controller's field manager, and its Lease's name. humus
	// imposes no name, so it is required.
	//
	// Default: none (""). Valid: a DNS-1123 subdomain. Check: CheckName.
	Name string

	// Namespace limits the controller to one namespace. "" means every
	// namespace.
	//
	// Default: "", every namespace. Valid: "" or a DNS-1123 label. Check:
	// CheckWatchNamespace.
	Namespace string

	// Workers is how many objects are reconciled at once. One object is
	// never reconciled twice at once.
	//
	// Default: 1. Valid: at least 1. Check: CheckWorkers.
	Workers int

	// Resync reconciles every object again this often, whether or not it
	// changed.
	//
	// Default: 10h. Valid: positive. Check: CheckResync.
	Resync time.Duration

	// RetryBaseDelay and RetryMaxDelay bound the backoff of an object whose
	// reconcile failed: the delay starts at RetryBaseDelay and doubles with
	// each failure, up to RetryMaxDelay. The defaults are the per-object
	// backoff of client-go's default controller rate limiter.
	//
	// Default: 5ms and 1000s. Valid: positive, and RetryMaxDelay at least
	// RetryBaseDelay, which NewController checks. Check: CheckRetryDelay,
	// for each.
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration

	// LeaderElection is the Lease the controller's replicas elect their
	// leader with.
	LeaderElection LeaderElection
}

// DefaultSettings returns the Controller's defaults: every namespace, 1
// worker, a resync every 10h, retries backed off from 5ms to 1000s, as
// client-go's default controller rate limiter does, and a Lease of 15s,
// renewed within 10s and retried every 2s, as kube-controller-manager's is.
// Name, LeaderElection.Namespace and LeaderElection.Identity have none, so
// they are "", which CheckName, CheckNamespace and CheckIdentity reject:
// the end user always sources all three.
func DefaultSettings() Settings {
	return Settings{
		Name:           "",
		Namespace:      "",
		Workers:        1,
		Resync:         10 * time.Hour,
		RetryBaseDelay: 5 * time.Millisecond,
		RetryMaxDelay:  1000 * time.Second,
		LeaderElection: LeaderElection{
			Namespace:     "",
			Identity:      "",
			LeaseDuration: 15 * time.Second,
			RenewDeadline: 10 * time.Second,
			RetryPeriod:   2 * time.Second,
		},
	}
}

// The causes the checks report. None of them names a field: whoever calls a
// check names the field, or the variable that sourced it.
var (
	errEmpty          = errors.New("must not be empty")
	errNotPositive    = errors.New("must be positive")
	errBelowOne       = errors.New("must be at least 1")
	errSubdomainLong  = fmt.Errorf("must be at most %d characters", validation.DNS1123SubdomainMaxLength)
	errLabelLong      = fmt.Errorf("must be at most %d characters", validation.DNS1123LabelMaxLength)
	errSubdomainChars = errors.New("must be a DNS-1123 subdomain: lowercase letters, digits, '-' and '.', starting and ending with a letter or a digit")
	errLabelChars     = errors.New("must be a DNS-1123 label: lowercase letters, digits and '-', starting and ending with a letter or a digit")
)

// CheckName rejects anything but a DNS-1123 subdomain, as the API server
// does for a Lease's name: at most 253 lowercase letters, digits, '-' and
// '.', starting and ending with a letter or a digit.
func CheckName(name string) error {
	switch {
	case name == "":
		return errEmpty
	case len(name) > validation.DNS1123SubdomainMaxLength:
		return errSubdomainLong
	case len(validation.IsDNS1123Subdomain(name)) > 0:
		return errSubdomainChars
	}
	return nil
}

// CheckWatchNamespace accepts "", which means every namespace, and
// otherwise rejects anything but a DNS-1123 label, as CheckNamespace does.
func CheckWatchNamespace(ns string) error {
	if ns == "" {
		return nil
	}
	return CheckNamespace(ns)
}

// CheckNamespace rejects anything but a DNS-1123 label, as the API server
// does for a namespace's name: at most 63 lowercase letters, digits and
// '-', starting and ending with a letter or a digit.
func CheckNamespace(ns string) error {
	switch {
	case ns == "":
		return errEmpty
	case len(ns) > validation.DNS1123LabelMaxLength:
		return errLabelLong
	case len(validation.IsDNS1123Label(ns)) > 0:
		return errLabelChars
	}
	return nil
}

// CheckIdentity rejects "": a replica with no name in the Lease couldn't
// tell its own Lease from another's.
func CheckIdentity(id string) error {
	if id == "" {
		return errEmpty
	}
	return nil
}

// CheckWorkers rejects a count below 1.
func CheckWorkers(n int) error {
	if n < 1 {
		return errBelowOne
	}
	return nil
}

// CheckResync rejects a duration that is not positive.
func CheckResync(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// CheckRetryDelay rejects a duration that is not positive. It checks
// RetryBaseDelay and RetryMaxDelay alike. NewController also rejects a
// RetryMaxDelay below RetryBaseDelay, which takes both fields to tell.
func CheckRetryDelay(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// CheckRetryPeriod rejects a duration that is not positive. NewController
// also checks the bounds between the Lease's durations: LeaseDuration
// longer than RenewDeadline, and RenewDeadline longer than 1.2
// RetryPeriods, which take more than one field to tell.
func CheckRetryPeriod(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}
