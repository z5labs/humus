// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/client-go/util/workqueue"
)

// The keys of the attributes humus defines for the controller's
// instruments.
const (
	controllerNameKey = attribute.Key("humus.kube.controller.name")
	outcomeKey        = attribute.Key("humus.kube.controller.reconcile.outcome")
)

// The values of humus.kube.controller.reconcile.outcome: error is a key
// retried with backoff, panic a recovered panic, and success everything
// else, Result.After included.
const (
	outcomeSuccess = "success"
	outcomeError   = "error"
	outcomePanic   = "panic"
)

// durationBoundaries are OpenTelemetry's advice for a histogram of a
// duration in seconds, as http.server.request.duration takes it. The SDK's
// default boundaries are for milliseconds.
var durationBoundaries = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

// instruments are the controller's own instruments. Every one but
// reconcileDuration is recorded by client-go's work queue, through
// queueMetrics.
type instruments struct {
	// name is the humus.kube.controller.name of reconcileDuration's data
	// points: Settings.Name, which also names the work queue.
	name attribute.KeyValue

	depth             metric.Int64UpDownCounter
	adds              metric.Int64Counter
	queueDuration     metric.Float64Histogram
	reconcileDuration metric.Float64Histogram
	unfinished        metric.Float64Gauge
	longest           metric.Float64Gauge
}

// newInstruments creates the instruments of the controller named name from
// meter. It returns every error creating one, joined.
func newInstruments(meter metric.Meter, name string) (*instruments, error) {
	m := &instruments{name: controllerNameKey.String(name)}
	var errs [6]error
	m.depth, errs[0] = meter.Int64UpDownCounter("humus.kube.controller.queue.depth",
		metric.WithUnit("{object}"),
		metric.WithDescription("Objects waiting in the work queue for a worker."),
	)
	m.adds, errs[1] = meter.Int64Counter("humus.kube.controller.queue.adds",
		metric.WithUnit("{object}"),
		metric.WithDescription("Objects added to the work queue, not counting one already waiting."),
	)
	m.queueDuration, errs[2] = meter.Float64Histogram("humus.kube.controller.queue.duration",
		metric.WithUnit("s"),
		metric.WithDescription("How long an object waited in the work queue for a worker."),
		metric.WithExplicitBucketBoundaries(durationBoundaries...),
	)
	m.reconcileDuration, errs[3] = meter.Float64Histogram("humus.kube.controller.reconcile.duration",
		metric.WithUnit("s"),
		metric.WithDescription("How long each reconcile took, by its outcome."),
		metric.WithExplicitBucketBoundaries(durationBoundaries...),
	)
	m.unfinished, errs[4] = meter.Float64Gauge("humus.kube.controller.reconcile.unfinished_duration",
		metric.WithUnit("s"),
		metric.WithDescription("How long the reconciles in flight have run, added together."),
	)
	m.longest, errs[5] = meter.Float64Gauge("humus.kube.controller.reconcile.longest_unfinished_duration",
		metric.WithUnit("s"),
		metric.WithDescription("How long the longest reconcile in flight has run."),
	)
	if err := errors.Join(errs[:]...); err != nil {
		return nil, err
	}
	return m, nil
}

// reconciled records one reconcile of seconds, which ended in outcome, in
// humus.kube.controller.reconcile.duration.
func (m *instruments) reconciled(ctx context.Context, seconds float64, outcome string) {
	m.reconcileDuration.Record(ctx, seconds, metric.WithAttributes(m.name, outcomeKey.String(outcome)))
}

// queueMetrics is the work queue's workqueue.MetricsProvider: the
// controller's own, set in the queue's config, never client-go's global
// one. client-go hands it the queue's name, Settings.Name, which it records
// as humus.kube.controller.name.
//
// The work duration and the retries aren't recorded: the first times what
// reconcile.duration times, without the outcome, and the second counts
// every AddAfter, so every Result.After would be a retry.
type queueMetrics struct {
	m *instruments
}

// named returns the measurement option of a queue named name's data
// points.
func named(name string) metric.MeasurementOption {
	return metric.WithAttributeSet(attribute.NewSet(controllerNameKey.String(name)))
}

// NewDepthMetric returns humus.kube.controller.queue.depth.
func (p queueMetrics) NewDepthMetric(name string) workqueue.GaugeMetric {
	return depthMetric{c: p.m.depth, opt: named(name)}
}

// NewAddsMetric returns humus.kube.controller.queue.adds.
func (p queueMetrics) NewAddsMetric(name string) workqueue.CounterMetric {
	return addsMetric{c: p.m.adds, opt: named(name)}
}

// NewLatencyMetric returns humus.kube.controller.queue.duration.
func (p queueMetrics) NewLatencyMetric(name string) workqueue.HistogramMetric {
	return histogramMetric{h: p.m.queueDuration, opt: named(name)}
}

// NewWorkDurationMetric returns a no-op: reconcile.duration times the same
// thing, with the outcome.
func (queueMetrics) NewWorkDurationMetric(string) workqueue.HistogramMetric {
	return noopMetric{}
}

// NewUnfinishedWorkSecondsMetric returns
// humus.kube.controller.reconcile.unfinished_duration.
func (p queueMetrics) NewUnfinishedWorkSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return gaugeMetric{g: p.m.unfinished, opt: named(name)}
}

// NewLongestRunningProcessorSecondsMetric returns
// humus.kube.controller.reconcile.longest_unfinished_duration.
func (p queueMetrics) NewLongestRunningProcessorSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return gaugeMetric{g: p.m.longest, opt: named(name)}
}

// NewRetriesMetric returns a no-op: the queue counts every AddAfter as a
// retry, and the error outcome of reconcile.duration counts the retries
// exactly.
func (queueMetrics) NewRetriesMetric(string) workqueue.CounterMetric {
	return noopMetric{}
}

// depthMetric is queue.depth of one queue.
type depthMetric struct {
	c   metric.Int64UpDownCounter
	opt metric.MeasurementOption
}

func (d depthMetric) Inc() { d.c.Add(context.Background(), 1, d.opt) }
func (d depthMetric) Dec() { d.c.Add(context.Background(), -1, d.opt) }

// addsMetric is queue.adds of one queue.
type addsMetric struct {
	c   metric.Int64Counter
	opt metric.MeasurementOption
}

func (a addsMetric) Inc() { a.c.Add(context.Background(), 1, a.opt) }

// histogramMetric is a histogram of seconds of one queue.
type histogramMetric struct {
	h   metric.Float64Histogram
	opt metric.MeasurementOption
}

func (h histogramMetric) Observe(seconds float64) { h.h.Record(context.Background(), seconds, h.opt) }

// gaugeMetric is a gauge of seconds of one queue.
type gaugeMetric struct {
	g   metric.Float64Gauge
	opt metric.MeasurementOption
}

func (g gaugeMetric) Set(seconds float64) { g.g.Record(context.Background(), seconds, g.opt) }

// noopMetric is a metric of the queue's that isn't recorded.
type noopMetric struct{}

func (noopMetric) Inc()            {}
func (noopMetric) Observe(float64) {}
