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
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/queue"
)

// scopeName is the instrumentation scope of the package's own logs.
const scopeName = "github.com/z5labs/humus/queue/rabbitmq"

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

	// serverAddr and serverPort are Connection.Addr's host and port.
	serverAddr string
	serverPort int

	// stopped is done once Shutdown has been called.
	stopped context.Context
	stop    context.CancelFunc

	// serving is set once: by the first Serve, to that Serve's call, or by
	// a Shutdown called before any Serve, to notServed.
	serving atomic.Pointer[call]
}

// call is one call of Serve: the function that cancels the context the
// deliveries are processed with, and served, which is closed when Serve
// returns.
type call struct {
	abort  context.CancelFunc
	served chan struct{}
}

// notServed is the call of a Service that was shut down before it was
// served.
var notServed = &call{}

// NewService checks settings and returns a Service that consumes
// settings.Queue and hands each delivery to p. It opens no connection: Serve
// connects.
//
// The Service logs through s.Telemetry's LoggerProvider, under the scope
// github.com/z5labs/humus/queue/rabbitmq, and reaches the broker only
// through s.Host.
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
	// CheckAddr has accepted Addr, so it splits, and its port is a number.
	host, port, _ := net.SplitHostPort(settings.Connection.Addr)
	portNum, _ := strconv.Atoi(port)

	stopped, stop := context.WithCancel(context.Background())
	svc := &Service{
		settings:   settings,
		proc:       p,
		retrier:    retrier,
		logger:     s.Telemetry.LoggerProvider.Logger(scopeName),
		serverAddr: host,
		serverPort: portNum,
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
	c := settings.Connection
	check("Settings.Connection.Addr", CheckAddr(c.Addr))
	check("Settings.Connection.Vhost", CheckVhost(c.Vhost))
	check("Settings.Connection.SASL", CheckSASL(c.SASL))
	check("Settings.Connection.Heartbeat", CheckHeartbeat(c.Heartbeat))
	check("Settings.Connection.ConnectTimeout", CheckConnectTimeout(c.ConnectTimeout))
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
// It returns nil when it stopped because ctx was done or Shutdown was
// called, including when Shutdown's context ran out first. It returns an
// error when the broker refused the login, the vhost or the queue, when a
// Processor's error under queue.AtLeastOnce stopped it, and when the
// connection was lost.
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
	s, err := svc.connect(running, logCtx)
	if err != nil || s == nil {
		return err
	}
	return s.consume(running, work, logCtx)
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
var errLost = errors.New("the broker stopped the consumer")

// consume hands the session's deliveries to Settings.Concurrency workers
// until running is done, a delivery fails under AtLeastOnce, or the
// connection is lost, and then stops:
//
//  1. it cancels the consumer;
//  2. it hands out no more deliveries, except the rest of an AtMostOnce
//     batch it already acked;
//  3. it waits for the deliveries in flight, which the workers ack;
//  4. it nacks with requeue, in one basic.nack with multiple set, every
//     delivery it received but didn't process;
//  5. it closes the channel and the connection.
//
// When work is cancelled, by Shutdown's context, the connection is closed
// at once.
func (s *session) consume(running, work, logCtx context.Context) error {
	svc := s.svc
	stopClosing := context.AfterFunc(work, func() { _ = s.conn.Close() })
	defer stopClosing()

	s.work = work
	s.stopping, s.stopNow = context.WithCancel(running)
	defer s.stopNow()

	svc.emit(logCtx, log.SeverityInfo, "consuming", semconv.MessagingDestinationName(svc.settings.Queue))

	jobs := make(chan amqp.Delivery)
	var wg sync.WaitGroup
	for range svc.settings.Concurrency {
		wg.Go(func() { s.worker(jobs, logCtx) })
	}

	var lost bool
	if svc.settings.Delivery == queue.AtMostOnce {
		lost = s.dispatchAtMostOnce(jobs)
	} else {
		lost = s.dispatchAtLeastOnce(jobs)
	}
	if !lost {
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
		if !lost && s.received > s.settled.Load() {
			// Every delivery handed out has been acked or rejected, so
			// this requeues the rest, without counting it against any of
			// them.
			_ = s.ch.Nack(s.last, true, true)
		}
		_ = s.ch.Close()
	}
	_ = s.conn.Close()

	if err := s.failed(); err != nil {
		return err
	}
	if lost && running.Err() == nil && work.Err() == nil {
		cause := errLost
		if s.lostErr != nil {
			cause = s.lostErr
		} else {
			select {
			case e := <-s.closes:
				if e != nil {
					cause = e
				}
			default:
			}
		}
		return fmt.Errorf("rabbitmq: lost the connection consuming %s: %w", svc.settings.Queue, cause)
	}
	return nil
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
			if err := s.process(d); err != nil && s.work.Err() == nil {
				svc.logProcessingError(logCtx, d, err)
			}
			continue
		}
		if s.stopping.Err() != nil {
			// Handed out as the session stopped: nacked with the rest.
			continue
		}
		err := s.process(d)
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

// process hands d to the Processor, with the context of processing. No
// panic is recovered.
func (s *session) process(d amqp.Delivery) error {
	return s.svc.proc.Process(s.work, newMessage(s.svc.settings.Queue, d))
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

// logProcessingError emits an ERROR record for the error processing d
// returned under queue.AtMostOnce.
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
