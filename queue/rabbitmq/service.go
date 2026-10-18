// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/semconv/v1.43.0/messagingconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/queue"
)

// scopeName is the instrumentation scope of the package's own spans,
// instruments and logs.
const scopeName = "github.com/z5labs/humus/queue/rabbitmq"

// processOperation is the messaging.operation.name of processing a
// delivery.
const processOperation = "process"

// consumerTag is the tag of every consumer the Service starts. Consumer tags
// are scoped to a channel, and the Service starts one consumer on each
// channel it opens, so a fixed tag never collides, and amqp091 never builds
// one from os.Args[0].
const consumerTag = "humus"

// The schedule of the attempts to connect: exponential backoff with full
// jitter, from 100ms up to 30s.
const (
	connectBackoffBase = 100 * time.Millisecond
	connectBackoffMax  = 30 * time.Second
)

// channel is what the Service uses of an AMQP channel. *amqp.Channel
// satisfies it, and a unit test replaces it with a stand-in.
type channel interface {
	Qos(prefetchCount, prefetchSize int, global bool) error
	ConsumeWithContext(ctx context.Context, queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error)
	Ack(tag uint64, multiple bool) error
	Nack(tag uint64, multiple, requeue bool) error
	Reject(tag uint64, requeue bool) error
	Cancel(consumer string, noWait bool) error
	NotifyClose(c chan *amqp.Error) chan *amqp.Error
	Close() error
}

var _ channel = (*amqp.Channel)(nil)

// openFunc opens a connection to the broker and a channel on it. Closing
// the connection closes the channel.
type openFunc func(ctx context.Context) (channel, io.Closer, error)

// Service consumes Settings.Queue and hands each delivery to a Processor. It
// is safe for concurrent use.
type Service struct {
	settings Settings
	proc     queue.Processor[Message]
	open     openFunc
	retrier  *retry.Retrier
	logger   log.Logger

	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	consumed   messagingconv.ClientConsumedMessages
	processing messagingconv.ProcessDuration

	// serverAddr and serverPort are Connection.Addr's host and port.
	serverAddr string
	serverPort int

	// faults is the App's failure injection, from the Setup.
	faults fault.Injector

	// stopped is done once Shutdown has been called.
	stopped context.Context
	stop    context.CancelFunc

	// serving is set once: by the first Serve, to that Serve's call, or by
	// a Shutdown called before any Serve, to notServed.
	serving atomic.Pointer[call]
}

// call is one call of Serve: the function that cancels the context the
// deliveries are processed with, served, which is closed when Serve
// returns, and consuming, which is set once the broker first answers the
// Service's basic.consume.
type call struct {
	abort     context.CancelFunc
	served    chan struct{}
	consuming atomic.Bool
}

// notServed is the call of a Service that was shut down before it was
// served.
var notServed = &call{}

// NewService checks settings and returns a Service that consumes
// settings.Queue and hands each delivery to p. It opens no connection: Serve
// connects.
//
// The Service reports through s.Telemetry, and never through
// OpenTelemetry's globals, under the scope
// github.com/z5labs/humus/queue/rabbitmq: its logs, a span for every
// delivery, and two instruments. It reaches the broker only through s.Host.
//
// A delivery's span is named "process <queue>", of kind CONSUMER, and
// Process is given its context. Its parent is the trace context
// s.Telemetry.Propagator extracts from the message's string headers; a
// message whose headers carry none starts a new trace. Its attributes are
// messaging.system (rabbitmq), messaging.operation.type and
// messaging.operation.name (both process), messaging.destination.name
// ({exchange}:{routing key}:{queue}, leaving out what is empty, and the
// routing key when it equals the queue's name),
// messaging.rabbitmq.destination.routing_key when it isn't empty,
// messaging.rabbitmq.message.delivery_tag, messaging.message.id and
// messaging.message.conversation_id when they are set, server.address and
// server.port. A processing error is recorded on it, with error.type and an
// error status. The body is never recorded.
//
// The instruments are messaging.client.consumed.messages, which counts the
// deliveries handed to p, and messaging.process.duration, how long each
// Process took, with error.type when it failed. Each has messaging.system,
// messaging.operation.name, messaging.destination.name, server.address and
// server.port. An ack has no reply, so it has no duration, and none is
// recorded. Since the destination name includes the routing key, a
// publisher that puts identifiers in its routing keys gives both
// instruments a series per key.
//
// It reports every field of settings that fails its check in one error that
// names each one, including a Concurrency above Prefetch, and it rejects a
// nil Processor. It opens no connection and registers no OnStop function,
// so an Init that fails after it leaks nothing.
func NewService(s *humus.Setup, settings Settings, p queue.Processor[Message]) (*Service, error) {
	if err := checkSettings(settings, p); err != nil {
		return nil, fmt.Errorf("rabbitmq: NewService: %w", err)
	}
	schedule, err := retry.Exponential(connectBackoffBase, connectBackoffMax)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: NewService: %w", err)
	}
	// No Budget and math.MaxInt attempts: only Serve's context ends the
	// attempts to connect.
	retrier, err := retry.New(math.MaxInt, schedule, classifyConnect, nil)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: NewService: %w", err)
	}
	meter := s.Telemetry.MeterProvider.Meter(scopeName)
	consumed, err1 := messagingconv.NewClientConsumedMessages(meter)
	processing, err2 := messagingconv.NewProcessDuration(meter)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("rabbitmq: NewService: creating the instruments: %w", err)
	}
	// CheckAddr has accepted Addr, so it splits, and its port is a number.
	host, port, _ := net.SplitHostPort(settings.Connection.Addr)
	portNum, _ := strconv.Atoi(port)

	stopped, stop := context.WithCancel(context.Background())
	svc := &Service{
		settings:   settings,
		proc:       p,
		retrier:    retrier,
		logger:     s.Telemetry.LoggerProvider.Logger(scopeName),
		tracer:     s.Telemetry.TracerProvider.Tracer(scopeName),
		propagator: s.Telemetry.Propagator,
		consumed:   consumed,
		processing: processing,
		serverAddr: host,
		serverPort: portNum,
		faults:     s.Faults,
		stopped:    stopped,
		stop:       stop,
	}
	svc.open = func(ctx context.Context) (channel, io.Closer, error) {
		return openChannel(ctx, s.Host, settings.Connection)
	}
	return svc, nil
}

// checkSettings runs every check on settings, and rejects a Concurrency above
// Prefetch and a nil Processor. It reports every failure in one error, each
// naming its field.
func checkSettings(settings Settings, p queue.Processor[Message]) error {
	var errs []error
	check := func(field string, err error) bool {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
			return false
		}
		return true
	}
	errs = append(errs, checkConnection("Settings.Connection", settings.Connection)...)
	check("Settings.Queue", CheckQueue(settings.Queue))
	check("Settings.Delivery", queue.CheckDelivery(settings.Delivery))
	prefetch := check("Settings.Prefetch", CheckPrefetch(settings.Prefetch))
	concurrency := check("Settings.Concurrency", CheckConcurrency(settings.Concurrency))
	if prefetch && concurrency && settings.Concurrency > settings.Prefetch {
		errs = append(errs, fmt.Errorf("Settings.Concurrency, %d, must be at most Settings.Prefetch, %d",
			settings.Concurrency, settings.Prefetch))
	}
	if p == nil {
		errs = append(errs, errors.New("the Processor must not be nil"))
	}
	return errors.Join(errs...)
}

// checkConnection runs every check on c, and returns an error for each
// field that fails its check, named under prefix.
func checkConnection(prefix string, c Connection) []error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s %w", prefix, field, err))
		}
	}
	check(".Addr", CheckAddr(c.Addr))
	check(".Vhost", CheckVhost(c.Vhost))
	check(".SASL", CheckSASL(c.SASL))
	check(".Heartbeat", CheckHeartbeat(c.Heartbeat))
	check(".ConnectTimeout", CheckConnectTimeout(c.ConnectTimeout))
	return errs
}

// openChannel connects to the broker through host and opens a channel. The
// connection is closed if ctx ends while the channel is being opened, or if
// opening it fails.
func openChannel(ctx context.Context, host humus.Host, c Connection) (channel, io.Closer, error) {
	conn, err := connect(ctx, host, c)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("rabbitmq: opening a channel to %s: %w", c.Addr, err)
	}
	return ch, conn, nil
}

// classifyConnect is the Retrier's classifier of an attempt to connect and
// consume: success, a refusal that stops the Service, or anything else,
// which a later attempt may fix.
func classifyConnect(_ context.Context, err error) (retry.Outcome, time.Duration) {
	switch {
	case err == nil:
		return retry.Success, 0
	case stops(err):
		return retry.Stop, 0
	default:
		return retry.Retry, 0
	}
}

// stops reports whether err is one the broker won't change its mind about,
// so connecting again won't fix it, and the Service stops:
//
//   - the login or the vhost refused: ACCESS_REFUSED, or amqp091's
//     ErrCredentials, ErrSASL or ErrVhost;
//   - the queue refused: 404 NOT_FOUND, 403 ACCESS_REFUSED, or 405
//     RESOURCE_LOCKED for a queue another connection consumes exclusively.
//
// Every other error, such as a refused dial, a timeout, or a connection the
// broker closed with 320 CONNECTION_FORCED, is one a later attempt may fix.
func stops(err error) bool {
	if errors.Is(err, amqp.ErrCredentials) || errors.Is(err, amqp.ErrSASL) || errors.Is(err, amqp.ErrVhost) {
		return true
	}
	ae, ok := errors.AsType[*amqp.Error](err)
	if !ok {
		return false
	}
	switch ae.Code {
	case amqp.AccessRefused, amqp.NotFound, amqp.ResourceLocked:
		return true
	default:
		return false
	}
}

// stopsConsuming reports whether lost, the error a channel closed with
// after consuming started, is one connecting again won't fix: one of the
// refusals stops reports, or 406 PRECONDITION_FAILED, which the broker
// closes a channel with when, for one, a delivery was held unacked past its
// consumer_timeout.
func stopsConsuming(lost error) bool {
	if stops(lost) {
		return true
	}
	ae, ok := errors.AsType[*amqp.Error](lost)
	return ok && ae.Code == amqp.PreconditionFailed
}

// Serve connects to the broker and consumes Settings.Queue until ctx is
// done, Shutdown is called, or the Service fails. It then cancels its
// consumer, lets the deliveries in flight finish, acks them, nacks with
// requeue every delivery it received but didn't process, closes the channel
// and the connection, and returns.
//
// Serve connects within a retry.Retrier: a failed attempt is logged at WARN,
// and the next follows after an exponential backoff of up to 30s, for as
// long as ctx allows, so a broker that is down at startup is waited for. A
// login, vhost or queue the broker refuses ends Serve with the broker's
// error.
//
// A connection lost after consuming started, or a consumer the broker
// cancelled, is logged at WARN, and Serve connects again in the same way,
// once the deliveries in flight have finished. Their acks fail, and the
// broker delivers them again. A channel the broker closes with 406
// PRECONDITION_FAILED, or with a refusal of the login, the vhost or the
// queue, ends Serve with the broker's error.
//
// It returns nil when it stopped because ctx was done or Shutdown was
// called, including when Shutdown's context ran out first, and while it was
// connecting again. It returns an error when the broker refused the login,
// the vhost or the queue, when it closed the channel with 406
// PRECONDITION_FAILED, and when a Processor's error under queue.AtLeastOnce
// stopped it.
//
// A Service is served once. A Serve called after Shutdown, or after an
// earlier Serve, returns nil at once.
func (svc *Service) Serve(ctx context.Context) error {
	// work is the context deliveries are processed with: it keeps ctx's
	// values but not its cancellation, and only Shutdown cancels it, when
	// its own context is done.
	work, abort := context.WithCancel(context.WithoutCancel(ctx))
	defer abort()
	c := &call{abort: abort, served: make(chan struct{})}
	if !svc.serving.CompareAndSwap(nil, c) {
		return nil
	}
	defer close(c.served)

	// running is done once ctx is, or Shutdown is called.
	running, stopRunning := context.WithCancel(ctx)
	defer stopRunning()
	unregister := context.AfterFunc(svc.stopped, stopRunning)
	defer unregister()

	logCtx := context.WithoutCancel(ctx)
	for {
		s, err := svc.connect(running, logCtx)
		if err != nil || s == nil {
			return err
		}
		// The broker has answered the basic.consume: the Service has
		// consumed, and stays so until Serve returns.
		c.consuming.Store(true)
		lost, err := s.consume(running, work, logCtx)
		switch {
		case err != nil:
			return err
		case lost == nil:
			return nil
		case stopsConsuming(lost):
			return fmt.Errorf("rabbitmq: the broker stopped consuming %s: %w", svc.settings.Queue, lost)
		}
	}
}

// connect makes attempts to connect and consume until one succeeds, the
// broker refuses the login, the vhost or the queue, or ctx is done. It
// returns a nil session and a nil error when ctx ended first.
func (svc *Service) connect(ctx, logCtx context.Context) (*session, error) {
	var (
		s   *session
		err error
	)
	for a := range svc.retrier.Attempts(ctx) {
		s, err = svc.start(ctx)
		if a.Done(err) == retry.Again {
			svc.emit(logCtx, log.SeverityWarn, "connecting failed",
				semconv.ErrorType(err),
				semconv.ExceptionMessage(err.Error()),
			)
		}
	}
	switch {
	case err == nil:
		return s, nil
	case ctx.Err() != nil:
		return nil, nil
	default:
		return nil, err
	}
}

// start opens a connection and a channel, sets the channel's prefetch count
// and consumes Settings.Queue on it. Until it returns, the connection is
// closed if ctx ends, so a broker that stops answering can't hold up
// shutdown. On any failure the connection is closed.
func (svc *Service) start(ctx context.Context) (*session, error) {
	ch, conn, err := svc.open(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	s, err := svc.consumeOn(ctx, ch, conn)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s, nil
}

// consumeOn sets ch's prefetch count to Settings.Prefetch, and consumes
// Settings.Queue on ch with manual acks.
func (svc *Service) consumeOn(ctx context.Context, ch channel, conn io.Closer) (*session, error) {
	q := svc.settings.Queue
	// One error at most is sent, and the channel is closed after it, so a
	// buffer of one never blocks amqp091.
	closes := ch.NotifyClose(make(chan *amqp.Error, 1))
	if err := ch.Qos(svc.settings.Prefetch, 0, false); err != nil {
		return nil, fmt.Errorf("rabbitmq: setting the prefetch count to consume %s: %w", q, err)
	}
	// amqp091 cancels the consumer from a goroutine of its own once the
	// context it consumes with is done, and that basic.cancel would race the
	// Service's own synchronous calls on the channel. So it is given a
	// context that is never done: the Service cancels the consumer itself,
	// and closing the connection, which start does when ctx ends, stops a
	// consume the broker doesn't answer.
	deliveries, err := ch.ConsumeWithContext(context.WithoutCancel(ctx), q, consumerTag, false, false, false, false, nil)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: consuming %s: %w", q, err)
	}
	return &session{
		svc:        svc,
		ch:         ch,
		conn:       conn,
		deliveries: deliveries,
		closes:     closes,
	}, nil
}

// session is one connection the Service consumes on: its channel, the
// deliveries of its consumer, and what the deliveries have come to.
type session struct {
	svc        *Service
	ch         channel
	conn       io.Closer
	deliveries <-chan amqp.Delivery
	closes     chan *amqp.Error

	// stopping is done once the consumer is to stop: once Serve's context
	// is done, Shutdown is called, or a delivery failed under AtLeastOnce.
	stopping context.Context
	stopNow  context.CancelFunc

	// work is the context of processing, which only Shutdown cancels.
	work context.Context

	// spanBase is work with no span and no baggage: the context a
	// delivery's trace context is extracted into, so its span's parent is
	// only what its headers carry.
	spanBase context.Context

	// received counts the deliveries taken from the consumer, and last is
	// the tag of the last one. Only the dispatching goroutine uses them.
	received int64
	last     uint64

	// settled counts the deliveries acked, or rejected, by the Service.
	settled atomic.Int64

	// cancelled is set once the consumer has been cancelled. Only the
	// dispatching goroutine uses it.
	cancelled bool

	// lostErr is the error the channel closed with, when the connection was
	// lost. Only the dispatching goroutine uses it.
	lostErr *amqp.Error

	// failure is the first error of a Processor under AtLeastOnce.
	failMu  sync.Mutex
	failure error
}

// errLost is the cause of a lost connection the broker gave no reason for,
// such as the consumer cancelled by the broker because its queue was
// deleted.
var errLost = errors.New("the broker cancelled the consumer")

// consume hands the session's deliveries to Settings.Concurrency workers
// until running is done, a delivery fails under AtLeastOnce, or the
// connection is lost, and then stops. It returns the Processor's error under
// AtLeastOnce, or, when the connection was lost while running and work were
// live, the error the channel closed with, or errLost. It stops:
//
//  1. it cancels the consumer;
//  2. it hands out no more deliveries, except the rest of an AtMostOnce
//     batch it already acked;
//  3. it waits for the deliveries in flight, which the workers ack;
//  4. it nacks with requeue, in one basic.nack with multiple set, every
//     delivery it received but didn't process;
//  5. it closes the channel and the connection.
//
// A lost connection skips steps 1 and 4, and is logged at WARN at once,
// unless it ends the Service: the broker has requeued every delivery that
// wasn't acked, so the acks of the deliveries in flight fail, and are
// dropped.
//
// When work is cancelled, by Shutdown's context, the connection is closed
// at once.
func (s *session) consume(running, work, logCtx context.Context) (cause, err error) {
	svc := s.svc
	stopClosing := context.AfterFunc(work, func() { _ = s.conn.Close() })
	defer stopClosing()

	s.work = work
	s.spanBase = baggage.ContextWithoutBaggage(trace.ContextWithSpanContext(work, trace.SpanContext{}))
	s.stopping, s.stopNow = context.WithCancel(running)
	defer s.stopNow()

	svc.emit(logCtx, log.SeverityInfo, "consuming", semconv.MessagingDestinationName(svc.settings.Queue))

	jobs := make(chan amqp.Delivery)
	var wg sync.WaitGroup
	for range svc.settings.Concurrency {
		wg.Go(func() { s.worker(jobs, logCtx) })
	}

	var wasLost bool
	if svc.settings.Delivery == queue.AtMostOnce {
		wasLost = s.dispatchAtMostOnce(jobs)
	} else {
		wasLost = s.dispatchAtLeastOnce(jobs)
	}
	var lost error
	if wasLost {
		lost = s.lostCause()
		// Logged at once, rather than once the deliveries in flight are
		// done, unless it ends the Service, which returns it instead.
		if running.Err() == nil && work.Err() == nil && !stopsConsuming(lost) {
			svc.logLost(logCtx, lost)
		}
	} else {
		s.cancel()
	}
	close(jobs)
	wg.Wait()

	// The deliveries the consumer had buffered: once it is cancelled, or
	// the channel closed, the consumer closes its deliveries after them.
	for d := range s.deliveries {
		s.received++
		s.last = d.DeliveryTag
	}
	// Once Shutdown's context is done, the connection is closed at once,
	// and the broker requeues every delivery that wasn't acked.
	if work.Err() == nil {
		if !wasLost && s.received > s.settled.Load() {
			// Every delivery handed out has been acked or rejected, so
			// this requeues the rest, without counting it against any of
			// them.
			_ = s.ch.Nack(s.last, true, true)
		}
		_ = s.ch.Close()
	}
	_ = s.conn.Close()

	if err := s.failed(); err != nil {
		return nil, err
	}
	if !wasLost || running.Err() != nil || work.Err() != nil {
		return nil, nil
	}
	return lost, nil
}

// lostCause returns the error the channel closed with, when the connection
// was lost, or errLost when the broker gave none, as when it cancelled the
// consumer. amqp091 sends the error to the channel's NotifyClose listeners
// before it ends the consumer's deliveries, so the error is there by the
// time the deliveries end.
func (s *session) lostCause() error {
	if s.lostErr != nil {
		return s.lostErr
	}
	select {
	case e := <-s.closes:
		if e != nil {
			return e
		}
	default:
	}
	return errLost
}

// cancel cancels the consumer, once. It is called only from the dispatching
// goroutine, which makes every synchronous call on the channel: amqp091's
// replies are matched to its calls in order, so two at once could take each
// other's.
func (s *session) cancel() {
	if s.cancelled {
		return
	}
	s.cancelled = true
	if err := s.ch.Cancel(consumerTag, false); err != nil {
		// A cancel fails because the channel is closed, which stops the
		// consumer as well. Closing it makes sure of that, so the
		// consumer's deliveries end.
		_ = s.ch.Close()
	}
}

// receive takes the next delivery from the consumer. ok is false when the
// session is stopping, and lost is true when the consumer stopped without
// being asked to: the connection, or the channel, was lost.
func (s *session) receive() (d amqp.Delivery, ok, lost bool) {
	select {
	case <-s.stopping.Done():
		return d, false, false
	case e, open := <-s.closes:
		return d, false, s.lose(e, open)
	case d, ok := <-s.deliveries:
		if !ok {
			return d, false, s.stopping.Err() == nil
		}
		s.received++
		s.last = d.DeliveryTag
		// A delivery taken once the channel has closed is not handed out:
		// the broker requeued it with the channel, and delivers it again.
		select {
		case e, open := <-s.closes:
			return d, false, s.lose(e, open)
		default:
		}
		// A delivery taken as the session stops is not handed out.
		return d, s.stopping.Err() == nil, false
	}
}

// lose records the error the channel closed with, if open, and reports
// whether the channel closing lost the connection: whether the session
// wasn't stopping already.
func (s *session) lose(e *amqp.Error, open bool) bool {
	if open && e != nil {
		s.lostErr = e
	}
	return s.stopping.Err() == nil
}

// dispatchAtLeastOnce hands each delivery to a worker, in delivery order,
// until the session stops. It reports whether the connection was lost.
func (s *session) dispatchAtLeastOnce(jobs chan<- amqp.Delivery) (lost bool) {
	for {
		d, ok, lost := s.receive()
		if !ok {
			return lost
		}
		select {
		case jobs <- d:
		case <-s.stopping.Done():
			return false
		case e, open := <-s.closes:
			// The broker requeued d with the connection.
			return s.lose(e, open)
		}
	}
}

// dispatchAtMostOnce takes a delivery and every delivery already buffered
// behind it, up to Settings.Prefetch, acks them with one basic.ack with
// multiple set, and waits for the reply to a basic.qos, which the broker
// sends only after it has handled the ack. Only then does it hand the batch
// to the workers, in delivery order: all of it, even when the session stops
// meanwhile, since it is acked, unless Shutdown's context ends first. It
// reports whether the connection was lost.
func (s *session) dispatchAtMostOnce(jobs chan<- amqp.Delivery) (lost bool) {
	prefetch := s.svc.settings.Prefetch
	for {
		d, ok, lost := s.receive()
		if !ok {
			return lost
		}
		batch := s.buffered(d, prefetch)
		if s.stopping.Err() != nil {
			// Not acked, so not handed out: it is nacked with the rest.
			return false
		}
		last := batch[len(batch)-1].DeliveryTag
		if err := s.ch.Ack(last, true); err != nil {
			// The channel is closed: the broker requeues the batch, or had
			// the ack and dropped it. Either way it is not processed.
			return s.stopping.Err() == nil
		}
		s.settled.Add(int64(len(batch)))
		if err := s.ch.Qos(prefetch, 0, false); err != nil {
			return s.stopping.Err() == nil
		}
		for _, d := range batch {
			select {
			case jobs <- d:
			case <-s.stopping.Done():
				// Stop the consumer now, rather than once the batch has
				// been handed out, and hand out the rest.
				s.cancel()
				select {
				case jobs <- d:
				case <-s.work.Done():
					return false
				}
			case <-s.work.Done():
				return false
			}
		}
	}
}

// buffered returns first, followed by every delivery already buffered behind
// it, up to limit in all.
//
// amqp091 hands deliveries over an unbuffered channel, fed by a goroutine
// of its own that buffers what the broker has sent. Having just handed one
// over, that goroutine has been readied but may not yet be waiting to hand
// over the next, so a receive that would block yields once, letting it run,
// before it takes the buffer to be empty.
func (s *session) buffered(first amqp.Delivery, limit int) []amqp.Delivery {
	batch := []amqp.Delivery{first}
	yielded := false
	for len(batch) < limit {
		select {
		case d, ok := <-s.deliveries:
			if !ok {
				return batch
			}
			s.received++
			s.last = d.DeliveryTag
			batch = append(batch, d)
			yielded = false
		default:
			if yielded {
				return batch
			}
			yielded = true
			runtime.Gosched()
		}
	}
	return batch
}

// worker processes the deliveries it is handed, one at a time, and settles
// each as Settings.Delivery says.
func (s *session) worker(jobs <-chan amqp.Delivery, logCtx context.Context) {
	svc := s.svc
	atMostOnce := svc.settings.Delivery == queue.AtMostOnce
	for d := range jobs {
		if atMostOnce {
			// The batch is acked already: every delivery of it is
			// processed, unless Shutdown's context has ended.
			if s.work.Err() != nil {
				continue
			}
			// process logs a failure itself, inside the delivery's span,
			// and processing goes on.
			_ = s.process(d, logCtx)
			continue
		}
		if s.stopping.Err() != nil {
			// Handed out as the session stopped: nacked with the rest.
			continue
		}
		err := s.process(d, logCtx)
		switch {
		case s.work.Err() != nil:
			// Shutdown's context cut processing short, and closed the
			// connection: the broker requeues the delivery.
		case err == nil:
			// An ack that fails fails because the channel is closed, and
			// the broker requeues the delivery.
			_ = s.ch.Ack(d.DeliveryTag, false)
			s.settled.Add(1)
		default:
			// The session stops before the reject, so the broker can't
			// hand the requeued delivery back to a worker of this
			// consumer: it is taken, if at all, as the session stops, and
			// nacked with the rest.
			s.fail(processingError(svc.settings.Queue, d, err))
			_ = s.ch.Reject(d.DeliveryTag, true)
			s.settled.Add(1)
		}
	}
}

// process hands d to the Processor, under the delivery's span, and records
// the delivery as consumed and how long processing it took. No panic is
// recovered.
//
// The span's parent is the trace context the propagator extracts from d's
// headers, whose baggage is on Process's context too, less a failure
// scenario Settings.TrustScenario doesn't trust. When the headers carry
// none, the span is the root of a new trace.
//
// Under queue.AtMostOnce, process logs a failure itself, with logCtx and
// the delivery's span context, unless Shutdown's context cut processing
// short.
func (s *session) process(d amqp.Delivery, logCtx context.Context) error {
	svc := s.svc
	headers := map[string]any(d.Headers)
	parent := svc.propagator.Extract(s.spanBase, (*headerCarrier)(&headers))

	attrs := svc.instrumentAttrs(destinationName(d.Exchange, d.RoutingKey, svc.settings.Queue))
	spanAttrs := append(attrs[:len(attrs):len(attrs)],
		semconv.MessagingSystemRabbitMQ,
		semconv.MessagingOperationTypeProcess,
		semconv.MessagingOperationName(processOperation),
		semconv.MessagingRabbitMQMessageDeliveryTag(deliveryTag(d.DeliveryTag)),
	)
	if d.RoutingKey != "" {
		spanAttrs = append(spanAttrs, semconv.MessagingRabbitMQDestinationRoutingKey(d.RoutingKey))
	}
	if d.MessageId != "" {
		spanAttrs = append(spanAttrs, semconv.MessagingMessageID(d.MessageId))
	}
	if d.CorrelationId != "" {
		spanAttrs = append(spanAttrs, semconv.MessagingMessageConversationID(d.CorrelationId))
	}
	ctx, span := svc.tracer.Start(parent, "process "+svc.settings.Queue,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(spanAttrs...),
	)
	defer span.End()

	m := newMessage(svc.settings.Queue, d)
	ctx = svc.strip(ctx, m)

	svc.consumed.Add(ctx, 1, processOperation, messagingconv.SystemRabbitMQ, attrs...)
	start := time.Now()
	err := svc.proc.Process(ctx, m)
	elapsed := time.Since(start).Seconds()

	if err != nil {
		errType := semconv.ErrorType(err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(errType)
		attrs = append(attrs, errType)
		if svc.settings.Delivery == queue.AtMostOnce && s.work.Err() == nil {
			svc.logProcessingError(trace.ContextWithSpan(logCtx, span), d, err)
		}
	}
	svc.processing.Record(ctx, elapsed, processOperation, messagingconv.SystemRabbitMQ, attrs...)
	return err
}

// instrumentAttrs returns the attributes both instruments record for a
// delivery to destination, beside messaging.system and
// messaging.operation.name: messaging.destination.name, server.address and
// server.port.
func (svc *Service) instrumentAttrs(destination string) []attribute.KeyValue {
	return []attribute.KeyValue{
		semconv.MessagingDestinationName(destination),
		semconv.ServerAddress(svc.serverAddr),
		semconv.ServerPort(svc.serverPort),
	}
}

// destinationName is a delivery's messaging.destination.name, as semconv
// v1.43.0 defines it for RabbitMQ: {exchange}:{routing key}:{queue}, leaving
// out what is empty, and the routing key when it equals the queue's name. A
// message published to the queue orders on the default exchange is orders.
func destinationName(exchange, routingKey, queue string) string {
	parts := make([]string, 0, 3)
	if exchange != "" {
		parts = append(parts, exchange)
	}
	if routingKey != "" && routingKey != queue {
		parts = append(parts, routingKey)
	}
	if queue != "" {
		parts = append(parts, queue)
	}
	return strings.Join(parts, ":")
}

// deliveryTag is tag as messaging.rabbitmq.message.delivery_tag records it.
// A channel numbers its deliveries from 1, so a tag never reaches
// math.MaxInt; one that did would be recorded as math.MaxInt.
func deliveryTag(tag uint64) int {
	if tag > math.MaxInt {
		return math.MaxInt
	}
	return int(tag)
}

// fail records err as the error Serve returns, if it is the first, and stops
// the session.
func (s *session) fail(err error) {
	s.failMu.Lock()
	if s.failure == nil {
		s.failure = err
	}
	s.failMu.Unlock()
	s.stopNow()
}

func (s *session) failed() error {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	return s.failure
}

// processingError is the error Serve returns when processing d, from queue,
// failed with err under AtLeastOnce. It names the queue, and the message's
// ID when it has one.
func processingError(queue string, d amqp.Delivery, err error) error {
	if d.MessageId != "" {
		return fmt.Errorf("rabbitmq: processing message %s from %s: %w", d.MessageId, queue, err)
	}
	return fmt.Errorf("rabbitmq: processing a message from %s: %w", queue, err)
}

// Consuming returns a monitor that is healthy once the Service has started
// consuming its queue: unhealthy until the broker first answers its
// basic.consume, and healthy from then until Serve returns. A lost
// connection after that doesn't make it unhealthy. It never returns an
// error. A consumer is made ready once it consumes with
//
//	health.Readiness(consumer.Consuming())
//
// It is for readiness only: restarting the pod doesn't bring the broker
// back, and an error that a restart would fix already ends Serve.
//
// Consuming may be called before Serve, in the end user's Init, and returns
// the same monitor each time. The monitor reports the Service's state when
// it is checked, and is safe for concurrent use.
func (svc *Service) Consuming() health.Monitor {
	return consumingMonitor{svc: svc}
}

// consumingMonitor is the monitor Consuming returns.
type consumingMonitor struct {
	svc *Service
}

// Healthy reports whether the Serve of the Service has consumed its queue
// and not yet returned. It never returns an error.
func (m consumingMonitor) Healthy(context.Context) (bool, error) {
	c := m.svc.serving.Load()
	if c == nil || c == notServed {
		return false, nil
	}
	select {
	case <-c.served:
		return false, nil
	default:
		return c.consuming.Load(), nil
	}
}

// Shutdown stops Serve and waits for it to return. Serve cancels its
// consumer, lets the deliveries in flight finish and acks them, nacks with
// requeue every delivery it received but didn't process, and closes the
// channel and the connection.
//
// If ctx is done first, or is done already, Shutdown cancels the context the
// deliveries in flight are processed with, closes the connection at once,
// waits for Serve to return, and returns an error that wraps ctx's error.
// The broker then requeues every delivery that wasn't acked.
//
// Shutdown returns nil at once when Serve was never called, after which
// Serve returns nil at once, and when Serve has already returned, so a
// second Shutdown returns nil.
func (svc *Service) Shutdown(ctx context.Context) error {
	svc.serving.CompareAndSwap(nil, notServed)
	svc.stop()
	c := svc.serving.Load()
	if c == notServed {
		return nil
	}
	select {
	case <-c.served:
		return nil
	default:
	}
	if ctx.Err() == nil {
		select {
		case <-c.served:
			return nil
		case <-ctx.Done():
		}
	}
	c.abort()
	<-c.served
	return fmt.Errorf("rabbitmq: shutting down the Service: %w", ctx.Err())
}

// logLost emits a WARN record for the connection lost with lost: with the
// AMQP reply code as error.type, and its reason as exception.message, when
// the broker gave one.
func (svc *Service) logLost(ctx context.Context, lost error) {
	attrs := []attribute.KeyValue{semconv.MessagingDestinationName(svc.settings.Queue)}
	if ae, ok := errors.AsType[*amqp.Error](lost); ok {
		attrs = append(attrs,
			semconv.ErrorTypeKey.String(strconv.Itoa(ae.Code)),
			semconv.ExceptionMessage(ae.Reason),
		)
	} else {
		attrs = append(attrs,
			semconv.ErrorType(lost),
			semconv.ExceptionMessage(lost.Error()),
		)
	}
	svc.emit(ctx, log.SeverityWarn, "lost the connection", attrs...)
}

// logProcessingError emits an ERROR record for the error processing d
// returned under queue.AtMostOnce, with ctx, which carries d's span.
func (svc *Service) logProcessingError(ctx context.Context, d amqp.Delivery, err error) {
	attrs := []attribute.KeyValue{
		semconv.MessagingDestinationName(svc.settings.Queue),
		semconv.ErrorType(err),
		semconv.ExceptionMessage(err.Error()),
	}
	if d.MessageId != "" {
		attrs = append(attrs, semconv.MessagingMessageID(d.MessageId))
	}
	svc.emit(ctx, log.SeverityError, "processing a message failed", attrs...)
}

// emit emits one record at sev, with ctx, body and attrs, and with the
// attributes every record of the Service has: messaging.system,
// server.address and server.port.
func (svc *Service) emit(ctx context.Context, sev log.Severity, body string, attrs ...attribute.KeyValue) {
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(sev.String())
	r.SetBody(attribute.StringValue(body))
	r.AddAttributes(
		semconv.MessagingSystemRabbitMQ,
		semconv.ServerAddress(svc.serverAddr),
		semconv.ServerPort(svc.serverPort),
	)
	r.AddAttributes(attrs...)
	svc.logger.Emit(ctx, r)
}
