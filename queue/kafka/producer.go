// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/semconv/v1.43.0/messagingconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
)

// sendOperation is the name of the operation the Producer reports, as
// messaging.operation.name.
const sendOperation = "send"

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

	// Headers are the record's headers, in order. A key may repeat. Any
	// header the Setup's propagator writes, such as traceparent or baggage,
	// is replaced by the record's own trace context.
	Headers []Header
}

// Producer produces records to one Kafka cluster. It is safe for concurrent
// use: one Producer serves every Service of the process.
type Producer struct {
	client          *kgo.Client
	deliveryTimeout time.Duration
	tracer          trace.Tracer
	propagator      propagation.TextMapPropagator

	// closed is set once the OnStop function has flushed the client and is
	// about to close it.
	closed atomic.Bool
}

// NewProducer runs every check on settings, reporting every bad field in
// one error that names each one, and builds the franz-go client. It opens
// no connection: the first Produce does. It registers a function with
// s.OnStop that flushes the client and closes it.
//
// The Producer dials every broker through s.Host. It reports through
// s.Telemetry: its spans and the two messaging instruments under the scope
// github.com/z5labs/humus/queue/kafka, franz-go's logs under
// github.com/twmb/franz-go/pkg/kgo, and franz-go's broker metrics, from
// kotel's Meter, under github.com/twmb/franz-go/plugin/kotel.
func NewProducer(s *humus.Setup, settings ProducerSettings) (*Producer, error) {
	if err := checkProducerSettings(settings); err != nil {
		return nil, fmt.Errorf("kafka: NewProducer: %w", err)
	}
	meter := s.Telemetry.MeterProvider.Meter(scopeName)
	sent, err1 := messagingconv.NewClientSentMessages(meter)
	operations, err2 := messagingconv.NewClientOperationDuration(meter)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("kafka: NewProducer: creating the instruments: %w", err)
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(settings.Brokers...),
		kgo.Dialer(tlsDialer(s.Host.Dial, settings.TLS)),
		kgo.WithLogger(kgoLogger{ctx: context.Background(), logger: s.Telemetry.LoggerProvider.Logger(kgoScope)}),
		kgo.WithHooks(
			sendHook{sent: sent, operations: operations},
			// kotel's Meter creates its instruments and starts nothing.
			kotel.NewMeter(kotel.MeterProvider(s.Telemetry.MeterProvider)),
		),
		kgo.AllowIdempotentProduceCancellation(),
	}
	if settings.SASL != nil {
		opts = append(opts, kgo.SASL(settings.SASL))
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka: NewProducer: building the client: %w", err)
	}
	p := &Producer{
		client:          cl,
		deliveryTimeout: settings.DeliveryTimeout,
		tracer:          s.Telemetry.TracerProvider.Tracer(scopeName),
		propagator:      s.Telemetry.Propagator,
	}
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
//
// Each record it hands to the client is sent under a span of its own, a
// child of the span in ctx, whose context replaces the propagator's headers
// of the record. The span ends, and the record is measured, when its result
// arrives, which may be after Produce returns.
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

	// The call's context keeps ctx's values but not its cancellation: once
	// handed to franz-go, the records are failed by DeliveryTimeout alone,
	// never by a caller giving up, which would fail every record buffered
	// behind them on their partition, whoever produced them.
	start := time.Now()
	call, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.deliveryTimeout)
	fields := p.propagator.Fields()
	krs := make([]*kgo.Record, len(records))
	for i, r := range records {
		krs[i] = p.send(ctx, call, start, fields, r)
	}

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

// sending is what the Producer's hook needs of a record handed to the
// client: the record's span, and when Produce took it.
type sending struct {
	span  trace.Span
	start time.Time
}

// sendingKey is the key of a record's sending in its context.
type sendingKey struct{}

// send starts r's span, a child of the span in ctx, and returns the
// franz-go record of r, whose partition is -1 until franz-go chooses one.
//
// The record's headers are r's, less every header whose key is one of
// fields, the propagator's, and then the span's context and ctx's baggage,
// which the propagator injects. The record's context is call, carrying the
// span and its sending, which the hook ends and measures.
func (p *Producer) send(ctx, call context.Context, start time.Time, fields []string, r Record) *kgo.Record {
	ctx, span := p.tracer.Start(ctx, sendOperation+" "+r.Topic,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithTimestamp(start),
		trace.WithAttributes(
			semconv.MessagingSystemKafka,
			semconv.MessagingOperationTypeSend,
			semconv.MessagingOperationName(sendOperation),
			semconv.MessagingDestinationName(r.Topic),
		),
	)

	headers := make([]kgo.RecordHeader, 0, len(r.Headers)+len(fields))
	for _, h := range r.Headers {
		if !slices.Contains(fields, h.Key) {
			headers = append(headers, kgo.RecordHeader{Key: h.Key, Value: h.Value})
		}
	}
	p.propagator.Inject(ctx, (*headerCarrier)(&headers))

	return &kgo.Record{
		Topic:     r.Topic,
		Partition: -1,
		Key:       r.Key,
		Value:     r.Value,
		Headers:   headers,
		Context:   context.WithValue(trace.ContextWithSpan(call, span), sendingKey{}, &sending{span: span, start: start}),
	}
}

// sendHook ends the span of each record the Producer produced, and
// measures it, as the record's result arrives.
type sendHook struct {
	sent       messagingconv.ClientSentMessages
	operations messagingconv.ClientOperationDuration
}

var _ kgo.HookProduceRecordUnbuffered = sendHook{}

// OnProduceRecordUnbuffered is called just before r's result, err, is
// handed to ProduceSync. An acknowledged record's span gets its partition
// and offset, and a failed record's span gets the error. Both instruments
// get the record's partition when one was chosen, and error.type when it
// failed.
func (h sendHook) OnProduceRecordUnbuffered(r *kgo.Record, err error) {
	s, ok := r.Context.Value(sendingKey{}).(*sending)
	if !ok {
		return
	}
	elapsed := time.Since(s.start).Seconds()

	attrs := []attribute.KeyValue{semconv.MessagingDestinationName(r.Topic)}
	if r.Partition >= 0 {
		attrs = append(attrs, semconv.MessagingDestinationPartitionID(strconv.FormatInt(int64(r.Partition), 10)))
	}
	if err == nil {
		s.span.SetAttributes(
			semconv.MessagingDestinationPartitionID(strconv.FormatInt(int64(r.Partition), 10)),
			semconv.MessagingKafkaOffset(int(r.Offset)),
		)
	} else {
		errType := semconv.ErrorType(err)
		s.span.RecordError(err)
		s.span.SetStatus(codes.Error, err.Error())
		s.span.SetAttributes(errType)
		attrs = append(attrs, errType)
	}
	s.span.End()

	h.sent.Add(r.Context, 1, sendOperation, messagingconv.SystemKafka, attrs...)
	h.operations.Record(r.Context, elapsed, sendOperation, messagingconv.SystemKafka,
		append(attrs, semconv.MessagingOperationTypeSend)...)
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
