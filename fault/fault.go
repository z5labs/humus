// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package fault

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Settings configures an App's failure injection. Start from
// DefaultSettings: Decoder has no default, so it is always set.
type Settings struct {
	// Decoder reads the scenario a request carries from its baggage, and
	// strips it. It owns the wire convention: which members carry a
	// scenario, their grammar, size, expiry and trust. The edge that writes
	// scenarios and every service that reads them agree on it. It is set
	// in code, not sourced, and must be safe for concurrent use, as fit
	// requires.
	//
	// Default: none. Valid: set, which New checks.
	Decoder fit.Decoder

	// MaxDelay is the longest Delay any point waits. A longer Delay is
	// clamped to it, as fit.NewEvaluator does, so services with different
	// maximums still agree on the rest of a scenario.
	//
	// Default: 10s. Valid: positive. Check: CheckMaxDelay.
	MaxDelay time.Duration
}

// DefaultSettings returns the defaults. Its Decoder is nil, which New
// rejects.
func DefaultSettings() Settings {
	return Settings{
		Decoder:  nil,
		MaxDelay: 10 * time.Second,
	}
}

// The causes the checks report. None of them names a field: whoever calls a
// check names the field, or the variable that sourced it.
var (
	errNil         = errors.New("must not be nil")
	errNotPositive = errors.New("must be positive")
)

// CheckMaxDelay reports what is wrong with d without naming the field: it
// rejects a duration that is not positive, as fit.NewEvaluator does.
func CheckMaxDelay(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// checkSettings reports every bad field of settings, each one named, joined
// into one error.
func checkSettings(settings Settings) error {
	var errs []error
	if settings.Decoder == nil {
		errs = append(errs, fmt.Errorf("Settings.Decoder %w", errNil))
	}
	if err := CheckMaxDelay(settings.MaxDelay); err != nil {
		errs = append(errs, fmt.Errorf("Settings.MaxDelay %w", err))
	}
	return errors.Join(errs...)
}

// Injector is an App's failure injection, which the App puts in
// Setup.Faults. Every humus server strips scenarios with it, and every
// humus client evaluates its point with it. The zero Injector is off:
// Eval returns the zero Fault and Strip returns its context unchanged. It
// is safe for concurrent use.
type Injector struct {
	decoder   fit.Decoder
	evaluator fit.Evaluator
}

// New checks settings, reporting every bad field in one error that names
// each one, and returns the Injector, whose fit.Evaluator is built from
// Decoder, MaxDelay and humus's observer.
func New(settings Settings) (Injector, error) {
	if err := checkSettings(settings); err != nil {
		return Injector{}, fmt.Errorf("fault: New: %w", err)
	}
	ev, err := fit.NewEvaluator(settings.Decoder, settings.MaxDelay, observe)
	if err != nil {
		// checkSettings accepts only what fit accepts, so this is humus's
		// bug.
		return Injector{}, fmt.Errorf("fault: New: %w", err)
	}
	return Injector{decoder: settings.Decoder, evaluator: ev}, nil
}

// Eval returns the Fault that the injection point named point emulates for
// the request ctx belongs to, as fit.Evaluator.Eval does. A request with
// no scenario gets the zero Fault. On a request with one, every
// evaluation, matched or not, is a humus.fit.evaluation event on the span
// in ctx.
func (in Injector) Eval(ctx context.Context, point string) fit.Fault {
	if in.decoder == nil {
		return fit.Fault{}
	}
	return in.evaluator.Eval(ctx, point)
}

// Strip returns ctx with the Decoder's members removed from its baggage,
// and whether there were any. Every other member stays. When there were
// none, it returns ctx itself.
func (in Injector) Strip(ctx context.Context) (context.Context, bool) {
	if in.decoder == nil {
		return ctx, false
	}
	b := baggage.FromContext(ctx)
	stripped := in.decoder.Strip(b)
	if stripped.Len() == b.Len() {
		return ctx, false
	}
	return baggage.ContextWithBaggage(ctx, stripped), true
}

// The span event of an evaluation, and its attributes. They are humus's
// own, so each starts with humus.: OpenTelemetry has no convention for
// failure injection. error.type and error.message are OpenTelemetry's.
const (
	evaluationEvent = "humus.fit.evaluation"

	pointKey        = attribute.Key("humus.fit.point")
	ruleKey         = attribute.Key("humus.fit.rule")
	delayKey        = attribute.Key("humus.fit.delay")
	abortKey        = attribute.Key("humus.fit.abort")
	errorMessageKey = attribute.Key("error.message")
)

// observe is humus's observer of fit's evaluations: it adds a
// humus.fit.evaluation event to the span in ctx. fit calls it only on a
// request that carries a scenario.
func observe(ctx context.Context, ev fit.Evaluation) {
	attrs := []attribute.KeyValue{pointKey.String(ev.Point)}
	if ev.Matched {
		attrs = append(attrs,
			ruleKey.String(ev.Rule.Point),
			delayKey.Float64(ev.Fault.Delay.Seconds()),
			abortKey.Bool(ev.Fault.Abort),
		)
	}
	if ev.Err != nil {
		attrs = append(attrs,
			semconv.ErrorType(ev.Err),
			errorMessageKey.String(ev.Err.Error()),
		)
	}
	trace.SpanFromContext(ctx).AddEvent(evaluationEvent, trace.WithAttributes(attrs...))
}
