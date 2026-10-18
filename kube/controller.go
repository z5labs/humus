// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/dynamic"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/leaderelection"

	"github.com/z5labs/humus"
)

// scopeName is the instrumentation scope of the package's own telemetry: the
// reconciles' spans, and every record, client-go's included.
const scopeName = "github.com/z5labs/humus/kube"

// Reconciler brings the cluster to what an object's spec declares.
type Reconciler[Spec, Status any] interface {
	// Reconcile is handed obj, its own copy of the object as the cache last
	// saw it, and returns what obj's status is now.
	Reconcile(ctx context.Context, obj Object[Spec, Status]) (Result[Status], error)
}

// ReconcilerFunc is a function that is a Reconciler.
type ReconcilerFunc[Spec, Status any] func(ctx context.Context, obj Object[Spec, Status]) (Result[Status], error)

// Reconcile calls f(ctx, obj).
func (f ReconcilerFunc[Spec, Status]) Reconcile(ctx context.Context, obj Object[Spec, Status]) (Result[Status], error) {
	return f(ctx, obj)
}

// Result is what a reconcile leaves its object with.
type Result[Status any] struct {
	// Status is the object's status now. nil leaves it as it is. The
	// controller applies it when it differs from the object's, whether or
	// not Reconcile returned an error, so a failure can be a condition.
	Status *Status

	// After reconciles the object again after this long, unless it changes
	// first. Zero waits for a change, or for the resync. A Reconcile that
	// returns an error is retried with backoff instead.
	After time.Duration
}

// Part is a part of a controller beyond its Reconciler, given to
// NewController.
type Part interface {
	// part keeps Part's implementations in the package.
	part()
}

// ErrLostLeadership is what Serve returns when the replica loses the Lease
// while it serves: its renewal failed for LeaderElection.RenewDeadline, or
// another replica took the Lease. Another replica may already be
// reconciling, so the App exits 1, and the pod campaigns again when it
// restarts.
var ErrLostLeadership = errors.New("kube: lost the lease")

// reconcileFunc reconciles one object from the cache. status is what the
// object's status is to be applied as, or nil when it is left alone: when
// the Reconciler returned no status, or one that encodes as the object's
// own. after is the Reconciler's Result.After.
type reconcileFunc func(ctx context.Context, u *unstructured.Unstructured) (status any, after time.Duration, err error)

// Controller reconciles the objects of one Resource. It is a humus.Service,
// which NewController builds.
type Controller struct {
	settings   Settings
	gvr        schema.GroupVersionResource
	apiVersion string
	group      string
	kind       string
	namespaced bool
	reconcile  reconcileFunc

	// owned are the resources each Owns names, in order.
	owned []schema.GroupVersionResource

	// finalizer is the name a Finalize gives, and finalize calls its f.
	// finalize is nil without a Finalize.
	finalizer string
	finalize  finalizeFunc

	dyn    dynamic.Interface
	meta   metadata.Interface
	leases coordinationv1.LeasesGetter

	tracer trace.Tracer
	logger log.Logger

	// stopped is done once Shutdown is called.
	stopped context.Context
	stop    context.CancelFunc

	// serving is set once: by the first Serve, to that Serve's call, or by
	// a Shutdown called before any Serve, to notServed.
	serving atomic.Pointer[call]
}

// call is one call of Serve: the function that cancels the context the
// reconciles run with, served, which is closed when Serve returns, and
// synced, which is set once the caches have synced.
type call struct {
	abort  context.CancelFunc
	served chan struct{}
	synced atomic.Bool
}

// notServed is the call of a Controller that was shut down before it was
// served.
var notServed = &call{}

// NewController returns a Controller of r's objects, which rec reconciles.
// It opens no connection: Serve does, through c.
//
// It returns one error that names every mistake it finds:
//   - each field of settings that fails its check, named after the field;
//   - a RetryMaxDelay below RetryBaseDelay, a LeaseDuration not longer than
//     RenewDeadline, or a RenewDeadline not longer than 1.2 RetryPeriods,
//     as client-go's leader election requires;
//   - each mistake in r, as [Resource.CRD] reports it;
//   - a Settings.Namespace that isn't "" for a Resource whose Scope is
//     Cluster, whose objects are in no namespace;
//   - an Owns whose GroupVersionResource has no Version or no Resource;
//   - a Finalize whose name is empty, whose f is nil, or whose f takes
//     something other than an Object[Spec, Status], and a second Finalize;
//   - a nil Reconciler, a nil Part, a nil Setup or a nil Client.
//
// The Controller reports through s.Telemetry, and never through
// OpenTelemetry's globals, under the scope github.com/z5labs/humus/kube.
// The package doc says what it records.
func NewController[Spec, Status any](s *humus.Setup, c *Client, settings Settings, r Resource[Spec, Status], rec Reconciler[Spec, Status], parts ...Part) (*Controller, error) {
	var errs []error
	if s == nil {
		errs = append(errs, errors.New("the Setup must not be nil"))
	}
	if c == nil {
		errs = append(errs, errors.New("the Client must not be nil"))
	}
	errs = append(errs, checkSettings(settings)...)
	if _, crdErrs := r.crd(); len(crdErrs) > 0 {
		errs = append(errs, fmt.Errorf("the CRD of %s: %w", r.name(), errors.Join(crdErrs...)))
	}
	if r.Scope == Cluster && settings.Namespace != "" {
		errs = append(errs, fmt.Errorf("Settings.Namespace is %q, but %s is cluster-scoped, so its objects are in no namespace",
			settings.Namespace, r.name()))
	}
	if rec == nil {
		errs = append(errs, errors.New("the Reconciler must not be nil"))
	} else if f, ok := rec.(ReconcilerFunc[Spec, Status]); ok && f == nil {
		errs = append(errs, errors.New("the Reconciler must not be a nil ReconcilerFunc"))
	}
	var (
		owned     []schema.GroupVersionResource
		finalizer string
		finalizes finalizeFunc
		finalized = -1
	)
	for i, p := range parts {
		switch p := p.(type) {
		case nil:
			errs = append(errs, fmt.Errorf("parts[%d] must not be nil", i))
		case owns:
			if p.gvr.Version == "" {
				errs = append(errs, fmt.Errorf("parts[%d], Owns(%s), has no Version", i, p.gvr))
			}
			if p.gvr.Resource == "" {
				errs = append(errs, fmt.Errorf("parts[%d], Owns(%s), has no Resource", i, p.gvr))
			}
			owned = append(owned, p.gvr)
		case finalize:
			if finalized >= 0 {
				errs = append(errs, fmt.Errorf("parts[%d], Finalize(%q), is a second Finalize, after parts[%d]", i, p.name, finalized))
			}
			finalized = i
			if p.name == "" {
				errs = append(errs, fmt.Errorf("parts[%d], a Finalize, has an empty name", i))
			}
			if p.nilF {
				errs = append(errs, fmt.Errorf("parts[%d], Finalize(%q), has a nil f", i, p.name))
			}
			f, ok := p.f.(func(context.Context, Object[Spec, Status]) error)
			if !ok {
				errs = append(errs, fmt.Errorf("parts[%d], Finalize(%q), takes a %s, but the Controller's objects are a %s",
					i, p.name, p.typ, reflect.TypeFor[Object[Spec, Status]]()))
			} else if !p.nilF {
				finalizer, finalizes = p.name, finalizerOf(f)
			}
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("kube: NewController: %w", errors.Join(errs...))
	}

	// None opens a connection: each only builds a client on the Client's
	// transport.
	dyn, err := dynamic.NewForConfig(c.Config())
	if err != nil {
		return nil, fmt.Errorf("kube: NewController: %w", err)
	}
	meta, err := metadata.NewForConfig(c.Config())
	if err != nil {
		return nil, fmt.Errorf("kube: NewController: %w", err)
	}
	leases, err := coordinationv1.NewForConfig(c.Config())
	if err != nil {
		return nil, fmt.Errorf("kube: NewController: %w", err)
	}

	stopped, stop := context.WithCancel(context.Background())
	return &Controller{
		settings:   settings,
		gvr:        r.GVR(),
		apiVersion: schema.GroupVersion{Group: r.Group, Version: r.Version}.String(),
		group:      r.Group,
		kind:       r.Kind,
		namespaced: r.Scope == Namespaced,
		reconcile:  reconcilerOf(rec),
		owned:      owned,
		finalizer:  finalizer,
		finalize:   finalizes,
		dyn:        dyn,
		meta:       meta,
		leases:     leases,
		tracer:     s.Telemetry.TracerProvider.Tracer(scopeName),
		logger:     s.Telemetry.LoggerProvider.Logger(scopeName),
		stopped:    stopped,
		stop:       stop,
	}, nil
}

// checkSettings runs every check on settings, and the bounds that take more
// than one field to tell. It returns an error for each failure, each naming
// its field.
func checkSettings(settings Settings) []error {
	var errs []error
	check := func(field string, err error) bool {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", "Settings."+field, err))
			return false
		}
		return true
	}
	le := settings.LeaderElection
	check("Name", CheckName(settings.Name))
	check("Namespace", CheckWatchNamespace(settings.Namespace))
	check("Workers", CheckWorkers(settings.Workers))
	check("Resync", CheckResync(settings.Resync))
	base := check("RetryBaseDelay", CheckRetryDelay(settings.RetryBaseDelay))
	maxDelay := check("RetryMaxDelay", CheckRetryDelay(settings.RetryMaxDelay))
	check("LeaderElection.Namespace", CheckNamespace(le.Namespace))
	check("LeaderElection.Identity", CheckIdentity(le.Identity))
	period := check("LeaderElection.RetryPeriod", CheckRetryPeriod(le.RetryPeriod))

	if base && maxDelay && settings.RetryMaxDelay < settings.RetryBaseDelay {
		errs = append(errs, fmt.Errorf("Settings.RetryMaxDelay, %s, must be at least Settings.RetryBaseDelay, %s",
			settings.RetryMaxDelay, settings.RetryBaseDelay))
	}
	if le.LeaseDuration <= le.RenewDeadline {
		errs = append(errs, fmt.Errorf("Settings.LeaderElection.LeaseDuration, %s, must be longer than Settings.LeaderElection.RenewDeadline, %s",
			le.LeaseDuration, le.RenewDeadline))
	}
	// The bound client-go's leader election checks, to the nanosecond.
	if period && le.RenewDeadline <= time.Duration(leaderelection.JitterFactor*float64(le.RetryPeriod)) {
		errs = append(errs, fmt.Errorf("Settings.LeaderElection.RenewDeadline, %s, must be longer than %g Settings.LeaderElection.RetryPeriods, %s",
			le.RenewDeadline, leaderelection.JitterFactor, time.Duration(leaderelection.JitterFactor*float64(le.RetryPeriod))))
	}
	return errs
}

// reconcilerOf returns the reconcileFunc that hands rec an Object decoded
// from the cache's object.
//
// The decode is the copy: the object is encoded as JSON and decoded into a
// new Object, as encoding/json decodes it, so nothing rec does changes the
// cache. The object's status is encoded again before rec is called, since
// rec may change what obj.Status's slices and maps share, and the status
// rec returns is applied only when its encoding differs.
func reconcilerOf[Spec, Status any](rec Reconciler[Spec, Status]) reconcileFunc {
	return func(ctx context.Context, u *unstructured.Unstructured) (any, time.Duration, error) {
		obj, err := decodeObject[Spec, Status](u)
		if err != nil {
			return nil, 0, err
		}
		before, err := json.Marshal(obj.Status)
		if err != nil {
			return nil, 0, fmt.Errorf("kube: encoding the object's status: %w", err)
		}

		res, err := rec.Reconcile(ctx, obj)
		if res.Status == nil {
			return nil, res.After, err
		}
		now, encErr := json.Marshal(res.Status)
		if encErr != nil {
			return nil, res.After, errors.Join(err, fmt.Errorf("kube: encoding the Result's status: %w", encErr))
		}
		if bytes.Equal(before, now) {
			return nil, res.After, err
		}
		// Numbers decode as int64 where they can, as client-go's
		// unstructured objects hold them.
		var status any
		if decErr := utiljson.Unmarshal(now, &status); decErr != nil {
			return nil, res.After, errors.Join(err, fmt.Errorf("kube: decoding the Result's status: %w", decErr))
		}
		return status, res.After, err
	}
}

// decodeObject returns a new Object decoded from u, as encoding/json decodes
// its JSON encoding, so it shares nothing with u.
func decodeObject[Spec, Status any](u *unstructured.Unstructured) (Object[Spec, Status], error) {
	var obj Object[Spec, Status]
	b, err := u.MarshalJSON()
	if err != nil {
		return obj, fmt.Errorf("kube: encoding the object: %w", err)
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return obj, fmt.Errorf("kube: decoding the object: %w", err)
	}
	return obj, nil
}
