// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the order status processor's black-box cases, run against a deployment of it.
package cases

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/fault/jsonscenario"
)

const (
	placed    = "placed"
	paid      = "paid"
	shipped   = "shipped"
	delivered = "delivered"
	cancelled = "cancelled"

	malformed         = "malformed"
	unknownType       = "unknown-type"
	invalidTransition = "invalid-transition"
	unavailable       = "unavailable"
)

const (
	storePoint = "postgresql:orders"
	slowBy     = time.Second
)

const (
	poll      = 100 * time.Millisecond
	lookback  = time.Minute
	rolloutBy = 200 * time.Millisecond
	orders    = 5
)

type Target struct {
	Kafka []kgo.Opt

	EventsTopic     string
	StatusTopic     string
	RetryTopic      string
	DeadLetterTopic string
	Group           string

	Scenario    jsonscenario.Codec
	ScenarioKey string

	RetryAttempts int
	RetryMinDelay time.Duration
	RetryMaxDelay time.Duration

	Timeout time.Duration
	Rollout func(ctx context.Context) error
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "Projection", Run: Projection},
	{Name: "Redelivery", Run: Redelivery},
	{Name: "DeadLetters", Run: DeadLetters},
	{Name: "OutOfOrder", Run: OutOfOrder},
	{Name: "ALateEvent", Run: ALateEvent},
	{Name: "Rollout", Run: Rollout},
	{Name: "SlowPostgreSQL", Run: SlowPostgreSQL},
	{Name: "SlowStatusTopic", Run: SlowStatusTopic},
	{Name: "PostgreSQLDown", Run: PostgreSQLDown},
}

func Projection(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	order := newOrderID()
	at := r.at(0)

	r.produce(ctx, t, event(order, placed, at), event(order, paid, r.at(1)), event(order, shipped, r.at(2)))
	got := r.settle(ctx, t)

	require.Equal(t, []change{
		{OrderID: order, Status: placed, At: at},
		{OrderID: order, Status: paid, Previous: placed, At: r.at(1)},
		{OrderID: order, Status: shipped, Previous: paid, At: r.at(2)},
	}, got.statuses(t, order))
	require.Empty(t, got.deadLetters(order))
}

func Redelivery(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	order := newOrderID()
	at := r.at(0)

	first := r.produce(ctx, t, event(order, placed, at))
	again := r.produce(ctx, t, event(order, placed, at))
	require.NotEqual(t, first[0].Offset, again[0].Offset)
	got := r.settle(ctx, t)

	require.Equal(t, []change{{OrderID: order, Status: placed, At: at}}, got.statuses(t, order))
	require.Empty(t, got.deadLetters(order))
	require.Empty(t, got.retried(order))
}

func DeadLetters(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	notJSON, unknown, refused, next := newOrderID(), newOrderID(), newOrderID(), newOrderID()

	history := r.produce(ctx, t, event(refused, placed, r.at(0)), event(refused, cancelled, r.at(1)), event(refused, delivered, r.at(2)))
	late := history[2]
	bad := &kgo.Record{Key: []byte(notJSON), Value: []byte("not JSON")}
	odd := event(unknown, "returned", r.at(0))
	following := event(next, placed, r.at(0))
	sent := r.produceTo(ctx, t, late.Partition, bad, odd, following)
	got := r.settle(ctx, t)

	requireDeadLetter(t, got.deadLetters(notJSON), sent[0], malformed, 0)
	requireDeadLetter(t, got.deadLetters(unknown), sent[1], unknownType, 0)
	require.Empty(t, got.retried(notJSON))
	require.Empty(t, got.retried(unknown))

	require.Equal(t, []change{
		{OrderID: refused, Status: placed, At: r.at(0)},
		{OrderID: refused, Status: cancelled, Previous: placed, At: r.at(1)},
	}, got.statuses(t, refused))
	requireDeadLetter(t, got.deadLetters(refused), late, invalidTransition, target.RetryAttempts)
	retried := got.retried(refused)
	require.Len(t, retried, target.RetryAttempts-1)
	for i, rec := range retried {
		require.Equal(t, strconv.Itoa(i+1), header(rec, "retry.attempt"))
		require.Equal(t, invalidTransition, header(rec, "retry.reason"))
		require.Equal(t, strconv.FormatInt(late.Offset, 10), header(rec, "retry.offset"))
	}

	require.Equal(t, []change{{OrderID: next, Status: placed, At: r.at(0)}}, got.statuses(t, next))
	require.Empty(t, got.deadLetters(next))
}

func OutOfOrder(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	order := newOrderID()

	r.produce(ctx, t, event(order, placed, r.at(0)), event(order, shipped, r.at(2)), event(order, paid, r.at(1)))
	got := r.settle(ctx, t)

	require.Equal(t, []change{
		{OrderID: order, Status: placed, At: r.at(0)},
		{OrderID: order, Status: paid, Previous: placed, At: r.at(1)},
		{OrderID: order, Status: shipped, Previous: paid, At: r.at(2)},
	}, got.statuses(t, order))
	require.Empty(t, got.deadLetters(order))
}

func ALateEvent(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	order := newOrderID()

	r.produce(ctx, t, event(order, placed, r.at(0)), event(order, cancelled, r.at(2)), event(order, paid, r.at(1)))
	got := r.settle(ctx, t)

	require.Equal(t, []change{
		{OrderID: order, Status: placed, At: r.at(0)},
		{OrderID: order, Status: cancelled, Previous: placed, At: r.at(2)},
		{OrderID: order, Status: paid, Previous: placed, At: r.at(1)},
		{OrderID: order, Status: cancelled, Previous: paid, At: r.at(2)},
	}, got.statuses(t, order))
	require.Empty(t, got.deadLetters(order))
}

func Rollout(t testing.TB, target Target) {
	if target.Rollout == nil {
		t.Skip("Target.Rollout is nil")
	}
	ctx, r := begin(t, target)

	var placedOrders []string
	place := func() *kgo.Record {
		order := newOrderID()
		placedOrders = append(placedOrders, order)
		return event(order, placed, r.at(0))
	}
	for range orders {
		r.produce(ctx, t, place())
	}

	type produced struct {
		records []*kgo.Record
		err     error
	}
	stop := make(chan struct{})
	during := make(chan produced, 1)
	go func() {
		var out produced
		defer func() { during <- out }()
		for {
			sent, err := r.send(ctx, r.client, event(newOrderID(), placed, r.at(0)))
			if err != nil {
				out.err = err
				return
			}
			out.records = append(out.records, sent...)
			select {
			case <-stop:
				return
			case <-ctx.Done():
				out.err = context.Cause(ctx)
				return
			case <-time.After(rolloutBy):
			}
		}
	}()
	err := target.Rollout(ctx)
	close(stop)
	got := <-during
	require.NoError(t, err, "rolling out")
	require.NoError(t, got.err, "producing during the rollout")
	require.NotEmpty(t, got.records)
	r.keep(got.records)
	for _, rec := range got.records {
		placedOrders = append(placedOrders, string(rec.Key))
	}

	for range orders {
		r.produce(ctx, t, place())
	}
	results := r.settle(ctx, t)

	for _, order := range placedOrders {
		require.Equal(t, []change{{OrderID: order, Status: placed, At: r.at(0)}}, results.statuses(t, order), "order %s", order)
		require.Empty(t, results.deadLetters(order), "order %s", order)
	}
}

func SlowPostgreSQL(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	order := newOrderID()
	at := r.at(0)

	r.produce(ctx, t, r.rehearse(t, event(order, placed, at), delay(storePoint, slowBy)))
	got := r.settle(ctx, t)

	require.Equal(t, []change{{OrderID: order, Status: placed, At: at}}, got.statuses(t, order))
	require.Empty(t, got.deadLetters(order))
	for _, rec := range of(got.status, order) {
		require.False(t, r.carriesScenario(t, rec), "the status event carries the scenario")
	}
}

func SlowStatusTopic(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	order := newOrderID()
	at := r.at(0)

	r.produce(ctx, t, r.rehearse(t, event(order, placed, at), delay("kafka:"+target.StatusTopic, slowBy)))
	got := r.settle(ctx, t)

	require.Equal(t, []change{{OrderID: order, Status: placed, At: at}}, got.statuses(t, order))
	require.Empty(t, got.deadLetters(order))
}

func PostgreSQLDown(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	down, next := newOrderID(), newOrderID()

	failing := r.produce(ctx, t, r.rehearse(t, event(down, placed, r.at(0)), abort(storePoint)))[0]
	following := r.produceTo(ctx, t, failing.Partition, event(next, placed, r.at(0)))[0]
	require.Greater(t, following.Offset, failing.Offset)
	got := r.settle(ctx, t)

	require.Empty(t, got.statuses(t, down))
	dead := got.deadLetters(down)
	requireDeadLetter(t, dead, failing, unavailable, target.RetryAttempts)
	require.False(t, r.carriesScenario(t, dead[0]), "the dead letter carries the scenario")
	retried := got.retried(down)
	require.Len(t, retried, target.RetryAttempts-1)
	for i, rec := range retried {
		require.Equal(t, strconv.Itoa(i+1), header(rec, "retry.attempt"))
		require.Equal(t, unavailable, header(rec, "retry.reason"))
		require.True(t, r.carriesScenario(t, rec), "retried copy %d has no scenario", i+1)
	}

	require.Equal(t, []change{{OrderID: next, Status: placed, At: r.at(0)}}, got.statuses(t, next))
	require.Empty(t, got.deadLetters(next))
}

func requireDeadLetter(t testing.TB, dead []*kgo.Record, source *kgo.Record, reason string, attempts int) {
	t.Helper()
	require.Len(t, dead, 1, "dead letters of %s", source.Key)
	d := dead[0]
	require.Equal(t, source.Value, d.Value)
	require.Equal(t, reason, header(d, "dlq.reason"))
	require.NotEmpty(t, header(d, "dlq.error"))
	require.Equal(t, source.Topic, header(d, "dlq.topic"))
	require.Equal(t, strconv.FormatInt(int64(source.Partition), 10), header(d, "dlq.partition"))
	require.Equal(t, strconv.FormatInt(source.Offset, 10), header(d, "dlq.offset"))
	if attempts == 0 {
		require.False(t, hasHeader(d, "dlq.attempts"), "the dead letter has dlq.attempts")
		return
	}
	require.Equal(t, strconv.Itoa(attempts), header(d, "dlq.attempts"))
}

func header(rec *kgo.Record, key string) string {
	for _, h := range slices.Backward(rec.Headers) {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func hasHeader(rec *kgo.Record, key string) bool {
	return slices.ContainsFunc(rec.Headers, func(h kgo.RecordHeader) bool { return h.Key == key })
}

func abort(point string) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Abort: true}}
}

func delay(point string, d time.Duration) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Delay: d}}
}

func (r *run) rehearse(t testing.TB, rec *kgo.Record, rules ...fit.Rule) *kgo.Record {
	t.Helper()
	require.NotEmpty(t, r.target.ScenarioKey, "Target.ScenarioKey")
	b, err := r.target.Scenario.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err, "Target.Scenario")
	require.NotEmpty(t, b.Member(r.target.ScenarioKey).Key(), "Target.Scenario does not write Target.ScenarioKey")
	rec.Headers = append(rec.Headers, kgo.RecordHeader{Key: "baggage", Value: []byte(b.String())})
	return rec
}

func (r *run) carriesScenario(t testing.TB, rec *kgo.Record) bool {
	t.Helper()
	var members []string
	for _, h := range rec.Headers {
		if h.Key == "baggage" {
			members = append(members, string(h.Value))
		}
	}
	b, err := baggage.Parse(strings.Join(members, ","))
	require.NoError(t, err, "the baggage of %s", rec.Key)
	return b.Member(r.target.ScenarioKey).Key() != ""
}

func newOrderID() string {
	return "order-" + rand.Text()
}

func event(orderID, typ string, at time.Time) *kgo.Record {
	value, err := json.Marshal(map[string]any{"type": typ, "at": at})
	if err != nil {
		panic(err)
	}
	return &kgo.Record{Key: []byte(orderID), Value: value}
}

type change struct {
	OrderID  string    `json:"order_id"`
	Status   string    `json:"status"`
	Previous string    `json:"previous"`
	At       time.Time `json:"at"`
}

type run struct {
	target   Target
	began    time.Time
	client   *kgo.Client
	pinned   *kgo.Client
	admin    *kadm.Client
	orders   map[string]bool
	produced []*kgo.Record
}

func begin(t testing.TB, target Target) (context.Context, *run) {
	t.Helper()
	require.NotEmpty(t, target.Kafka, "Target.Kafka")
	for name, topic := range map[string]string{
		"Target.EventsTopic":     target.EventsTopic,
		"Target.StatusTopic":     target.StatusTopic,
		"Target.RetryTopic":      target.RetryTopic,
		"Target.DeadLetterTopic": target.DeadLetterTopic,
		"Target.Group":           target.Group,
	} {
		require.NotEmpty(t, topic, name)
	}
	require.Positive(t, target.RetryAttempts, "Target.RetryAttempts")
	require.Positive(t, target.RetryMinDelay, "Target.RetryMinDelay")
	require.GreaterOrEqual(t, target.RetryMaxDelay, target.RetryMinDelay, "Target.RetryMaxDelay")
	require.Positive(t, target.Timeout, "Target.Timeout")

	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout+attemptsTake(target))
	t.Cleanup(cancel)

	client := newClient(t, target)
	return ctx, &run{
		target: target,
		began:  time.Now().UTC().Truncate(time.Millisecond),
		client: client,
		pinned: newClient(t, target, kgo.RecordPartitioner(kgo.ManualPartitioner())),
		admin:  kadm.NewClient(client),
		orders: map[string]bool{},
	}
}

func attemptsTake(target Target) time.Duration {
	var total time.Duration
	d := target.RetryMinDelay
	for range target.RetryAttempts - 1 {
		total += d
		d = min(2*d, target.RetryMaxDelay)
	}
	return total
}

func newClient(t testing.TB, target Target, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	cl, err := kgo.NewClient(append(slices.Clone(target.Kafka), opts...)...)
	require.NoError(t, err)
	t.Cleanup(cl.Close)
	return cl
}

func (r *run) at(minutes int) time.Time {
	return r.began.Add(time.Duration(minutes) * time.Minute)
}

func (r *run) own(orderID string) {
	r.orders[orderID] = true
}

func (r *run) produce(ctx context.Context, t testing.TB, records ...*kgo.Record) []*kgo.Record {
	t.Helper()
	sent, err := r.send(ctx, r.client, records...)
	require.NoError(t, err)
	r.keep(sent)
	return sent
}

func (r *run) produceTo(ctx context.Context, t testing.TB, partition int32, records ...*kgo.Record) []*kgo.Record {
	t.Helper()
	for _, rec := range records {
		rec.Partition = partition
	}
	sent, err := r.send(ctx, r.pinned, records...)
	require.NoError(t, err)
	r.keep(sent)
	return sent
}

func (r *run) send(ctx context.Context, cl *kgo.Client, records ...*kgo.Record) ([]*kgo.Record, error) {
	for _, rec := range records {
		rec.Topic = r.target.EventsTopic
	}
	results := cl.ProduceSync(ctx, records...)
	if err := results.FirstErr(); err != nil {
		return nil, fmt.Errorf("producing to %s: %w", r.target.EventsTopic, err)
	}
	return records, nil
}

func (r *run) keep(sent []*kgo.Record) {
	for _, rec := range sent {
		r.own(string(rec.Key))
		r.produced = append(r.produced, rec)
	}
}

func (r *run) settle(ctx context.Context, t testing.TB) results {
	t.Helper()
	for {
		committed, err := r.admin.FetchOffsets(ctx, r.target.Group)
		if err == nil {
			err = committed.Error()
		}
		require.NoError(t, err, "fetching %s's committed offsets", r.target.Group)

		retried := r.read(ctx, t, r.target.RetryTopic)
		pending := r.uncommitted(committed, r.produced) + r.uncommitted(committed, retried)
		if pending == 0 {
			return results{
				retry:  retried,
				status: r.read(ctx, t, r.target.StatusTopic),
				dead:   r.read(ctx, t, r.target.DeadLetterTopic),
			}
		}
		pause(ctx, t, fmt.Sprintf("%s to commit %d of this case's records", r.target.Group, pending))
	}
}

func (r *run) uncommitted(committed kadm.OffsetResponses, records []*kgo.Record) int {
	n := 0
	for _, rec := range records {
		o, ok := committed.Lookup(rec.Topic, rec.Partition)
		if !ok || o.At <= rec.Offset {
			n++
		}
	}
	return n
}

func (r *run) read(ctx context.Context, t testing.TB, topic string) []*kgo.Record {
	t.Helper()
	starts, err := r.admin.ListOffsetsAfterMilli(ctx, r.began.Add(-lookback).UnixMilli(), topic)
	if err == nil {
		err = starts.Error()
	}
	require.NoError(t, err, "listing %s's offsets from %s", topic, r.began.Add(-lookback))
	ends, err := r.admin.ListEndOffsets(ctx, topic)
	if err == nil {
		err = ends.Error()
	}
	require.NoError(t, err, "listing %s's end offsets", topic)

	from := map[int32]kgo.Offset{}
	last := map[int32]int64{}
	ends.Each(func(end kadm.ListedOffset) {
		start, ok := starts.Lookup(topic, end.Partition)
		if !ok || start.Offset < 0 || start.Offset >= end.Offset {
			return
		}
		from[end.Partition] = kgo.NewOffset().At(start.Offset)
		last[end.Partition] = end.Offset - 1
	})
	if len(from) == 0 {
		return nil
	}

	cl, err := kgo.NewClient(append(slices.Clone(r.target.Kafka), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: from}))...)
	require.NoError(t, err)
	defer cl.Close()

	var out []*kgo.Record
	for len(last) > 0 {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			require.FailNowf(t, "timed out", "reading %s up to its end offsets: %v", topic, context.Cause(ctx))
		}
		fetches.EachError(func(_ string, p int32, err error) {
			require.NoError(t, err, "reading %s, partition %d", topic, p)
		})
		fetches.EachRecord(func(rec *kgo.Record) {
			end, ok := last[rec.Partition]
			if !ok || rec.Offset > end {
				return
			}
			if r.orders[string(rec.Key)] {
				out = append(out, rec)
			}
			if rec.Offset == end {
				delete(last, rec.Partition)
			}
		})
	}
	slices.SortStableFunc(out, func(a, b *kgo.Record) int {
		return cmp.Or(cmp.Compare(a.Partition, b.Partition), cmp.Compare(a.Offset, b.Offset))
	})
	return out
}

func pause(ctx context.Context, t testing.TB, what string) {
	t.Helper()
	select {
	case <-time.After(poll):
	case <-ctx.Done():
		require.FailNowf(t, "timed out", "waiting for %s: %v", what, context.Cause(ctx))
	}
}

type results struct {
	retry  []*kgo.Record
	status []*kgo.Record
	dead   []*kgo.Record
}

func (res results) statuses(t testing.TB, orderID string) []change {
	t.Helper()
	var out []change
	for _, rec := range of(res.status, orderID) {
		var c change
		require.NoError(t, json.Unmarshal(rec.Value, &c), "the status event %s", rec.Value)
		out = append(out, c)
	}
	return out
}

func (res results) deadLetters(orderID string) []*kgo.Record {
	return of(res.dead, orderID)
}

func (res results) retried(orderID string) []*kgo.Record {
	return of(res.retry, orderID)
}

func of(records []*kgo.Record, orderID string) []*kgo.Record {
	var out []*kgo.Record
	for _, rec := range records {
		if string(rec.Key) == orderID {
			out = append(out, rec)
		}
	}
	return out
}
