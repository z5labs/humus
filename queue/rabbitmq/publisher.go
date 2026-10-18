// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
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

// sendOperation is the messaging.operation.name of publishing a message.
const sendOperation = "send"

// defaultExchange is the name a publish to the default exchange has in its
// span's name, as semconv names it.
const defaultExchange = "amq.default"

// Publishing is a message to publish. Its zero value is persistent.
type Publishing struct {
	// Exchange is the exchange to publish to, "" for the default exchange.
	// It must already exist: the Publisher declares nothing.
	Exchange string

	// RoutingKey is the routing key to publish with.
	RoutingKey string

	// Headers are its headers. Publish never changes the map: it publishes
	// a copy, with the trace context injected.
	Headers map[string]any

	// ContentType is its MIME content type.
	ContentType string

	// ContentEncoding is its MIME content encoding.
	ContentEncoding string

	// Transient makes it transient, with delivery mode 1, so the broker
	// doesn't write it to disk. It is false by default, which publishes it
	// persistent, with delivery mode 2.
	Transient bool

	// Priority is its priority, from 0 to 9.
	Priority uint8

	// CorrelationID is its correlation ID.
	CorrelationID string

	// ReplyTo is the address to reply to.
	ReplyTo string

	// Expiration is its expiration, in milliseconds.
	Expiration string

	// MessageID is its ID.
	MessageID string

	// Timestamp is its time.
	Timestamp time.Time

	// Type is its type name.
	Type string

	// AppID is the ID of the application that publishes it.
	AppID string

	// Body is its body.
	Body []byte
}

// amqpPublishing returns m as amqp091's Publishing: persistent unless
// Transient is set, with a copy of m.Headers, a new map when m.Headers is
// nil, and every other field as it is.
func (m Publishing) amqpPublishing() amqp.Publishing {
	mode := amqp.Persistent
	if m.Transient {
		mode = amqp.Transient
	}
	headers := make(amqp.Table, len(m.Headers))
	maps.Copy(headers, m.Headers)
	return amqp.Publishing{
		Headers:         headers,
		ContentType:     m.ContentType,
		ContentEncoding: m.ContentEncoding,
		DeliveryMode:    mode,
		Priority:        m.Priority,
		CorrelationId:   m.CorrelationID,
		ReplyTo:         m.ReplyTo,
		Expiration:      m.Expiration,
		MessageId:       m.MessageID,
		Timestamp:       m.Timestamp,
		Type:            m.Type,
		AppId:           m.AppID,
		Body:            m.Body,
	}
}

// ErrUnroutable is matched by the error of a publish the broker returned
// because no queue was bound to receive it.
var ErrUnroutable = errors.New("rabbitmq: message unroutable")

// errStopped is the error of a Publish once the Publisher's OnStop function
// has run.
var errStopped = errors.New("rabbitmq: the Publisher is stopped")

// errNacked is matched by the error of a publish the broker nacked.
var errNacked = errors.New("the broker nacked the message")

// lostError is the error of an attempt that never reached the broker, or
// whose answer was lost: a failed dial or handshake, a lost connection, or
// a channel that closed without an error of the broker's. Its message, and
// its error.type, are its cause's, so marking an error lost changes neither.
type lostError struct{ err error }

func (e *lostError) Error() string { return e.err.Error() }

func (e *lostError) Unwrap() error { return e.err }

// ErrorType is semconv's error.type of the cause, which semconv.ErrorType
// asks for when it finds a lostError in an error's chain.
func (e *lostError) ErrorType() string {
	return semconv.ErrorType(e.err).Value.AsString()
}

// lost marks err as the error of an attempt that never reached the broker,
// or whose answer was lost, so classify retries it.
func lost(err error) error {
	return &lostError{err: err}
}

// classify is the Publisher's classifier for retry.New. RabbitMQ's protocol
// fixes what each result of an attempt means, so it is fixed too:
//
//   - confirmed: Success;
//   - returned unroutable, ErrUnroutable: Stop, since another attempt is
//     routed the same way;
//   - a channel error, such as 404 NOT_FOUND or 403 ACCESS_REFUSED: Stop,
//     since the broker refused the publish;
//   - nacked by the broker, or lost: a dropped connection, a channel closed
//     without the broker's error, a failed dial or a failed handshake:
//     Retry, since another attempt, on a new channel or connection, may be
//     confirmed;
//   - an attempt that ended with ctx, its deadline or the caller
//     cancelling: Retry, which retry turns into Expired, since ctx is done;
//   - an attempt the breaker rejected, circuit.ErrRejected: Stop, since
//     nothing was sent and the breaker stays open for a while yet;
//   - the Publisher stopped, and anything else: Stop.
func classify(ctx context.Context, err error) (retry.Outcome, time.Duration) {
	switch {
	case err == nil:
		return retry.Success, 0
	case errors.Is(err, ErrUnroutable), errors.Is(err, errStopped), errors.Is(err, circuit.ErrRejected):
		return retry.Stop, 0
	case errors.Is(err, errNacked):
		return retry.Retry, 0
	}
	if _, ok := errors.AsType[*lostError](err); ok {
		return retry.Retry, 0
	}
	if ctx.Err() != nil {
		return retry.Retry, 0
	}
	return retry.Stop, 0
}

// publishChannel is what the Publisher uses of an AMQP channel.
// *amqp.Channel satisfies it, and a unit test replaces it with a stand-in.
type publishChannel interface {
	Confirm(noWait bool) error
	NotifyPublish(c chan amqp.Confirmation) chan amqp.Confirmation
	NotifyReturn(c chan amqp.Return) chan amqp.Return
	NotifyClose(c chan *amqp.Error) chan *amqp.Error
	PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error
	IsClosed() bool
	Close() error
}

var _ publishChannel = (*amqp.Channel)(nil)

// publishConn is what the Publisher uses of an AMQP connection. amqpConn
// adapts *amqp.Connection to it, and a unit test replaces it with a
// stand-in.
type publishConn interface {
	// OpenChannel opens a channel on the connection.
	OpenChannel() (publishChannel, error)
	// IsClosed reports whether the connection is closed, by either end or
	// by its loss.
	IsClosed() bool
	// CloseDeadline closes the connection, waiting for the broker's answer
	// until deadline, or without a bound when it is zero.
	CloseDeadline(deadline time.Time) error
}

// amqpConn is an amqp091 connection, as a publishConn.
type amqpConn struct{ *amqp.Connection }

func (c amqpConn) OpenChannel() (publishChannel, error) {
	ch, err := c.Channel()
	if err != nil {
		return nil, err
	}
	return ch, nil
}

// dialFunc connects to the broker.
type dialFunc func(ctx context.Context) (publishConn, error)

// Publisher publishes to the broker over a connection of its own.
type Publisher struct {
	settings PublisherSettings
	dial     dialFunc

	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	sent       messagingconv.ClientSentMessages
	duration   messagingconv.ClientOperationDuration
	logger     log.Logger

	// retrier makes the attempts of every Publish, within one Budget.
	retrier *retry.Retrier

	// breaker guards the broker. Each attempt of every Publish is one call
	// through it.
	breaker *circuit.Breaker

	// serverAddr and serverPort are Connection.Addr's host and port.
	serverAddr string
	serverPort int

	// metrics are humus's own instruments of the breaker and the retries.
	metrics *instruments

	// faults is the App's failure injection, from the Setup.
	faults fault.Injector

	// slots holds a token for each publish in flight, so there are at most
	// Channels of them.
	slots chan struct{}

	// idle are the channels no publish is using, the most recently used
	// last.
	idleMu sync.Mutex
	idle   []*confirmChannel

	// connLock is held, by sending to it, while conn is read or replaced.
	// It is a channel, so that waiting for it can end with a context.
	connLock chan struct{}
	conn     publishConn

	// stopped is done once the Publisher's OnStop function has run.
	stopped context.Context
	stop    context.CancelFunc
}

// confirmChannel is a channel in confirm mode, with its listeners, on the
// connection it was opened on.
type confirmChannel struct {
	ch       publishChannel
	conn     publishConn
	confirms chan amqp.Confirmation
	returns  chan amqp.Return
	closes   chan *amqp.Error
}

// NewPublisher checks settings and returns a Publisher. It opens no
// connection: the first Publish does. It registers the connection's close
// with s.OnStop.
//
// It reports every field of settings that fails its check in one error that
// names each one, a BackoffMax below BackoffBase included. The Publisher
// reaches the broker only through s.Host.
//
// It builds the one retry.Budget, from RetryBudget, and the one
// retry.Retrier, with retry.Exponential(BackoffBase, BackoffMax) and
// MaxAttempts, that every Publish shares.
//
// It builds one circuit breaker, with circuit.New, from Breaker and
// BreakerPolicy, that every attempt of every Publish goes through, inside
// the retries. A BreakerPolicy with a nil Trip or Probe is a bad field,
// reported with the others. Each change of the breaker's state is one log
// record, as Publish describes.
//
// The function NewPublisher registers with s.OnStop closes the connection,
// if one is open, within the grace period. It runs once every Service has
// stopped, so a Processor finishing its delivery during the drain can still
// publish, and it also runs when Init fails after NewPublisher. After it
// has run, Publish fails at once.
//
// The Publisher reports through s.Telemetry, and never through
// OpenTelemetry's globals, under the Service's scope,
// github.com/z5labs/humus/queue/rabbitmq: a span for every Publish, two
// messaging instruments for every attempt, a log record for every change
// of the breaker's state, and four instruments of humus's own for the
// breaker and the retries, as Publish describes. humus.circuit.state's
// callback is registered last, once nothing after it can fail, so a
// NewPublisher that returns an error leaves none behind.
func NewPublisher(s *humus.Setup, settings PublisherSettings) (*Publisher, error) {
	if err := checkPublisherSettings(settings); err != nil {
		return nil, fmt.Errorf("rabbitmq: NewPublisher: %w", err)
	}
	meter := s.Telemetry.MeterProvider.Meter(scopeName)
	sent, err1 := messagingconv.NewClientSentMessages(meter)
	duration, err2 := messagingconv.NewClientOperationDuration(meter)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("rabbitmq: NewPublisher: creating the instruments: %w", err)
	}
	retrier, err := newRetrier(settings)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: NewPublisher: %w", err)
	}
	// CheckAddr has accepted Addr, so it splits, and its port is a number.
	host, port, _ := net.SplitHostPort(settings.Connection.Addr)
	portNum, _ := strconv.Atoi(port)
	metrics, err := newInstruments(meter, host, portNum)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: NewPublisher: creating the instruments: %w", err)
	}

	stopped, stop := context.WithCancel(context.Background())
	p := &Publisher{
		settings:   settings,
		tracer:     s.Telemetry.TracerProvider.Tracer(scopeName),
		propagator: s.Telemetry.Propagator,
		sent:       sent,
		duration:   duration,
		logger:     s.Telemetry.LoggerProvider.Logger(scopeName),
		retrier:    retrier,
		serverAddr: host,
		serverPort: portNum,
		metrics:    metrics,
		faults:     s.Faults,
		slots:      make(chan struct{}, settings.Channels),
		connLock:   make(chan struct{}, 1),
		stopped:    stopped,
		stop:       stop,
	}
	breaker, err := circuit.New(settings.Breaker, settings.BreakerPolicy, metrics.countCalls(classifyBreaker), p.observe)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: NewPublisher: building the circuit breaker: %w", err)
	}
	// humus.circuit.state's callback is registered last, once nothing after
	// it can fail, so a NewPublisher that returns an error leaves none
	// behind.
	if err := metrics.observeState(meter, breaker); err != nil {
		return nil, fmt.Errorf("rabbitmq: NewPublisher: registering humus.circuit.state's callback: %w", err)
	}
	p.breaker = breaker
	p.dial = func(ctx context.Context) (publishConn, error) {
		c, err := connect(ctx, s.Host, settings.Connection)
		if err != nil {
			return nil, err
		}
		return amqpConn{c}, nil
	}
	s.OnStop(p.close)
	return p, nil
}

// checkPublisherSettings runs every check on settings, and reports every
// failure in one error, each naming its field.
func checkPublisherSettings(settings PublisherSettings) error {
	errs := checkConnection("PublisherSettings.Connection", settings.Connection)
	if err := CheckChannels(settings.Channels); err != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.Channels %w", err))
	}
	if err := CheckTimeout(settings.Timeout); err != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.Timeout %w", err))
	}
	if err := CheckMaxAttempts(settings.MaxAttempts); err != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.MaxAttempts %w", err))
	}
	errBase := CheckBackoffBase(settings.BackoffBase)
	if errBase != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.BackoffBase %w", errBase))
	}
	errMax := CheckBackoffMax(settings.BackoffMax)
	if errMax != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.BackoffMax %w", errMax))
	}
	if errBase == nil && errMax == nil && settings.BackoffMax < settings.BackoffBase {
		errs = append(errs, errors.New("PublisherSettings.BackoffMax must be at least PublisherSettings.BackoffBase"))
	}
	if err := CheckRetryBudgetMaxTokens(settings.RetryBudget.MaxTokens); err != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.RetryBudget.MaxTokens %w", err))
	}
	if err := CheckRetryBudgetTokenRatio(settings.RetryBudget.TokenRatio); err != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.RetryBudget.TokenRatio %w", err))
	}
	if err := CheckBreakerWindow(settings.Breaker.Window); err != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.Breaker.Window %w", err))
	}
	if err := CheckBreakerProbes(settings.Breaker.Probes); err != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.Breaker.Probes %w", err))
	}
	if err := CheckBreakerOpenFor(settings.Breaker.OpenFor); err != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.Breaker.OpenFor %w", err))
	}
	if err := CheckBreakerProbeTimeout(settings.Breaker.ProbeTimeout); err != nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.Breaker.ProbeTimeout %w", err))
	}
	if settings.BreakerPolicy.Trip == nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.BreakerPolicy.Trip %w", errNotSet))
	}
	if settings.BreakerPolicy.Probe == nil {
		errs = append(errs, fmt.Errorf("PublisherSettings.BreakerPolicy.Probe %w", errNotSet))
	}
	return errors.Join(errs...)
}

// newRetrier builds the Publisher's retry.Retrier from settings, which
// checkPublisherSettings has passed.
func newRetrier(settings PublisherSettings) (*retry.Retrier, error) {
	budget, err := retry.NewBudget(settings.RetryBudget)
	if err != nil {
		return nil, err
	}
	schedule, err := retry.Exponential(settings.BackoffBase, settings.BackoffMax)
	if err != nil {
		return nil, err
	}
	return retry.New(settings.MaxAttempts, schedule, classify, budget)
}

// close is the Publisher's OnStop function. It stops the Publisher, so every
// Publish after it fails at once, and closes the connection, if one is open,
// within ctx.
func (p *Publisher) close(ctx context.Context) error {
	p.stop()
	select {
	case p.connLock <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("rabbitmq: closing the Publisher's connection to %s: %w", p.settings.Connection.Addr, ctx.Err())
	}
	defer func() { <-p.connLock }()
	if p.conn == nil || p.conn.IsClosed() {
		return nil
	}
	deadline, _ := ctx.Deadline()
	if err := p.conn.CloseDeadline(deadline); err != nil && !errors.Is(err, amqp.ErrClosed) {
		return fmt.Errorf("rabbitmq: closing the Publisher's connection to %s: %w", p.settings.Connection.Addr, err)
	}
	return nil
}

// Publish publishes m and returns once the broker has confirmed it. It is
// safe for concurrent use.
//
// Each attempt publishes m with mandatory set, on a channel in confirm
// mode, and waits for the broker's confirm. An attempt after a lost channel
// or connection takes a new channel, connecting first if needed. What each
// attempt's result means is fixed by RabbitMQ's protocol, so the Publisher
// has no Classify setting:
//
//   - confirmed: Publish returns nil;
//   - returned because no queue was bound to receive m: Publish returns an
//     error matching ErrUnroutable at once, since another attempt would be
//     routed the same way;
//   - a channel error, as with 404 NOT_FOUND for an exchange that doesn't
//     exist or 403 ACCESS_REFUSED: Publish returns the broker's *amqp.Error
//     at once, since the broker refused m;
//   - nacked by the broker, a dropped connection, a failed dial or a failed
//     handshake: another attempt follows, after a wait drawn from
//     retry.Exponential(BackoffBase, BackoffMax), up to MaxAttempts
//     attempts in all, and Publish returns the last attempt's error. 1
//     MaxAttempts turns retries off.
//
// The retries of every Publish of p share one RetryBudget: each failure
// that would be retried takes a token, each confirm adds back TokenRatio,
// and no attempt follows a failure while half the tokens or fewer are
// left, so a broker that keeps failing gets one attempt per Publish rather
// than MaxAttempts. A first attempt is never refused.
//
// Every attempt of every Publish of p goes through one circuit breaker,
// built from Breaker and BreakerPolicy, so a broker that keeps failing
// fails each Publish at once rather than holding it for Timeout. While the
// breaker is open, an attempt sends nothing and fails at once with an error
// wrapping a *circuit.RejectedError, which matches circuit.ErrRejected and
// says how much longer the breaker stays open, and Publish returns it
// without another attempt. Whether to wait and try again is the caller's.
// With the default BreakerPolicy, the breaker opens after 5 failed
// attempts in a row, or once half of the last 20 or more failed; after
// OpenFor it lets one attempt through at a time, and closes after 3 of
// them are confirmed or reopens on one that fails. What each attempt's
// result counts as:
//
//   - confirmed: a success;
//   - returned unroutable, or a channel error such as 404 NOT_FOUND or 403
//     ACCESS_REFUSED: a success, since the broker answered;
//   - nacked by the broker, a dropped connection, a failed dial or a
//     failed handshake: a failure;
//   - ended by ctx's deadline, Timeout's or the caller's: a failure;
//   - ended by the caller cancelling ctx: not counted, since it says
//     nothing about the broker.
//
// Each change of the breaker's state is one log record, through the
// Setup's LoggerProvider, in the context of the Publish that made it:
// "circuit breaker open" at WARN, and "circuit breaker half-open" and
// "circuit breaker closed" at INFO. Each has server.address and
// server.port, from Connection.Addr, the state the breaker left as
// humus.circuit.previous_state, and the counts of that state, as
// humus.circuit.successes, humus.circuit.failures,
// humus.circuit.consecutive_successes, humus.circuit.consecutive_failures
// and humus.circuit.timed_out.
//
// The breaker and the retries are measured with humus's own instruments,
// each with server.address and server.port: humus.circuit.state reads the
// breaker's state at each collection, humus.circuit.transitions counts each
// change of state, humus.circuit.calls counts each attempt with the
// circuit.Outcome the breaker gave it, or rejected for an attempt the
// breaker didn't admit, and humus.retry.decisions counts the
// retry.Decision after each attempt, stopped for a rejected one.
//
// Settings.Timeout bounds the whole Publish, every attempt and the waits
// between them included, unless ctx has an earlier deadline, which then
// wins. No attempt starts after it, and no wait is begun that would end
// after it. When ctx is done first, or the caller cancels it, no further
// attempt is made, and Publish returns an error wrapping ctx's error.
//
// A confirm can be lost with its connection after the broker took m, and
// the attempt that follows then publishes m again. So publishing is at
// least once, and a consumer of m is idempotent anyway, under queue's
// contract.
//
// Each Publish has a span, a child of ctx's, named "send <exchange>", or
// "send amq.default" for the default exchange, of kind PRODUCER. Its
// attributes are messaging.system (rabbitmq), messaging.operation.type and
// messaging.operation.name (both send), messaging.destination.name
// ({exchange}:{routing key}, leaving out what is empty),
// messaging.rabbitmq.destination.routing_key when it isn't empty,
// messaging.message.id and messaging.message.conversation_id when they are
// set, server.address and server.port. The span is started before m's
// headers are written, and the Setup's propagator injects its trace context
// into a copy of m.Headers, so the consumer's span is its child. m.Headers
// itself is never changed. A failed Publish is recorded on its span, with
// error.type and an error status, a Publish the breaker rejected included.
// The body is never recorded. Unless ForwardScenario is set, a failure
// scenario is stripped from the context the headers are injected from,
// with a humus.fit.strip event on the span.
//
// With failure injection, each attempt the breaker admits evaluates its
// point, rabbitmq:<messaging.destination.name>, before it sends anything,
// as the package doc's Failure injection describes: an abort fails the
// attempt as a lost connection does, and a Delay is waited inside Timeout.
//
// Each attempt that publishes m records messaging.client.sent.messages, and
// messaging.client.operation.duration, from the publish to the broker's
// confirm, in seconds. Each has messaging.system,
// messaging.operation.name, messaging.destination.name, server.address and
// server.port, and error.type when the attempt failed: the broker's reply
// code for a return or a channel error, nacked for a nack. An attempt that
// fails before it publishes, waiting for a channel or connecting, publishes
// nothing and records neither, and so does an attempt the breaker
// rejected. Every attempt is within the Publish's one span.
func (p *Publisher) Publish(ctx context.Context, m Publishing) (err error) {
	ctx, span := p.tracer.Start(ctx, spanName(m.Exchange),
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(p.spanAttrs(m)...),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			span.SetAttributes(errorType(err))
		}
		span.End()
	}()

	if p.stopped.Err() != nil {
		return errStopped
	}
	ctx, cancel := context.WithTimeout(ctx, p.settings.Timeout)
	defer cancel()

	msg := m.amqpPublishing()
	p.propagator.Inject(p.headersContext(ctx, span), (*headerCarrier)(&msg.Headers))

	for a := range p.retrier.Attempts(ctx) {
		err = p.attempt(ctx, m, msg)
		p.metrics.done(ctx, a, err)
	}
	if err != nil && ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
		// ctx ended during the wait after a failed attempt: the caller sees
		// why it ended, and why the attempt before failed.
		err = fmt.Errorf("rabbitmq: publishing to %s: %w; the last attempt failed: %w", destination(m), context.Cause(ctx), err)
	}
	return err
}

// attempt is one attempt of a Publish, and one call through the breaker.
// Once the breaker admits it, failure injection evaluates m's point, and
// unless a fault ended the attempt, it takes a channel, publishes msg,
// which is m with its trace context, on it, and gives the channel back.
// While the breaker is open, it returns an error wrapping the breaker's
// *circuit.RejectedError at once, without evaluating m's point or
// sending anything.
func (p *Publisher) attempt(ctx context.Context, m Publishing, msg amqp.Publishing) error {
	t, err := p.breaker.Allow(ctx)
	if err != nil {
		p.metrics.rejected(ctx)
		return fmt.Errorf("rabbitmq: publishing to %s: %w", destination(m), err)
	}
	defer t.Release()

	// Failure injection has its point here: the attempt is inside the
	// breaker, so an attempt the breaker rejected evaluates nothing, and
	// the breaker counts an attempt a fault ended as it counts any other;
	// and nothing of it has been written to the broker.
	if err = p.inject(ctx, m); err == nil {
		err = p.send(ctx, m, msg)
	}
	t.Done(err)
	return err
}

// send takes a channel, publishes msg, which is m with its trace context,
// on it, and gives the channel back.
func (p *Publisher) send(ctx context.Context, m Publishing, msg amqp.Publishing) error {
	cc, err := p.take(ctx, m)
	if err != nil {
		return err
	}
	reuse, err := p.publish(ctx, cc, m, msg)
	p.give(cc, reuse)
	return err
}

// publish publishes msg, which is m with its trace context, on cc, and
// records it on both instruments. It returns what cc.publish returns.
func (p *Publisher) publish(ctx context.Context, cc *confirmChannel, m Publishing, msg amqp.Publishing) (reuse bool, err error) {
	start := time.Now()
	reuse, err = cc.publish(ctx, m, msg)
	elapsed := time.Since(start).Seconds()

	attrs := p.instrumentAttrs(m)
	if err != nil {
		attrs = append(attrs, errorType(err))
	}
	p.sent.Add(ctx, 1, sendOperation, messagingconv.SystemRabbitMQ, attrs...)
	p.duration.Record(ctx, elapsed, sendOperation, messagingconv.SystemRabbitMQ, attrs...)
	return reuse, err
}

// spanName is the name of the span of a publish to exchange.
func spanName(exchange string) string {
	if exchange == "" {
		exchange = defaultExchange
	}
	return sendOperation + " " + exchange
}

// spanAttrs returns the attributes of m's span.
func (p *Publisher) spanAttrs(m Publishing) []attribute.KeyValue {
	attrs := append(p.instrumentAttrs(m),
		semconv.MessagingSystemRabbitMQ,
		semconv.MessagingOperationTypeSend,
		semconv.MessagingOperationName(sendOperation),
	)
	if m.RoutingKey != "" {
		attrs = append(attrs, semconv.MessagingRabbitMQDestinationRoutingKey(m.RoutingKey))
	}
	if m.MessageID != "" {
		attrs = append(attrs, semconv.MessagingMessageID(m.MessageID))
	}
	if m.CorrelationID != "" {
		attrs = append(attrs, semconv.MessagingMessageConversationID(m.CorrelationID))
	}
	return attrs
}

// instrumentAttrs returns the attributes both instruments record for m,
// beside messaging.system, messaging.operation.name and error.type:
// messaging.destination.name, when it isn't empty, server.address and
// server.port. Each call returns a new slice.
func (p *Publisher) instrumentAttrs(m Publishing) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 10)
	if name := sendDestinationName(m.Exchange, m.RoutingKey); name != "" {
		attrs = append(attrs, semconv.MessagingDestinationName(name))
	}
	return append(attrs,
		semconv.ServerAddress(p.serverAddr),
		semconv.ServerPort(p.serverPort),
	)
}

// sendDestinationName is a publish's messaging.destination.name, as semconv
// v1.43.0 defines it for RabbitMQ: {exchange}:{routing key}, leaving out
// what is empty. A message published to the default exchange with the
// routing key orders is orders.
func sendDestinationName(exchange, routingKey string) string {
	parts := make([]string, 0, 2)
	if exchange != "" {
		parts = append(parts, exchange)
	}
	if routingKey != "" {
		parts = append(parts, routingKey)
	}
	return strings.Join(parts, ":")
}

// errorType is the error.type of a failed publish: the AMQP reply code of
// a channel error, or of the return of an unroutable message, nacked for a
// nack, and semconv's error.type of any other error.
func errorType(err error) attribute.KeyValue {
	if ae, ok := errors.AsType[*amqp.Error](err); ok {
		return semconv.ErrorTypeKey.String(strconv.Itoa(ae.Code))
	}
	switch {
	case errors.Is(err, ErrUnroutable):
		return semconv.ErrorTypeKey.String(strconv.Itoa(amqp.NoRoute))
	case errors.Is(err, errNacked):
		return semconv.ErrorTypeKey.String("nacked")
	default:
		return semconv.ErrorType(err)
	}
}

// destination names where m is published, for an error.
func destination(m Publishing) string {
	return fmt.Sprintf("exchange %q with routing key %q", m.Exchange, m.RoutingKey)
}

// take waits for a publish slot, and returns an idle channel, or a new one
// when none is idle, connecting first when there is no open connection.
func (p *Publisher) take(ctx context.Context, m Publishing) (*confirmChannel, error) {
	select {
	case p.slots <- struct{}{}:
	case <-p.stopped.Done():
		return nil, errStopped
	case <-ctx.Done():
		return nil, fmt.Errorf("rabbitmq: waiting for a channel to publish to %s: %w", destination(m), ctx.Err())
	}
	if cc := p.takeIdle(); cc != nil {
		return cc, nil
	}
	cc, err := p.open(ctx)
	if err != nil {
		<-p.slots
		return nil, err
	}
	return cc, nil
}

// takeIdle returns an idle channel that is still open, or nil when there is
// none. It closes every idle channel it finds closed, or on a connection
// that is.
func (p *Publisher) takeIdle() *confirmChannel {
	p.idleMu.Lock()
	defer p.idleMu.Unlock()
	for len(p.idle) > 0 {
		cc := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		if cc.open() {
			return cc
		}
		discard(cc)
	}
	return nil
}

// give returns cc's slot, and cc to the idle channels when reuse is true.
// Otherwise it closes cc.
func (p *Publisher) give(cc *confirmChannel, reuse bool) {
	if reuse {
		p.idleMu.Lock()
		p.idle = append(p.idle, cc)
		p.idleMu.Unlock()
	} else {
		discard(cc)
	}
	<-p.slots
}

// discard closes cc on a goroutine of its own: amqp091 waits for the
// broker's channel.close-ok, which a broker that stopped answering never
// sends, and a closed connection ends the wait.
func discard(cc *confirmChannel) {
	go func() { _ = cc.ch.Close() }()
}

// open opens a channel in confirm mode, connecting first when there is no
// open connection.
func (p *Publisher) open(ctx context.Context) (*confirmChannel, error) {
	conn, err := p.connection(ctx)
	if err != nil {
		return nil, err
	}
	type opened struct {
		cc  *confirmChannel
		err error
	}
	// amqp091 waits for the broker's channel.open-ok and confirm.select-ok
	// without a context, so the wait is on a goroutine of its own, and ctx
	// ends it. A channel opened after ctx ended is closed.
	done := make(chan opened, 1)
	go func() {
		cc, err := openConfirmChannel(conn)
		done <- opened{cc, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			err := fmt.Errorf("rabbitmq: opening a channel to %s: %w", p.settings.Connection.Addr, o.err)
			if conn.IsClosed() {
				// The connection was lost: the next attempt connects again.
				return nil, lost(err)
			}
			return nil, err
		}
		return o.cc, nil
	case <-ctx.Done():
		go func() {
			if o := <-done; o.cc != nil {
				_ = o.cc.ch.Close()
			}
		}()
		return nil, fmt.Errorf("rabbitmq: opening a channel to %s: %w", p.settings.Connection.Addr, ctx.Err())
	}
}

// openConfirmChannel opens a channel on conn, listens for its confirms,
// returns and close, and puts it in confirm mode.
//
// Each listener has room for one value, and a channel carries one publish
// at a time, so amqp091 never waits on a listener.
func openConfirmChannel(conn publishConn) (*confirmChannel, error) {
	ch, err := conn.OpenChannel()
	if err != nil {
		return nil, err
	}
	cc := &confirmChannel{
		ch:       ch,
		conn:     conn,
		closes:   ch.NotifyClose(make(chan *amqp.Error, 1)),
		returns:  ch.NotifyReturn(make(chan amqp.Return, 1)),
		confirms: ch.NotifyPublish(make(chan amqp.Confirmation, 1)),
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("putting it in confirm mode: %w", err)
	}
	return cc, nil
}

// connection returns the open connection, connecting first when there is
// none: at first, and after the connection was lost. Concurrent calls
// connect once.
func (p *Publisher) connection(ctx context.Context) (publishConn, error) {
	select {
	case p.connLock <- struct{}{}:
	case <-p.stopped.Done():
		return nil, errStopped
	case <-ctx.Done():
		return nil, fmt.Errorf("rabbitmq: waiting to connect to %s: %w", p.settings.Connection.Addr, ctx.Err())
	}
	defer func() { <-p.connLock }()
	if p.stopped.Err() != nil {
		return nil, errStopped
	}
	if p.conn != nil && !p.conn.IsClosed() {
		return p.conn, nil
	}

	// The OnStop function ends a dial in progress, so it doesn't wait for
	// it.
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	unregister := context.AfterFunc(p.stopped, cancel)
	defer unregister()
	conn, err := p.dial(dialCtx)
	if p.stopped.Err() != nil {
		if err == nil {
			_ = conn.CloseDeadline(time.Time{})
		}
		return nil, errStopped
	}
	if err != nil {
		if ctx.Err() != nil {
			// The Publish ended, not the dial: classify sees ctx's error.
			return nil, err
		}
		return nil, lost(err)
	}
	p.conn = conn
	return conn, nil
}

// open reports whether cc and its connection are still open.
func (cc *confirmChannel) open() bool {
	return !cc.conn.IsClosed() && !cc.ch.IsClosed()
}

// publish publishes msg, which is m as amqp091 publishes it, on cc with
// mandatory set, and waits for the broker's confirm, or for ctx. It returns
// whether cc can carry another publish, and the publish's error.
//
// amqp091 hands a basic.return to the channel's listener before it hands
// over the confirm that follows it, so a return that is waiting once the
// confirm arrives belongs to this publish.
func (cc *confirmChannel) publish(ctx context.Context, m Publishing, msg amqp.Publishing) (reuse bool, err error) {
	dest := destination(m)
	if err := cc.ch.PublishWithContext(ctx, m.Exchange, m.RoutingKey, true, false, msg); err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("rabbitmq: publishing to %s: %w", dest, ctx.Err())
		}
		// The broker never had m: its channel or connection closed first.
		return false, lost(fmt.Errorf("rabbitmq: publishing to %s: %w", dest, err))
	}
	select {
	case c, ok := <-cc.confirms:
		if !ok {
			return false, cc.closed(dest)
		}
		select {
		case r := <-cc.returns:
			return true, fmt.Errorf("rabbitmq: publishing to %s: %w: %d %s", dest, ErrUnroutable, r.ReplyCode, r.ReplyText)
		default:
		}
		if !c.Ack {
			return true, fmt.Errorf("rabbitmq: publishing to %s: %w", dest, errNacked)
		}
		return true, nil
	case e, ok := <-cc.closes:
		return false, cc.closedError(dest, e, ok)
	case <-ctx.Done():
		// A confirm may still arrive on the channel, so it is not reused.
		return false, fmt.Errorf("rabbitmq: waiting for the broker to confirm the message published to %s: %w", dest, ctx.Err())
	}
}

// closed returns the error cc closed with, once its confirms have ended.
// amqp091 hands the error to the close listener before it ends the
// confirms, so the error is waiting if there is one.
func (cc *confirmChannel) closed(dest string) error {
	select {
	case e, ok := <-cc.closes:
		return cc.closedError(dest, e, ok)
	default:
		return cc.closedError(dest, nil, false)
	}
}

// closedError is the error of a publish whose channel closed with e: the
// broker's error, either the channel's or the lost connection's, or
// amqp.ErrClosed when the channel closed without one.
//
// Only an error the broker closed the channel with, on a connection still
// open, is a channel error. The rest are marked lost: amqp091 marks a
// connection closed before it closes its channels with the connection's
// error, so a lost connection is seen as one here.
func (cc *confirmChannel) closedError(dest string, e *amqp.Error, ok bool) error {
	if ok && e != nil {
		err := fmt.Errorf("rabbitmq: publishing to %s: %w", dest, e)
		if cc.conn.IsClosed() {
			return lost(err)
		}
		return err
	}
	return lost(fmt.Errorf("rabbitmq: publishing to %s: %w", dest, amqp.ErrClosed))
}
