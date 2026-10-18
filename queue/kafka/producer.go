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
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/semconv/v1.43.0/messagingconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
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
	// header whose key is one the Setup's propagator writes, such as
	// traceparent or baggage, is removed, and the propagator then writes
	// the record's own span context and the baggage of Produce's ctx.
	Headers []Header
}

// Producer produces records to one Kafka cluster. It is safe for concurrent
// use: one Producer serves every Service of the process.
type Producer struct {
	client          *kgo.Client
	deliveryTimeout time.Duration
	tracer          trace.Tracer
	propagator      propagation.TextMapPropagator
	logger          log.Logger

	// breaker guards the cluster. Each call to Produce is one call through
	// it.
	breaker *circuit.Breaker

	// metrics are humus's own instruments of the breaker.
	metrics *instruments

	// faults is the App's failure injection, from the Setup, and forward
	// is ProducerSettings.ForwardScenario.
	faults  fault.Injector
	forward bool

	// closed is set once the OnStop function has flushed the client and is
	// about to close it.
	closed atomic.Bool

	// refusal is the first refusal of the credentials by a broker, nil
	// until there is one. refused is closed once it is set.
	refusal atomic.Pointer[error]
	refused chan struct{}
}

// NewProducer runs every check on settings, reporting every bad field in
// one error that names each one, and builds the franz-go client and the
// circuit breaker. It opens no connection: the first Produce does. It
// registers a function with s.OnStop that flushes the client and closes it.
//
// The breaker, built with settings.Breaker and settings.BreakerPolicy,
// guards the cluster: one Producer is one client to one cluster, with one
// breaker. A BreakerPolicy with a nil Trip or Probe is a bad field, reported
// with the others.
//
// The Producer dials every broker through s.Host. It reports through
// s.Telemetry: its spans, the two messaging instruments and its own logs
// under the scope github.com/z5labs/humus/queue/kafka, franz-go's logs under
// github.com/twmb/franz-go/pkg/kgo, and franz-go's broker metrics, from
// kotel's Meter, under github.com/twmb/franz-go/plugin/kotel. The
// breaker's state, its changes of state and the calls it counted or
// rejected are three instruments of humus's own, under the Producer's
// scope, and each change of the breaker's state is one of its own log
// records.
//
// A Producer can't stop the App, so when a broker refuses settings.SASL's
// credentials, with SASL_AUTHENTICATION_FAILED, UNSUPPORTED_SASL_MECHANISM
// or ILLEGAL_SASL_STATE, it fails every call instead: it emits one ERROR
// record naming the broker, the calls waiting on their records return the
// refusal, and every later Produce returns it at once, with nothing sent.
// Credentials don't fix themselves while the process runs, so the Producer
// never tries them again.
func NewProducer(s *humus.Setup, settings ProducerSettings) (*Producer, error) {
	if err := checkProducerSettings(settings); err != nil {
		return nil, fmt.Errorf("kafka: NewProducer: %w", err)
	}
	meter := s.Telemetry.MeterProvider.Meter(scopeName)
	sent, err1 := messagingconv.NewClientSentMessages(meter)
	operations, err2 := messagingconv.NewClientOperationDuration(meter)
	metrics, err3 := newInstruments(meter, settings.Brokers[0])
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, fmt.Errorf("kafka: NewProducer: creating the instruments: %w", err)
	}
	p := &Producer{
		deliveryTimeout: settings.DeliveryTimeout,
		tracer:          s.Telemetry.TracerProvider.Tracer(scopeName),
		propagator:      s.Telemetry.Propagator,
		logger:          s.Telemetry.LoggerProvider.Logger(scopeName),
		metrics:         metrics,
		faults:          s.Faults,
		forward:         settings.ForwardScenario,
		refused:         make(chan struct{}),
	}
	breaker, err := circuit.New(settings.Breaker, settings.BreakerPolicy, metrics.countCalls(classify), p.observe)
	if err != nil {
		return nil, fmt.Errorf("kafka: NewProducer: building the circuit breaker: %w", err)
	}
	p.breaker = breaker
	opts := []kgo.Opt{
		kgo.SeedBrokers(settings.Brokers...),
		kgo.Dialer(tlsDialer(s.Host.Dial, settings.TLS)),
		kgo.WithLogger(kgoLogger{ctx: context.Background(), logger: s.Telemetry.LoggerProvider.Logger(kgoScope)}),
		kgo.WithHooks(
			sendHook{sent: sent, operations: operations},
			connectHook{onRefusal: p.refuse},
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
	// humus.circuit.state's callback is registered last, once nothing after
	// it can fail, so a NewProducer that returns an error leaves none
	// behind.
	if err := metrics.observeState(meter, breaker); err != nil {
		cl.Close()
		return nil, fmt.Errorf("kafka: NewProducer: registering humus.circuit.state's callback: %w", err)
	}
	p.client = cl
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
	check("ProducerSettings.Breaker.Window", CheckBreakerWindow(settings.Breaker.Window))
	check("ProducerSettings.Breaker.Probes", CheckBreakerProbes(settings.Breaker.Probes))
	check("ProducerSettings.Breaker.OpenFor", CheckBreakerOpenFor(settings.Breaker.OpenFor))
	check("ProducerSettings.Breaker.ProbeTimeout", CheckBreakerProbeTimeout(settings.Breaker.ProbeTimeout))
	if settings.BreakerPolicy.Trip == nil {
		check("ProducerSettings.BreakerPolicy.Trip", errNotSet)
	}
	if settings.BreakerPolicy.Probe == nil {
		check("ProducerSettings.BreakerPolicy.Probe", errNotSet)
	}
	return errors.Join(errs...)
}

// refuse records err, a broker's refusal of the credentials, if it is the
// first. It emits one ERROR record naming the broker, and then fails every
// call waiting on its records, and every later call, with err.
func (p *Producer) refuse(meta kgo.BrokerMetadata, err error) {
	if !p.refusal.CompareAndSwap(nil, &err) {
		return
	}
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(log.SeverityError)
	r.SetSeverityText(log.SeverityError.String())
	r.SetBody(attribute.StringValue("a broker refused the credentials"))
	r.AddAttributes(
		semconv.MessagingSystemKafka,
		semconv.ServerAddress(meta.Host),
		semconv.ServerPort(int(meta.Port)),
		semconv.ErrorType(err),
		semconv.ExceptionMessage(err.Error()),
	)
	p.logger.Emit(context.Background(), r)
	close(p.refused)
}

// refusalErr returns the broker's refusal of the credentials, or nil when
// no broker has refused them.
func (p *Producer) refusalErr() error {
	if err := p.refusal.Load(); err != nil {
		return *err
	}
	return nil
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
// record failed or ProducerSettings.DeliveryTimeout passed; an error that
// wraps a *circuit.RejectedError, which matches circuit.ErrRejected and says
// how much longer the breaker stays open, at once and with nothing sent,
// while the breaker is open; and an error that wraps ctx's error, at once,
// when ctx is done first. A call whose ctx is already done sends nothing.
// Records handed to the client before ctx is done go on to their result,
// within DeliveryTimeout, and never fail another call's records. A record
// that failed may still have been stored, so a caller that produces it
// again may store it twice.
//
// Produce rejects a record with no Topic before it sends anything. With no
// records, it returns nil.
//
// When a broker refuses the credentials, Produce returns an error naming
// the broker as host:port, which matches the refusal's kerr code, such as
// kerr.SaslAuthenticationFailed, with errors.Is. A call waiting on its
// records returns it as soon as the broker refuses, and every call after
// that returns it at once, with nothing sent: the Producer doesn't try the
// credentials again. The records of a call that returned the refusal stay
// in the client until DeliveryTimeout fails them.
//
// Each call is one call through the Producer's circuit breaker, admitted
// before any of its records reach franz-go and counted once every record
// has its result, which may be after Produce returns. A call that succeeds
// counts as a success, and one whose records failed because the cluster
// didn't store them in time counts as a failure. A call whose records
// failed only for their own reasons or their topics', such as
// MESSAGE_TOO_LARGE, isn't counted by the breaker. humus.circuit.calls
// counts each call once, with the outcome the breaker gave it: success,
// failure or ignored. A call the breaker rejects is counted there too,
// once whatever its number of records, as rejected. Produce doesn't retry
// a rejected call: whether to wait and try again is the caller's. A call
// whose ctx is already done, and a call after the cluster refused the
// credentials, never reach the breaker, so it counts neither, and neither
// does humus.circuit.calls.
//
// Each record it hands to the client is sent under a span of its own, a
// child of the span in ctx. The record's headers whose keys the propagator
// writes are removed, and the propagator writes the span's context and
// ctx's baggage in their place, less a failure scenario unless
// ProducerSettings.ForwardScenario is set. The span ends, and the record is
// measured, when its result arrives, which may be after Produce returns. A
// record the breaker rejected was never sent: its span records the
// rejection as a failure, and neither messaging instrument counts it.
//
// With failure injection, each record of a call the breaker admits
// evaluates its point, kafka:<topic>, as the package doc's Failure
// injection describes. The call waits the longest Delay of its records,
// inside DeliveryTimeout, and when any record's fault aborts, every record
// of the call fails unsent with that point's *fault.InjectedError, which
// the breaker counts as a failure.
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
	if err := p.refusalErr(); err != nil {
		return err
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

	t, err := p.breaker.Allow(call)
	if err != nil {
		p.metrics.rejected(call)
		cancel()
		for _, kr := range krs {
			s := kr.Context.Value(sendingKey{}).(*sending)
			recordFailure(s.span, err)
			s.span.End()
		}
		return fmt.Errorf("kafka: producing: %w", err)
	}
	// The ticket is settled once every record has its result, which may be
	// after Produce returns, so the guard is a flag set once the ticket is
	// handed off, not a deferred Release.
	handedOff := false
	defer func() {
		if !handedOff {
			t.Release()
		}
	}()

	// Failure injection has its point here, just after Allow: each record
	// evaluates kafka:<topic> with its own context, so a call the breaker
	// rejected evaluates nothing, and the breaker counts a call a fault
	// ended as it counts any other.
	inj := p.evaluate(krs)

	results := make(chan error, 1)
	go func() {
		defer cancel()
		var err error
		if injected := inj.inject(call); injected != nil {
			err = failUnsent(krs, injected)
		} else {
			err = produceError(krs, p.client.ProduceSync(call, krs...))
		}
		// Settled before the result is handed back, so a caller's next
		// call meets the breaker this one left.
		t.Done(err)
		results <- err
	}()
	handedOff = true

	select {
	case err := <-results:
		return err
	case <-ctx.Done():
		return fmt.Errorf("kafka: producing: %w", ctx.Err())
	case <-p.refused:
		return p.refusalErr()
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
// which the propagator injects, less the failure scenario unless the
// Producer forwards it. The record's context is call, carrying the
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
	p.propagator.Inject(p.headersContext(ctx, span), (*headerCarrier)(&headers))

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
		attrs = append(attrs, recordFailure(s.span, err))
	}
	s.span.End()

	h.sent.Add(r.Context, 1, sendOperation, messagingconv.SystemKafka, attrs...)
	h.operations.Record(r.Context, elapsed, sendOperation, messagingconv.SystemKafka,
		append(attrs, semconv.MessagingOperationTypeSend)...)
}

// recordFailure records err on span as an exception event, with error.type
// and an error status, and returns the error.type attribute.
func recordFailure(span trace.Span, err error) attribute.KeyValue {
	errType := semconv.ErrorType(err)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	span.SetAttributes(errType)
	return errType
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
