// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/z5labs/humus"
)

// Record is a Kafka record to produce.
type Record struct {
	// Topic is the topic the record is produced to. It is required.
	Topic string

	// Key is the record's key, nil when it has none. Records with the same
	// key go to the same partition of Topic, hashed with murmur2 as Kafka's
	// Java producer hashes them.
	Key []byte

	// Value is the record's value, nil when it has none, which a compacted
	// topic reads as a tombstone.
	Value []byte

	// Headers are the record's headers, in order. A key may repeat.
	Headers []Header
}

// Producer produces records to one Kafka cluster. It is safe for concurrent
// use: one Producer serves every Service of the process.
type Producer struct {
	client          *kgo.Client
	deliveryTimeout time.Duration

	// closed is set once the OnStop function has flushed the client and is
	// about to close it.
	closed atomic.Bool
}

// NewProducer runs every check on settings, reporting every bad field in
// one error that names each one, and builds the franz-go client. It opens
// no connection: the first Produce does. It registers a function with
// s.OnStop that flushes the client and closes it.
//
// The Producer dials every broker through s.Host, and reports franz-go's
// logs through s.Telemetry, under the scope
// github.com/twmb/franz-go/pkg/kgo.
func NewProducer(s *humus.Setup, settings ProducerSettings) (*Producer, error) {
	if err := checkProducerSettings(settings); err != nil {
		return nil, fmt.Errorf("kafka: NewProducer: %w", err)
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(settings.Brokers...),
		kgo.Dialer(tlsDialer(s.Host.Dial, settings.TLS)),
		kgo.WithLogger(kgoLogger{ctx: context.Background(), logger: s.Telemetry.LoggerProvider.Logger(kgoScope)}),
		kgo.AllowIdempotentProduceCancellation(),
	}
	if settings.SASL != nil {
		opts = append(opts, kgo.SASL(settings.SASL))
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka: NewProducer: building the client: %w", err)
	}
	p := &Producer{client: cl, deliveryTimeout: settings.DeliveryTimeout}
	s.OnStop(p.stop)
	return p, nil
}

// checkProducerSettings runs every check on settings. It reports every
// failure in one error, each naming its field.
func checkProducerSettings(settings ProducerSettings) error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
		}
	}
	check("ProducerSettings.Brokers", CheckBrokers(settings.Brokers))
	check("ProducerSettings.DeliveryTimeout", CheckDeliveryTimeout(settings.DeliveryTimeout))
	return errors.Join(errs...)
}

// stop flushes the client with ctx, which the lifecycle bounds by the grace
// period, and then closes it. When ctx is done first, Close fails the
// records still in the client with kgo.ErrClientClosed, and stop returns an
// error that wraps ctx's error.
func (p *Producer) stop(ctx context.Context) error {
	err := p.client.Flush(ctx)
	p.closed.Store(true)
	p.client.Close()
	if err != nil {
		return fmt.Errorf("kafka: flushing the Producer: %w", err)
	}
	return nil
}

// Produce produces records, and returns nil once the cluster has
// acknowledged every one. Records to one partition are stored in the order
// they are given.
//
// It returns an error, naming each record that failed by its topic, when a
// record failed or ProducerSettings.DeliveryTimeout passed, and an error
// that wraps ctx's error, at once, when ctx is done first. A call whose ctx
// is already done sends nothing. Records handed to the client before ctx is
// done go on to their result, within DeliveryTimeout, and never fail another
// call's records. A record that failed may still have been stored, so a
// caller that produces it again may store it twice.
//
// Produce rejects a record with no Topic before it sends anything. With no
// records, it returns nil.
func (p *Producer) Produce(ctx context.Context, records ...Record) error {
	if len(records) == 0 {
		return nil
	}
	for i, r := range records {
		if r.Topic == "" {
			return fmt.Errorf("kafka: producing: record %d has no Topic", i)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("kafka: producing: %w", err)
	}
	if p.closed.Load() {
		return fmt.Errorf("kafka: producing: %w", kgo.ErrClientClosed)
	}

	krs := make([]*kgo.Record, len(records))
	for i, r := range records {
		krs[i] = newKgoRecord(r)
	}

	// The call's context keeps ctx's values but not its cancellation: once
	// handed to franz-go, the records are failed by DeliveryTimeout alone,
	// never by a caller giving up, which would fail every record buffered
	// behind them on their partition, whoever produced them.
	call, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.deliveryTimeout)
	results := make(chan kgo.ProduceResults, 1)
	go func() {
		defer cancel()
		results <- p.client.ProduceSync(call, krs...)
	}()

	select {
	case rs := <-results:
		return produceError(krs, rs)
	case <-ctx.Done():
		return fmt.Errorf("kafka: producing: %w", ctx.Err())
	}
}

// newKgoRecord returns the franz-go record of r. Its partition is -1 until
// franz-go chooses one.
func newKgoRecord(r Record) *kgo.Record {
	var headers []kgo.RecordHeader
	if len(r.Headers) > 0 {
		headers = make([]kgo.RecordHeader, len(r.Headers))
		for i, h := range r.Headers {
			headers[i] = kgo.RecordHeader{Key: h.Key, Value: h.Value}
		}
	}
	return &kgo.Record{
		Topic:     r.Topic,
		Partition: -1,
		Key:       r.Key,
		Value:     r.Value,
		Headers:   headers,
	}
}

// produceError joins the error of each record of rs that failed, in the
// order of krs, with its topic, and its partition when one was chosen. It
// returns nil when every record was acknowledged.
func produceError(krs []*kgo.Record, rs kgo.ProduceResults) error {
	failed := make(map[*kgo.Record]error, len(rs))
	for _, r := range rs {
		if r.Err != nil {
			failed[r.Record] = r.Err
		}
	}
	if len(failed) == 0 {
		return nil
	}
	var errs []error
	for _, r := range krs {
		err, ok := failed[r]
		if !ok {
			continue
		}
		if r.Partition < 0 {
			errs = append(errs, fmt.Errorf("kafka: producing to %s: %w", r.Topic, err))
			continue
		}
		errs = append(errs, fmt.Errorf("kafka: producing to %s, partition %d: %w", r.Topic, r.Partition, err))
	}
	return errors.Join(errs...)
}
