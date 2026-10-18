// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// The names of the controller's instruments.
const (
	queueDepth        = "humus.kube.controller.queue.depth"
	queueAdds         = "humus.kube.controller.queue.adds"
	queueDuration     = "humus.kube.controller.queue.duration"
	reconcileDuration = "humus.kube.controller.reconcile.duration"
	unfinished        = "humus.kube.controller.reconcile.unfinished_duration"
	longestUnfinished = "humus.kube.controller.reconcile.longest_unfinished_duration"
)

// durationBuckets are OpenTelemetry's advice for a duration in seconds.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

// controllerMetrics collects tel's metrics now, and returns those in the
// kube scope, by name. It fails t unless every data point of them carries
// humus.kube.controller.name, controllerName, and none carries an object's
// namespace or name.
func controllerMetrics(t *testing.T, tel *humustest.Telemetry) map[string]metricdata.Metrics {
	t.Helper()
	rm, err := tel.Metrics(t.Context())
	require.NoError(t, err)
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "github.com/z5labs/humus/kube" {
			continue
		}
		for _, m := range sm.Metrics {
			for _, set := range attributeSets(t, m) {
				name, ok := set.Value("humus.kube.controller.name")
				require.True(t, ok, "a data point of %s without humus.kube.controller.name: %v", m.Name, set.ToSlice())
				require.Equal(t, controllerName, name.AsString())
				for _, key := range []attribute.Key{"k8s.namespace.name", "humus.kube.object.name"} {
					require.False(t, set.HasValue(key), "a data point of %s with %s", m.Name, key)
				}
			}
			out[m.Name] = m
		}
	}
	return out
}

// attributeSets returns the attributes of each of m's data points.
func attributeSets(t *testing.T, m metricdata.Metrics) []attribute.Set {
	t.Helper()
	var sets []attribute.Set
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	default:
		t.Fatalf("%s is a %T", m.Name, m.Data)
	}
	return sets
}

// sumOf returns the value of the int64 sum m's only data point.
func sumOf(t *testing.T, m metricdata.Metrics) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s is a %T", m.Name, m.Data)
	require.Len(t, sum.DataPoints, 1, m.Name)
	return sum.DataPoints[0].Value
}

// gaugeOf returns the value of the float64 gauge m's only data point.
func gaugeOf(t *testing.T, m metricdata.Metrics) float64 {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[float64])
	require.True(t, ok, "%s is a %T", m.Name, m.Data)
	require.Len(t, g.DataPoints, 1, m.Name)
	return g.DataPoints[0].Value
}

// histogramOf returns the data points of the float64 histogram m.
func histogramOf(t *testing.T, m metricdata.Metrics) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	h, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "%s is a %T", m.Name, m.Data)
	return h.DataPoints
}

// outcomes returns the count of reconcile.duration's data points, and their
// sums, by outcome.
func outcomes(t *testing.T, ms map[string]metricdata.Metrics) (counts map[string]uint64, sums map[string]float64) {
	t.Helper()
	counts, sums = map[string]uint64{}, map[string]float64{}
	m, ok := ms[reconcileDuration]
	if !ok {
		return counts, sums
	}
	for _, dp := range histogramOf(t, m) {
		outcome, ok := dp.Attributes.Value("humus.kube.controller.reconcile.outcome")
		require.True(t, ok, "a data point of %s without an outcome", reconcileDuration)
		require.Equal(t, 2, dp.Attributes.Len(), "the controller's name and the outcome: %v", dp.Attributes.ToSlice())
		counts[outcome.AsString()] += dp.Count
		sums[outcome.AsString()] += dp.Sum
	}
	return counts, sums
}

func TestController_instruments(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var got reconciles
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got)))
		time.Sleep(settle)

		ms := controllerMetrics(t, tel)
		want := []struct {
			name, unit, description string
			data                    metricdata.Aggregation
		}{
			{queueDepth, "{object}", "Objects waiting in the work queue for a worker.", metricdata.Sum[int64]{}},
			{queueAdds, "{object}", "Objects added to the work queue, not counting one already waiting.", metricdata.Sum[int64]{}},
			{queueDuration, "s", "How long an object waited in the work queue for a worker.", metricdata.Histogram[float64]{}},
			{reconcileDuration, "s", "How long each reconcile took, by its outcome.", metricdata.Histogram[float64]{}},
			{unfinished, "s", "How long the reconciles in flight have run, added together.", metricdata.Gauge[float64]{}},
			{longestUnfinished, "s", "How long the longest reconcile in flight has run.", metricdata.Gauge[float64]{}},
		}
		require.Len(t, ms, len(want), "the six instruments, and nothing else")
		for _, w := range want {
			m, ok := ms[w.name]
			require.True(t, ok, "no %s", w.name)
			require.Equal(t, w.unit, m.Unit, w.name)
			require.Equal(t, w.description, m.Description, w.name)
			require.IsType(t, w.data, m.Data, w.name)
		}

		depth := ms[queueDepth].Data.(metricdata.Sum[int64])
		require.False(t, depth.IsMonotonic, "queue.depth is an UpDownCounter")
		adds := ms[queueAdds].Data.(metricdata.Sum[int64])
		require.True(t, adds.IsMonotonic, "queue.adds is a Counter")
		for _, name := range []string{queueDuration, reconcileDuration} {
			for _, dp := range histogramOf(t, ms[name]) {
				require.Equal(t, durationBuckets, dp.Bounds, name)
			}
		}

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestController_measures_the_work_queue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("a", 1), memcachedObject("b", 2), memcachedObject("c", 3))
		var got reconciles
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(_ context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			got.add(m)
			time.Sleep(10 * time.Second)
			return kube.Result[MemcachedStatus]{}, nil
		})
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		start := time.Now()
		sv := serve(t, newController(t, s, fc, testSettings(), rec))

		time.Sleep(time.Second)
		ms := controllerMetrics(t, tel)
		require.Equal(t, 1, got.count(), "one worker, holding the first key")
		require.EqualValues(t, 3, sumOf(t, ms[queueAdds]), "a key for each object")
		require.EqualValues(t, 2, sumOf(t, ms[queueDepth]), "the worker has taken one of the three")

		time.Sleep(settle)
		ms = controllerMetrics(t, tel)
		require.EqualValues(t, 3, sumOf(t, ms[queueAdds]), "nothing was added again")
		require.EqualValues(t, 0, sumOf(t, ms[queueDepth]), "the workers took every key")

		at := got.times()
		require.Len(t, at, 3)
		var waited float64
		for _, a := range at {
			waited += a.Sub(start).Seconds()
		}
		dps := histogramOf(t, ms[queueDuration])
		require.Len(t, dps, 1)
		require.EqualValues(t, 3, dps[0].Count, "one for each key a worker took")
		// Each key waited for the Lease, then 0s, 10s and 20s behind the
		// reconciles before it.
		lease := at[0].Sub(start).Seconds()
		require.Less(t, lease, 1.0)
		require.InDelta(t, 30+3*lease, waited, 1e-9)
		require.InDelta(t, waited, dps[0].Sum, 1e-9, "each key's wait, from its add to a worker taking it")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestController_counts_no_wait_of_After_or_of_a_backoff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("after", 1), memcachedObject("fail", 2))
		var got reconciles
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(_ context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			n := got.add(m)
			if m.Name == "fail" && n <= 2 {
				return kube.Result[MemcachedStatus]{}, errors.New("the Deployment wasn't applied")
			}
			return kube.Result[MemcachedStatus]{After: 30 * time.Second}, nil
		})
		settings := testSettings()
		settings.Workers = 2
		settings.RetryBaseDelay = 20 * time.Second
		settings.RetryMaxDelay = 20 * time.Second
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		start := time.Now()
		sv := serve(t, newController(t, s, fc, settings, rec))
		time.Sleep(45 * time.Second)
		// The first two keys waited for the Lease, and no key after them
		// waited at all.
		lease := got.times()[0].Sub(start).Seconds()
		require.Less(t, lease, 1.0)

		ms := controllerMetrics(t, tel)
		dps := histogramOf(t, ms[queueDuration])
		require.Len(t, dps, 1)
		require.Equal(t, uint64(sumOf(t, ms[queueAdds])), dps[0].Count, "every key added was taken")
		require.Greater(t, dps[0].Count, uint64(2), "keys came back after After and after a backoff")
		require.InDelta(t, 2*lease, dps[0].Sum, 1e-9, "two workers took every key coming back as it was added")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestController_records_each_reconciles_outcome(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		rec     func(context.Context) (kube.Result[MemcachedStatus], error)
		outcome string
	}{
		{name: "a success", outcome: "success", rec: func(context.Context) (kube.Result[MemcachedStatus], error) {
			return kube.Result[MemcachedStatus]{}, nil
		}},
		{name: "an After", outcome: "success", rec: func(context.Context) (kube.Result[MemcachedStatus], error) {
			return kube.Result[MemcachedStatus]{After: time.Hour}, nil
		}},
		{name: "an error", outcome: "error", rec: func(context.Context) (kube.Result[MemcachedStatus], error) {
			return kube.Result[MemcachedStatus]{}, errors.New("the Deployment wasn't applied")
		}},
		{name: "a panic", outcome: "panic", rec: func(context.Context) (kube.Result[MemcachedStatus], error) {
			panic("the Deployment is nil")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				fc := newFakeCluster(t, memcachedObject("cache", 3))
				var got reconciles
				rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
					got.add(m)
					time.Sleep(2 * time.Second)
					return tc.rec(ctx)
				})
				// The error's retry comes after the test has looked.
				settings := testSettings()
				settings.RetryBaseDelay = time.Hour
				settings.RetryMaxDelay = time.Hour
				tel := humustest.NewTelemetry()
				s := newSetup(t, humus.Host{Dial: noDial}, tel)
				sv := serve(t, newController(t, s, fc, settings, rec))
				time.Sleep(settle)
				require.Equal(t, 1, got.count())

				counts, sums := outcomes(t, controllerMetrics(t, tel))
				require.Equal(t, map[string]uint64{tc.outcome: 1}, counts)
				require.Equal(t, map[string]float64{tc.outcome: 2}, sums, "the reconcile's 2s")

				var spans int
				for _, span := range tel.Spans() {
					if span.Name() == "reconcile Memcached" {
						spans++
						require.Equal(t, 2*time.Second, span.EndTime().Sub(span.StartTime()), "the same time as the span")
					}
				}
				require.Equal(t, 1, spans)

				sv.cancel()
				require.NoError(t, sv.wait(t))
			})
		})
	}
}

func TestController_measures_the_reconciles_in_flight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("a", 1), memcachedObject("b", 2))
		inFlight, finish := make(chan struct{}, 2), make(chan struct{})
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(context.Context, kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			inFlight <- struct{}{}
			<-finish
			return kube.Result[MemcachedStatus]{}, nil
		})
		settings := testSettings()
		settings.Workers = 2
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newController(t, s, fc, settings, rec))
		<-inFlight
		<-inFlight

		// Between two of the queue's updates, every 500ms.
		time.Sleep(10*time.Second + 250*time.Millisecond)
		ms := controllerMetrics(t, tel)
		longest := gaugeOf(t, ms[longestUnfinished])
		require.InDelta(t, 10, longest, 0.5, "the longest reconcile in flight, as of the last update")
		require.Equal(t, 2*longest, gaugeOf(t, ms[unfinished]), "both reconciles in flight, which started together")

		close(finish)
		time.Sleep(time.Second)
		ms = controllerMetrics(t, tel)
		require.Zero(t, gaugeOf(t, ms[longestUnfinished]), "no reconcile in flight")
		require.Zero(t, gaugeOf(t, ms[unfinished]), "no reconcile in flight")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestController_standby_records_no_reconcile(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("a", 1), memcachedObject("b", 2))
		var led, stood reconciles
		leader := serve(t, newController(t, newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry()), fc, testSettings(), readyReplicas(&led)))
		time.Sleep(settle)
		require.Equal(t, identity, fc.holder(t))

		settings := testSettings()
		settings.LeaderElection.Identity = "replica-2"
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		standby := serve(t, newController(t, s, fc, settings, readyReplicas(&stood)))
		time.Sleep(settle)

		require.Zero(t, stood.count(), "the standby doesn't hold the Lease")
		ms := controllerMetrics(t, tel)
		counts, _ := outcomes(t, ms)
		require.Empty(t, counts, "no reconcile.duration on a standby")
		require.Zero(t, gaugeOf(t, ms[unfinished]))
		require.Zero(t, gaugeOf(t, ms[longestUnfinished]))
		require.EqualValues(t, 2, sumOf(t, ms[queueDepth]), "its queue holds a key for every object")
		_, took := ms[queueDuration]
		require.False(t, took, "nothing leaves a standby's queue")

		standby.cancel()
		require.NoError(t, standby.wait(t))
		leader.cancel()
		require.NoError(t, leader.wait(t))
	})
}

// failingMeterProvider is a MeterProvider whose meters fail to create a
// Float64Gauge.
type failingMeterProvider struct {
	metricnoop.MeterProvider
}

func (failingMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return failingMeter{}
}

type failingMeter struct {
	metricnoop.Meter
}

func (failingMeter) Float64Gauge(name string, _ ...metric.Float64GaugeOption) (metric.Float64Gauge, error) {
	return nil, errors.New("no gauge for " + name)
}

func TestNewController_reports_an_instrument_it_couldnt_create(t *testing.T) {
	t.Parallel()

	s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
	client := testClient(t, s)
	s.Telemetry.MeterProvider = failingMeterProvider{}
	settings := testSettings()
	settings.Workers = 0

	ctrl, err := kube.NewController(s, client, settings, Memcached, readyReplicas(&reconciles{}))
	require.Nil(t, ctrl)
	require.ErrorContains(t, err, "kube: NewController: ")
	require.ErrorContains(t, err, "Settings.Workers must be at least 1", "among the other errors")
	require.ErrorContains(t, err, "creating its instruments: ")
	require.ErrorContains(t, err, "no gauge for humus.kube.controller.reconcile.unfinished_duration")
	require.ErrorContains(t, err, "no gauge for humus.kube.controller.reconcile.longest_unfinished_duration")
}
