// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/workqueue"

	"github.com/z5labs/humus/health"
)

// The overall limit on retries, across every object, which client-go's
// DefaultTypedControllerRateLimiter keeps beside each object's own backoff.
// Neither is a setting.
const (
	retriesPerSecond = 10
	retryBurst       = 100
)

// objectNameKey is the attribute that names the object a reconcile is of.
const objectNameKey = attribute.Key("humus.kube.object.name")

// Serve runs the controller until ctx is done, Shutdown is called, or the
// replica loses the Lease. It:
//
//  1. checks the resource, and each resource an Owns names, is served, by
//     listing it once with a limit of 1. An error, such as a CRD that isn't
//     installed, ends Serve with an error that names the resource;
//  2. starts an informer on the resource, through the dynamic client, and
//     one for each Owns, through the metadata client, all limited to
//     Settings.Namespace. A change to an owned resource queues the object
//     its controller owner reference names, when that reference names the
//     Resource's group and kind;
//  3. waits for the informers' caches to sync, then marks Synced healthy;
//  4. campaigns for the Lease named Settings.Name, in
//     LeaderElection.Namespace, as LeaderElection.Identity. Every replica
//     runs steps 1 to 3, so its caches are warm when it takes the Lease;
//  5. once it holds the Lease, reconciles with Settings.Workers workers.
//     The work queue never hands one object's key to two workers at once.
//
// When ctx is done, or Shutdown is called, the workers stop taking keys:
// each finishes the reconcile it has in flight, and a key one takes after
// that is marked done without being reconciled. The work queue is shut
// down, never drained, so its backlog is left for the next leader. Then
// Serve releases the Lease, stops its informers, and returns nil.
//
// When the replica loses the Lease, the workers stop taking keys, the
// reconciles in flight are cancelled, and Serve returns ErrLostLeadership
// once they have returned.
//
// A reconcile error never ends Serve: the object is retried with backoff.
// Nor does a panic of the Reconciler or of a Finalize's f, which is
// recovered and recorded, and never retried.
//
// A Controller is served once. A Serve called after Shutdown, or after an
// earlier Serve, returns nil at once.
func (c *Controller) Serve(ctx context.Context) error {
	// work is the context reconciles run with: it keeps ctx's values but
	// not its cancellation. Only Shutdown's context running out, or a lost
	// Lease, cancels it.
	work, abort := context.WithCancel(context.WithoutCancel(ctx))
	defer abort()
	cl := &call{abort: abort, served: make(chan struct{})}
	if !c.serving.CompareAndSwap(nil, cl) {
		return nil
	}
	defer close(cl.served)

	// Every context of client-go's carries the logger, so none of its
	// records falls back to klog's global logger.
	logger := newLogger(c.logger)
	ctx = logr.NewContext(ctx, logger)
	work = logr.NewContext(work, logger)

	// running is done once ctx is, or Shutdown is called.
	running, stopRunning := context.WithCancel(ctx)
	defer stopRunning()
	unregister := context.AfterFunc(c.stopped, stopRunning)
	defer unregister()

	// 1. The resource is served.
	_, err := c.resource(c.settings.Namespace).List(running, metav1.ListOptions{Limit: 1})
	if err != nil {
		if running.Err() != nil {
			return nil
		}
		return fmt.Errorf("kube: listing %s: %w", c.gvr, err)
	}
	for _, gvr := range c.owned {
		_, err := c.ownedResource(gvr).List(running, metav1.ListOptions{Limit: 1})
		if err != nil {
			if running.Err() != nil {
				return nil
			}
			return fmt.Errorf("kube: listing %s, which the Controller owns: %w", gvr, err)
		}
	}

	// 2. The informers, and the queue their events fill. Every replica
	// queues keys, so a new leader starts with them.
	queue := c.newQueue(logger)
	defer queue.ShutDown()
	informer := dynamicinformer.NewFilteredDynamicInformer(c.dyn, c.gvr, c.settings.Namespace, c.settings.Resync, cache.Indexers{}, nil).Informer()
	enqueue := func(obj any) {
		key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
		if err == nil {
			queue.Add(key)
		}
	}
	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueue,
		UpdateFunc: func(_, obj any) { enqueue(obj) },
		DeleteFunc: enqueue,
	})
	if err != nil {
		return fmt.Errorf("kube: watching %s: %w", c.gvr, err)
	}
	informers := []cache.SharedIndexInformer{informer}
	enqueueOwner := func(obj any) {
		if key, ok := c.ownerKey(obj); ok {
			queue.Add(key)
		}
	}
	for _, gvr := range c.owned {
		// No resync: the owner's own informer resyncs it.
		owned := metadatainformer.NewFilteredMetadataInformer(c.meta, gvr, c.settings.Namespace, 0, cache.Indexers{}, nil).Informer()
		_, err = owned.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    enqueueOwner,
			UpdateFunc: func(_, obj any) { enqueueOwner(obj) },
			DeleteFunc: enqueueOwner,
		})
		if err != nil {
			return fmt.Errorf("kube: watching %s: %w", gvr, err)
		}
		informers = append(informers, owned)
	}
	informing, stopInforming := context.WithCancel(context.WithoutCancel(ctx))
	var informed sync.WaitGroup
	synced := make([]cache.InformerSynced, 0, len(informers))
	for _, inf := range informers {
		informed.Go(func() { inf.RunWithContext(informing) })
		synced = append(synced, inf.HasSynced)
	}
	defer func() {
		stopInforming()
		informed.Wait()
	}()

	// 3. The caches sync.
	if !cache.WaitForCacheSync(running.Done(), synced...) {
		return nil
	}
	cl.synced.Store(true)

	// 4. The Lease. Its own context, which only Serve cancels, so the
	// Lease is renewed until the reconciles in flight have finished.
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: c.settings.Name, Namespace: c.settings.LeaderElection.Namespace},
		Client:     c.leases,
		LockConfig: resourcelock.ResourceLockConfig{Identity: c.settings.LeaderElection.Identity},
	}
	// The callback runs on a goroutine of client-go's, which only hands
	// the leading context over, and never blocks.
	leading := make(chan context.Context, 1)
	le := c.settings.LeaderElection
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: le.LeaseDuration,
		RenewDeadline: le.RenewDeadline,
		RetryPeriod:   le.RetryPeriod,
		Name:          c.settings.Name,
		// Serve releases the Lease itself, once the reconciles in flight
		// have finished. client-go's own release would keep a replica
		// that lost the Lease reconciling for up to RenewDeadline more.
		ReleaseOnCancel: false,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) { leading <- ctx },
			OnStoppedLeading: func() {},
		},
	})
	if err != nil {
		return fmt.Errorf("kube: electing a leader: %w", err)
	}
	electing, stopElecting := context.WithCancel(context.WithoutCancel(ctx))
	defer stopElecting()
	elected := make(chan struct{})
	go func() {
		defer close(elected)
		elector.Run(electing)
	}()

	// 5. The workers, once the replica leads.
	var (
		workers  sync.WaitGroup
		stopping atomic.Bool
		lost     bool
	)
	indexer := informer.GetIndexer()
wait:
	for {
		select {
		case <-running.Done():
			break wait
		case <-elected:
			// Run returns on its own only when the Lease is lost.
			lost = true
			break wait
		case <-leading:
			for range c.settings.Workers {
				workers.Go(func() { c.worker(work, queue, indexer, &stopping) })
			}
		}
	}

	// The workers stop taking keys, and the queue hands each one the rest
	// of its backlog, marked done unreconciled, until it is empty.
	stopping.Store(true)
	if lost {
		abort()
	}
	queue.ShutDown()
	workers.Wait()

	stopElecting()
	<-elected
	// Once Shutdown's context is done, the Lease is left to expire.
	if !lost && work.Err() == nil && elector.IsLeader() {
		c.release(work, lock)
	}
	if lost {
		return ErrLostLeadership
	}
	return nil
}

// newQueue returns the work queue of object keys. Each key backs off from
// RetryBaseDelay, doubling to RetryMaxDelay, under an overall limit of 10
// retries a second, bursting to 100, as client-go's
// DefaultTypedControllerRateLimiter limits them. It has no name, so it
// registers no metrics with client-go's global provider.
func (c *Controller) newQueue(logger logr.Logger) workqueue.TypedRateLimitingInterface[string] {
	limiter := workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](c.settings.RetryBaseDelay, c.settings.RetryMaxDelay),
		&workqueue.TypedBucketRateLimiter[string]{Limiter: rate.NewLimiter(rate.Limit(retriesPerSecond), retryBurst)},
	)
	return workqueue.NewTypedRateLimitingQueueWithConfig(limiter, workqueue.TypedRateLimitingQueueConfig[string]{
		DelayingQueue: workqueue.NewTypedDelayingQueueWithConfig(workqueue.TypedDelayingQueueConfig[string]{
			Logger: &logger,
		}),
	})
}

// release gives up the Lease, as client-go's own release does, if this
// replica still holds it: its holder becomes empty, and it expires a second
// later, so the next replica takes it without waiting out LeaseDuration.
// It is given RenewDeadline, as client-go's is, and gives up at once when
// Shutdown's context is done; the Lease then expires on its own.
func (c *Controller) release(work context.Context, lock *resourcelock.LeaseLock) {
	ctx, cancel := context.WithTimeout(work, c.settings.LeaderElection.RenewDeadline)
	defer cancel()
	logger := logr.FromContextOrDiscard(ctx)
	rec, _, err := lock.Get(ctx)
	if err != nil {
		logger.Error(err, "Failed to release lease", "lock", lock.Describe())
		return
	}
	if rec.HolderIdentity != lock.Identity() {
		return
	}
	now := metav1.Now()
	err = lock.Update(ctx, resourcelock.LeaderElectionRecord{
		LeaderTransitions:    rec.LeaderTransitions,
		LeaseDurationSeconds: 1,
		RenewTime:            now,
		AcquireTime:          now,
	})
	if err != nil {
		logger.Error(err, "Failed to release lease", "lock", lock.Describe())
		return
	}
	logger.Info("Released lease", "lock", lock.Describe())
}

// worker reconciles the keys it takes from queue until queue is shut down.
// Once stopping is set, it marks each key it takes done without
// reconciling it.
func (c *Controller) worker(work context.Context, queue workqueue.TypedRateLimitingInterface[string], indexer cache.Indexer, stopping *atomic.Bool) {
	for {
		key, quit := queue.Get()
		if quit {
			return
		}
		if !stopping.Load() {
			c.process(work, queue, indexer, key)
		}
		queue.Done(key)
	}
}

// process reconciles the object of key, as the cache holds it, and settles
// the key: a panic forgets it, an error retries it with backoff, a
// Result.After queues it again after that long, and otherwise it is
// forgotten until the object changes or the resync comes. An object being
// deleted is finalized instead, when the controller's finalizer holds it,
// and otherwise left alone.
func (c *Controller) process(work context.Context, queue workqueue.TypedRateLimitingInterface[string], indexer cache.Indexer, key string) {
	obj, exists, err := indexer.GetByKey(key)
	u, ok := obj.(*unstructured.Unstructured)
	if err != nil || !exists || !ok {
		// Gone: what it owns is garbage collected through its owner
		// references.
		queue.Forget(key)
		return
	}
	if u.GetDeletionTimestamp() != nil && (c.finalize == nil || !hasFinalizer(u, c.finalizer)) {
		// Being deleted, and no Finalize of the controller's holds it.
		queue.Forget(key)
		return
	}

	after, recovered, err := c.reconcileObject(work, u)
	switch {
	case recovered:
		// A panic is a bug in the controller, which a retry would only
		// hit again: the object waits for a change, the resync, or a new
		// leader, as a settled one does.
		queue.Forget(key)
	case err != nil:
		queue.AddRateLimited(key)
	case after > 0:
		queue.Forget(key)
		queue.AddAfter(key, after)
	default:
		queue.Forget(key)
	}
}

// reconcileObject reconciles u, as a root span, and applies the status the
// Reconciler returned, whether or not it returned an error. A failure sets
// the span's status to Error, and is one WARN record in the span's context.
//
// A panic of the Reconciler, or of a Finalize's f, is recovered, recorded
// by recordPanic, and reported as recovered. Nothing is applied after it: no
// status, since there is no Result, and no removal of the finalizer.
//
// With a Finalize, the span holds what the controller does in place of the
// Reconciler: it adds the finalizer to an object that hasn't got it, and
// calls f for an object being deleted, then removes the finalizer once f
// returns nil.
func (c *Controller) reconcileObject(work context.Context, u *unstructured.Unstructured) (after time.Duration, recovered bool, err error) {
	attrs := make([]attribute.KeyValue, 0, 2)
	if ns := u.GetNamespace(); ns != "" {
		attrs = append(attrs, semconv.K8SNamespaceName(ns))
	}
	attrs = append(attrs, objectNameKey.String(u.GetName()))
	ctx, span := c.tracer.Start(work, "reconcile "+c.kind,
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	defer span.End()

	switch {
	case u.GetDeletionTimestamp() != nil:
		// process finalizes only an object its finalizer holds.
		if p := guard(func() { err = c.finalize(ctx, u) }); p != nil {
			c.recordPanic(ctx, attrs, p)
			return 0, true, nil
		}
		if err == nil {
			err = c.applyFinalizer(ctx, u, false)
		}
	case c.finalize != nil && !hasFinalizer(u, c.finalizer):
		// The apply changes the object, which reconciles it again.
		err = c.applyFinalizer(ctx, u, true)
	default:
		var status any
		if p := guard(func() { status, after, err = c.reconcile(ctx, u) }); p != nil {
			c.recordPanic(ctx, attrs, p)
			return 0, true, nil
		}
		if status != nil {
			err = errors.Join(err, c.applyStatus(ctx, u, status))
		}
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		c.emit(ctx, log.SeverityWarn, "reconcile failed", append(attrs,
			semconv.ErrorType(err),
			semconv.ExceptionMessage(err.Error()),
		)...)
	}
	return after, false, err
}

// applyStatus applies status to u's status subresource with server-side
// apply, under the field manager Settings.Name, forcing, since the
// controller owns the status whole.
func (c *Controller) applyStatus(ctx context.Context, u *unstructured.Unstructured, status any) error {
	metadata := map[string]any{"name": u.GetName()}
	if ns := u.GetNamespace(); ns != "" {
		metadata["namespace"] = ns
	}
	patch := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": c.apiVersion,
		"kind":       c.kind,
		"metadata":   metadata,
		"status":     status,
	}}
	_, err := c.resource(u.GetNamespace()).ApplyStatus(ctx, u.GetName(), patch, metav1.ApplyOptions{
		FieldManager: c.settings.Name,
		Force:        true,
	})
	if err != nil {
		return fmt.Errorf("kube: applying the status of %s: %w", objectName(u), err)
	}
	return nil
}

// resource returns the dynamic client of the resource's objects in ns, or
// in every namespace when ns is "", as a cluster-scoped resource's are.
func (c *Controller) resource(ns string) dynamic.ResourceInterface {
	r := c.dyn.Resource(c.gvr)
	if c.namespaced && ns != "" {
		return r.Namespace(ns)
	}
	return r
}

// objectName names u in an error: namespace/name, or its name alone when it
// is in no namespace.
func objectName(u *unstructured.Unstructured) string {
	if ns := u.GetNamespace(); ns != "" {
		return ns + "/" + u.GetName()
	}
	return u.GetName()
}

// emit emits a record of the controller's own, with ctx.
func (c *Controller) emit(ctx context.Context, sev log.Severity, body string, attrs ...attribute.KeyValue) {
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(sev.String())
	r.SetBody(attribute.StringValue(body))
	r.AddAttributes(attrs...)
	c.logger.Emit(ctx, r)
}

// Synced returns a monitor that is healthy once the controller's caches have
// synced, its resource's and each owned resource's: unhealthy until then, and healthy from then until Serve returns,
// on every replica, leader or not, so a rollout isn't held up by the
// replicas waiting for the Lease. It never returns an error. A controller is
// made ready with
//
//	health.Readiness(ctrl.Synced())
//
// It is for readiness only: restarting the pod doesn't make the Lease
// come sooner.
//
// Synced may be called before Serve, in the end user's Init. The monitor
// reports the Controller's state when it is checked, and is safe for
// concurrent use.
func (c *Controller) Synced() health.Monitor {
	return syncedMonitor{c: c}
}

// syncedMonitor is the monitor Synced returns.
type syncedMonitor struct {
	c *Controller
}

// Healthy reports whether the Serve of the Controller has synced its caches
// and not yet returned. It never returns an error.
func (m syncedMonitor) Healthy(context.Context) (bool, error) {
	cl := m.c.serving.Load()
	if cl == nil || cl == notServed {
		return false, nil
	}
	select {
	case <-cl.served:
		return false, nil
	default:
		return cl.synced.Load(), nil
	}
}

// Shutdown stops Serve and waits for it to return. Serve stops taking keys,
// lets each reconcile in flight finish, releases the Lease, and stops its
// informers.
//
// If ctx is done first, or is done already, Shutdown cancels the context
// the reconciles in flight run with, waits for Serve to return, and returns
// an error that wraps ctx's error. Serve then gives up releasing the Lease,
// which expires on its own.
//
// Shutdown returns nil at once when Serve was never called, after which
// Serve returns nil at once, and when Serve has already returned, so a
// second Shutdown returns nil.
func (c *Controller) Shutdown(ctx context.Context) error {
	c.serving.CompareAndSwap(nil, notServed)
	c.stop()
	cl := c.serving.Load()
	if cl == notServed {
		return nil
	}
	select {
	case <-cl.served:
		return nil
	default:
	}
	if ctx.Err() == nil {
		select {
		case <-cl.served:
			return nil
		case <-ctx.Done():
		}
	}
	cl.abort()
	<-cl.served
	return fmt.Errorf("kube: shutting down the Controller: %w", ctx.Err())
}
