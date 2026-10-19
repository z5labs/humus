// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/z5labs/humus/fault"
)

// TrustScenario carries the failure scenario that an object's annotation
// holds into each of the object's reconciles, when trust trusts the object.
// The annotation's value is W3C baggage that carries a scenario as the
// App's Decoder, fault.Settings.Decoder, reads it, such as a member of
// jsonscenario's. Only the Decoder's members join the reconcile's context.
// The reconcile's Reconciler, a Finalize's f, the controller's own writes,
// and every request and injection point they reach see the scenario. The
// Client carries it on to the reconciled object's children: it sets the
// annotation on every child the reconcile applies.
//
// trust is asked only about an object whose annotation carries a scenario,
// and only when the App has failure injection. It is set in code, not
// sourced. A panic in trust is recovered and recorded as a panic in the
// Reconciler is, and the object isn't retried. Without TrustScenario, no
// reconcile carries a scenario.
func TrustScenario(annotation string, trust func(obj metav1.Object) bool) Part {
	return trustScenario{annotation: annotation, trust: trust}
}

// trustScenario is the Part TrustScenario returns.
type trustScenario struct {
	annotation string
	trust      func(obj metav1.Object) bool
}

func (trustScenario) part() {}

// errorMessageKey is OpenTelemetry's error.message, which the strip of an
// annotation that isn't W3C baggage carries beside error.type.
const errorMessageKey = attribute.Key("error.message")

// checkTrustScenario returns the mistakes in p, the parts[i] given to a
// Controller's constructor: an annotation that isn't a valid annotation
// key, as the API server checks one, and a nil trust.
func checkTrustScenario(i int, p trustScenario) []error {
	var errs []error
	// The API server checks an annotation's key in lower case.
	if msgs := validation.IsQualifiedName(strings.ToLower(p.annotation)); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("parts[%d], TrustScenario(%q), names an annotation that isn't a valid annotation key: %s",
			i, p.annotation, strings.Join(msgs, "; ")))
	}
	if p.trust == nil {
		errs = append(errs, fmt.Errorf("parts[%d], TrustScenario(%q), has a nil trust", i, p.annotation))
	}
	return errs
}

// scenarios carries the scenario an object's annotation holds into its
// reconciles, for a Controller given a TrustScenario in an App with failure
// injection. The zero scenarios, of any other Controller, reads nothing.
type scenarios struct {
	annotation string
	trust      func(obj metav1.Object) bool
	faults     fault.Injector
}

// newScenarios returns the scenarios of a Controller given p, or the zero
// scenarios when p is nil or faults is the zero Injector of an App without
// failure injection. fault.Injector can't be compared, since it holds a
// func, so its zero is told by reflection.
func newScenarios(p *trustScenario, faults fault.Injector) scenarios {
	if p == nil || reflect.ValueOf(faults).IsZero() {
		return scenarios{}
	}
	return scenarios{annotation: p.annotation, trust: p.trust, faults: faults}
}

// reconciledKey is the context key of a reconcile's reconciled.
type reconciledKey struct{}

// reconciled marks the context of a reconcile by a Controller given a
// TrustScenario, for the Client: the reconciled object's UID, which names
// its children, and the annotation the Client sets on each child it
// applies.
type reconciled struct {
	uid        types.UID
	annotation string
}

// reconciledFrom returns the reconciled ctx is marked with, if it is a
// reconcile's context or one made from it.
func reconciledFrom(ctx context.Context) (reconciled, bool) {
	r, ok := ctx.Value(reconciledKey{}).(reconciled)
	return r, ok
}

// with returns ctx, a reconcile's context, whose span is the reconcile's,
// with the scenario u's annotation carries joined to its baggage, when
// trust trusts u. u is the object as the cache holds it. Whatever it
// carries, ctx is marked with u's reconciled, so the Client can carry a
// scenario on to u's children. It is read as the package doc's four cases
// say:
//
//   - no annotation, or none of the Decoder's members in it: ctx, and
//     nothing is recorded;
//   - a value that isn't W3C baggage: ctx, and a humus.fit.strip event on
//     the span, with error.type and error.message;
//   - an object trust doesn't trust: ctx, and a humus.fit.strip event with
//     no attributes;
//   - a trusted object: ctx with the Decoder's members of the annotation,
//     and no other member, joined to its baggage.
//
// trust is handed its own copy of u, so it can't change the cache. A panic
// in trust is recovered and returned, and ctx with it.
func (sc scenarios) with(ctx context.Context, u *unstructured.Unstructured) (context.Context, *panicked) {
	if sc.trust == nil {
		return ctx, nil
	}
	ctx = context.WithValue(ctx, reconciledKey{}, reconciled{uid: u.GetUID(), annotation: sc.annotation})
	value, ok := u.GetAnnotations()[sc.annotation]
	if !ok {
		return ctx, nil
	}
	b, err := baggage.Parse(value)
	if err != nil {
		// baggage.Parse returns what it could read beside the error, but
		// a value that isn't W3C baggage carries no scenario at all.
		trace.SpanFromContext(ctx).AddEvent(stripEvent, trace.WithAttributes(
			semconv.ErrorType(err),
			errorMessageKey.String(err.Error()),
		))
		return ctx, nil
	}
	// The Decoder's members are those its Strip removes.
	stripped, carried := sc.faults.Strip(baggage.ContextWithBaggage(context.Background(), b))
	if !carried {
		return ctx, nil
	}
	rest := baggage.FromContext(stripped)

	var trusted bool
	obj := u.DeepCopy()
	if p := guard(func() { trusted = sc.trust(obj) }); p != nil {
		return ctx, p
	}
	if !trusted {
		trace.SpanFromContext(ctx).AddEvent(stripEvent)
		return ctx, nil
	}

	joined := baggage.FromContext(ctx)
	for _, m := range b.Members() {
		if rest.Member(m.Key()).Key() != "" {
			continue
		}
		// m is a member Parse read, so SetMember accepts it.
		joined, _ = joined.SetMember(m)
	}
	return baggage.ContextWithBaggage(ctx, joined), nil
}
