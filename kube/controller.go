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
// reconciles' spans, the controller's instruments, and every record,
// client-go's included.
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
// NewController or NewKindController.
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
// own, and always for a Kind. after is the Reconciler's Result.After, or
// the after a KindReconciler returned.
type reconcileFunc func(ctx context.Context, u *unstructured.Unstructured) (status any, after time.Duration, err error)

// Controller reconciles the objects of one Resource or one Kind. It is a
// humus.Service, which NewController or NewKindController builds.
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

	// scenarios reads the scenario an object's annotation carries into its
	// reconciles. It is the zero scenarios, which reads nothing, without a
	// TrustScenario or in an App without failure injection.
	scenarios scenarios

	dyn    dynamic.Interface
	meta   metadata.Interface
	leases coordinationv1.LeasesGetter

	tracer  trace.Tracer
	logger  log.Logger
	metrics *instruments

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
//   - a TrustScenario whose annotation isn't a valid annotation key, or
//     whose trust is nil, and a second TrustScenario;
//   - a nil Reconciler, a nil Part, a nil Setup or a nil Client;
//   - an error creating one of its instruments.
//
// The Controller reports through s.Telemetry, and never through
// OpenTelemetry's globals, under the scope github.com/z5labs/humus/kube.
// The package doc says what it records.
func NewController[Spec, Status any](s *humus.Setup, c *Client, settings Settings, r Resource[Spec, Status], rec Reconciler[Spec, Status], parts ...Part) (*Controller, error) {
	var recErr error
	if rec == nil {
		recErr = errors.New("the Reconciler must not be nil")
	} else if f, ok := rec.(ReconcilerFunc[Spec, Status]); ok && f == nil {
		recErr = errors.New("the Reconciler must not be a nil ReconcilerFunc")
	}
	var errs []error
	if _, crdErrs := r.crd(); len(crdErrs) > 0 {
		errs = append(errs, fmt.Errorf("the CRD of %s: %w", r.name(), errors.Join(crdErrs...)))
	}
	return newController[Object[Spec, Status]](s, c, settings, target{
		ctor:      "NewController",
		name:      r.name(),
		gvr:       r.GVR(),
		kind:      r.Kind,
		scope:     r.Scope,
		mistakes:  errs,
		recErr:    recErr,
		reconcile: reconcilerOf(rec),
	}, parts)
}

// target is what a Controller reconciles, a Resource or a Kind, as its
// constructor hands it to newController.
type target struct {
	// ctor is the constructor, which prefixes its error.
	ctor string
	// name names the Resource or Kind in an error.
	name  string
	gvr   schema.GroupVersionResource
	kind  string
	scope Scope

	// mistakes are those in the Resource or Kind itself, and recErr is a
	// nil Reconciler's, or nil.
	mistakes []error
	recErr   error

	reconcile reconcileFunc
}

// newController returns the Controller of t, whose objects decode into T,
// or one error naming every mistake in s, c, settings, t and parts.
func newController[T any](s *humus.Setup, c *Client, settings Settings, t target, parts []Part) (*Controller, error) {
	var errs []error
	if s == nil {
		errs = append(errs, errors.New("the Setup must not be nil"))
	}
	if c == nil {
		errs = append(errs, errors.New("the Client must not be nil"))
	}
	errs = append(errs, checkSettings(settings)...)
	errs = append(errs, t.mistakes...)
	if t.scope == Cluster && settings.Namespace != "" {
		errs = append(errs, fmt.Errorf("Settings.Namespace is %q, but %s is cluster-scoped, so its objects are in no namespace",
			settings.Namespace, t.name))
	}
	if t.recErr != nil {
		errs = append(errs, t.recErr)
	}
	var (
		owned     []schema.GroupVersionResource
		finalizer string
		finalizes finalizeFunc
		finalized = -1
		trusts    *trustScenario
		trusted   = -1
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
			f, ok := p.f.(func(context.Context, T) error)
			if !ok {
				errs = append(errs, fmt.Errorf("parts[%d], Finalize(%q), takes a %s, but the Controller's objects are a %s",
					i, p.name, p.typ, reflect.TypeFor[T]()))
			} else if !p.nilF {
				finalizer, finalizes = p.name, finalizerOf(f)
			}
		case trustScenario:
			if trusted >= 0 {
				errs = append(errs, fmt.Errorf("parts[%d], TrustScenario(%q), is a second TrustScenario, after parts[%d]", i, p.annotation, trusted))
			}
			trusted = i
			if pErrs := checkTrustScenario(i, p); len(pErrs) > 0 {
				errs = append(errs, pErrs...)
			} else if trusts == nil {
				trusts = &p
			}
		}
	}
	var metrics *instruments
	if s != nil {
		var err error
		metrics, err = newInstruments(s.Telemetry.MeterProvider.Meter(scopeName), settings.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("creating its instruments: %w", err))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("kube: %s: %w", t.ctor, errors.Join(errs...))
	}

	// None opens a connection: each only builds a client on the Client's
	// transport.
	dyn, err := dynamic.NewForConfig(c.Config())
	if err != nil {
		return nil, fmt.Errorf("kube: %s: %w", t.ctor, err)
	}
	meta, err := metadata.NewForConfig(c.Config())
	if err != nil {
		return nil, fmt.Errorf("kube: %s: %w", t.ctor, err)
	}
	leases, err := coordinationv1.NewForConfig(c.Config())
	if err != nil {
		return nil, fmt.Errorf("kube: %s: %w", t.ctor, err)
	}

	stopped, stop := context.WithCancel(context.Background())
	return &Controller{
		settings:   settings,
		gvr:        t.gvr,
		apiVersion: t.gvr.GroupVersion().String(),
		group:      t.gvr.Group,
		kind:       t.kind,
		namespaced: t.scope == Namespaced,
		reconcile:  t.reconcile,
		owned:      owned,
		finalizer:  finalizer,
		finalize:   finalizes,
		scenarios:  newScenarios(trusts, s.Faults),
		dyn:        dyn,
		meta:       meta,
		leases:     leases,
		tracer:     s.Telemetry.TracerProvider.Tracer(scopeName),
		logger:     s.Telemetry.LoggerProvider.Logger(scopeName),
		metrics:    metrics,
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
		obj, err := decode[Object[Spec, Status]](u)
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

// decode returns a new T, an Object or a Kind's type, decoded from u, as
// encoding/json decodes its JSON encoding, so it shares nothing with u.
func decode[T any](u *unstructured.Unstructured) (T, error) {
	var obj T
	b, err := u.MarshalJSON()
	if err != nil {
		return obj, fmt.Errorf("kube: encoding the object: %w", err)
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return obj, fmt.Errorf("kube: decoding the object: %w", err)
	}
	return obj, nil
}
