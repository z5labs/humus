// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the invoicer's black-box cases, which the Watermill service the Watermill migration guide starts from and the humus service it ends with both pass.
package cases

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

const poll = 100 * time.Millisecond

type Target struct {
	Cluster *kfake.Cluster
	Kafka   []kgo.Opt

	Group    string
	Consumes []string

	OrdersTopic     string
	InvoicesTopic   string
	DeadLetterTopic string

	Timeout time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "AnOrder", Run: AnOrder},
	{Name: "NotOrders", Run: NotOrders},
	{Name: "ARefusedInvoice", Run: ARefusedInvoice},
}

func AnOrder(t testing.TB, target Target) {
	ctx, r := begin(t, target)

	r.produce(ctx, t, order("order-1", "corr-1", `{"id":"order-1","customer":"ada","total":4250}`))
	got := r.settle(ctx, t)

	require.Len(t, got.invoices, 1)
	requireInvoice(t, got.invoices[0], "corr-1", `{"order_id":"order-1","customer":"ada","amount":4250}`)
	require.Empty(t, got.deadLetters)
}

func NotOrders(t testing.TB, target Target) {
	ctx, r := begin(t, target)

	r.produce(ctx, t,
		order("order-2", "corr-2", "not JSON"),
		order("order-3", "corr-3", `{"customer":"ada","total":100}`),
		order("order-4", "corr-4", `{"id":"order-4","customer":"grace","total":990}`),
	)
	got := r.settle(ctx, t)

	require.Len(t, got.deadLetters, 2)
	require.Equal(t, "not JSON", string(got.deadLetters[0].Value))
	require.Equal(t, "corr-2", header(got.deadLetters[0], "correlation_id"))
	require.Equal(t, `{"customer":"ada","total":100}`, string(got.deadLetters[1].Value))
	require.Equal(t, "corr-3", header(got.deadLetters[1], "correlation_id"))
	require.Len(t, got.invoices, 1)
	requireInvoice(t, got.invoices[0], "corr-4", `{"order_id":"order-4","customer":"grace","amount":990}`)
}

func ARefusedInvoice(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	refused := target.Cluster.Fault(kfake.Fault{
		Keys:  []kmsg.Key{kmsg.Produce},
		Topic: target.InvoicesTopic,
		Err:   kerr.TopicAuthorizationFailed,
		Count: 2,
	})

	r.produce(ctx, t, order("order-5", "corr-5", `{"id":"order-5","customer":"ada","total":1200}`))
	got := r.settle(ctx, t)

	require.Equal(t, 2, refused.Hits())
	require.Len(t, got.invoices, 1)
	requireInvoice(t, got.invoices[0], "corr-5", `{"order_id":"order-5","customer":"ada","amount":1200}`)
	require.Empty(t, got.deadLetters)
}

func order(id, correlationID, value string) *kgo.Record {
	return &kgo.Record{
		Key:     []byte(id),
		Value:   []byte(value),
		Headers: []kgo.RecordHeader{{Key: "correlation_id", Value: []byte(correlationID)}},
	}
}

func requireInvoice(t testing.TB, rec *kgo.Record, correlationID, want string) {
	t.Helper()
	require.JSONEq(t, want, string(rec.Value))
	require.Equal(t, correlationID, header(rec, "correlation_id"))
}

func header(rec *kgo.Record, key string) string {
	for _, h := range rec.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

type run struct {
	target Target
	client *kgo.Client
	admin  *kadm.Client
}

func begin(t testing.TB, target Target) (context.Context, *run) {
	t.Helper()
	require.NotNil(t, target.Cluster, "Target.Cluster")
	require.NotEmpty(t, target.Kafka, "Target.Kafka")
	require.NotEmpty(t, target.Group, "Target.Group")
	require.NotEmpty(t, target.Consumes, "Target.Consumes")
	require.NotEmpty(t, target.OrdersTopic, "Target.OrdersTopic")
	require.NotEmpty(t, target.InvoicesTopic, "Target.InvoicesTopic")
	require.NotEmpty(t, target.DeadLetterTopic, "Target.DeadLetterTopic")
	require.Positive(t, target.Timeout, "Target.Timeout")

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	t.Cleanup(cancel)

	client, err := kgo.NewClient(target.Kafka...)
	require.NoError(t, err)
	t.Cleanup(client.Close)
	return ctx, &run{target: target, client: client, admin: kadm.NewClient(client)}
}

func (r *run) produce(ctx context.Context, t testing.TB, records ...*kgo.Record) {
	t.Helper()
	for _, rec := range records {
		rec.Topic = r.target.OrdersTopic
	}
	require.NoError(t, r.client.ProduceSync(ctx, records...).FirstErr(), "producing to %s", r.target.OrdersTopic)
}

type results struct {
	invoices    []*kgo.Record
	deadLetters []*kgo.Record
}

func (r *run) settle(ctx context.Context, t testing.TB) results {
	t.Helper()
	for {
		pending, err := r.pending(ctx)
		if err == nil && pending == 0 {
			return results{
				invoices:    r.read(ctx, t, r.target.InvoicesTopic),
				deadLetters: r.read(ctx, t, r.target.DeadLetterTopic),
			}
		}
		what := fmt.Sprintf("%s to commit %d records of %v", r.target.Group, pending, r.target.Consumes)
		if err != nil {
			what = fmt.Sprintf("%s's committed offsets: %v", r.target.Group, err)
		}
		select {
		case <-time.After(poll):
		case <-ctx.Done():
			require.FailNowf(t, "timed out", "waiting for %s: %v", what, context.Cause(ctx))
		}
	}
}

func (r *run) pending(ctx context.Context) (int64, error) {
	committed, err := r.admin.FetchOffsets(ctx, r.target.Group)
	if err == nil {
		err = committed.Error()
	}
	if err != nil {
		return 0, err
	}
	ends, err := r.admin.ListEndOffsets(ctx, r.target.Consumes...)
	if err == nil {
		err = ends.Error()
	}
	if err != nil {
		return 0, err
	}
	var pending int64
	ends.Each(func(end kadm.ListedOffset) {
		at := int64(0)
		if o, ok := committed.Lookup(end.Topic, end.Partition); ok {
			at = o.At
		}
		pending += max(end.Offset-at, 0)
	})
	return pending, nil
}

func (r *run) read(ctx context.Context, t testing.TB, topic string) []*kgo.Record {
	t.Helper()
	starts, err := r.admin.ListStartOffsets(ctx, topic)
	if err == nil {
		err = starts.Error()
	}
	require.NoError(t, err, "listing %s's start offsets", topic)
	ends, err := r.admin.ListEndOffsets(ctx, topic)
	if err == nil {
		err = ends.Error()
	}
	require.NoError(t, err, "listing %s's end offsets", topic)

	from := map[int32]kgo.Offset{}
	last := map[int32]int64{}
	ends.Each(func(end kadm.ListedOffset) {
		start, ok := starts.Lookup(topic, end.Partition)
		if !ok || start.Offset >= end.Offset {
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
			out = append(out, rec)
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
