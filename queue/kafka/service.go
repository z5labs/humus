// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/semconv/v1.43.0/messagingconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/queue"
)

// scopeName is the instrumentation scope of the package's own spans,
// metrics and logs.
const scopeName = "github.com/z5labs/humus/queue/kafka"

// The names of the operations the Service reports, as
// messaging.operation.name.
const (
	processOperation = "process"
	commitOperation  = "commit"
)

// partitionsKey is humus.messaging.destination.partition.ids, the attribute
// that lists the partitions of a topic in the records of an assignment, a
// revoke and a loss. OpenTelemetry's messaging conventions name one
// partition, and these records name several, so the name is humus's own
// and starts with humus.
const partitionsKey = attribute.Key("humus.messaging.destination.partition.ids")

// firstOffsetKey and lastOffsetKey are humus.messaging.kafka.offset.first
// and humus.messaging.kafka.offset.last, which bound the offsets of a batch
// that was skipped. OpenTelemetry's messaging conventions name one offset,
// so the names are humus's own and start with humus.
const (
	firstOffsetKey = attribute.Key("humus.messaging.kafka.offset.first")
	lastOffsetKey  = attribute.Key("humus.messaging.kafka.offset.last")
)

// Service consumes Settings.Topics as a member of the consumer group
// Settings.GroupID, and hands each record to a Processor. It is safe for
// concurrent use.
type Service struct {
	settings Settings
	proc     queue.Processor[Message]
	dial     dialFunc
	logger   log.Logger
	// kgoLogger is the Logger of franz-go's own logs.
	kgoLogger log.Logger

	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	consumed   messagingconv.ClientConsumedMessages
	processing messagingconv.ProcessDuration
	operations messagingconv.ClientOperationDuration
	// brokers is kotel's Meter: franz-go's broker metrics, as client hooks.
	brokers *kotel.Meter

	// stopped is done once Shutdown has been called.
	stopped context.Context
	stop    context.CancelFunc

	// serving is set once: by the first Serve, to that Serve's call, or by
	// a Shutdown called before any Serve, to notServed.
	serving atomic.Pointer[call]
}

// call is one call of Serve: the function that cancels the context of
// processing, committing and leaving the group, served, which is closed
// when Serve returns, and joined, which is set once franz-go first hands
// the member an assignment.
type call struct {
	abort  context.CancelFunc
	served chan struct{}
	joined atomic.Bool
}

// notServed is the call of a Service that was shut down before it was
// served.
var notServed = &call{}

// NewService checks settings and returns a Service. It opens no connection:
// Serve builds the client and joins the group.
//
// The Service reports through s.Telemetry: its spans, the three messaging
// instruments and its own logs under the scope
// github.com/z5labs/humus/queue/kafka, franz-go's logs under
// github.com/twmb/franz-go/pkg/kgo, and franz-go's broker metrics, from
// kotel's Meter, under github.com/twmb/franz-go/plugin/kotel.
//
// It reports every field of settings that fails its check in one error that
// names each one, and it rejects a nil Processor. It builds no client and
// registers no OnStop function, so an Init that fails after it leaks
// nothing.
func NewService(s *humus.Setup, settings Settings, p queue.Processor[Message]) (*Service, error) {
	if err := checkSettings(settings, p); err != nil {
		return nil, fmt.Errorf("kafka: NewService: %w", err)
	}
	meter := s.Telemetry.MeterProvider.Meter(scopeName)
	consumed, err1 := messagingconv.NewClientConsumedMessages(meter)
	processing, err2 := messagingconv.NewProcessDuration(meter)
	operations, err3 := messagingconv.NewClientOperationDuration(meter)
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, fmt.Errorf("kafka: NewService: creating the instruments: %w", err)
	}
	stopped, stop := context.WithCancel(context.Background())
	return &Service{
		settings:   settings,
		proc:       p,
		dial:       tlsDialer(s.Host.Dial, settings.TLS),
		logger:     s.Telemetry.LoggerProvider.Logger(scopeName),
		kgoLogger:  s.Telemetry.LoggerProvider.Logger(kgoScope),
		tracer:     s.Telemetry.TracerProvider.Tracer(scopeName),
		propagator: s.Telemetry.Propagator,
		consumed:   consumed,
		processing: processing,
		operations: operations,
		// kotel's Meter creates its instruments and starts nothing.
		brokers: kotel.NewMeter(kotel.MeterProvider(s.Telemetry.MeterProvider)),
		stopped: stopped,
		stop:    stop,
	}, nil
}

// checkSettings runs every check on settings, and rejects a nil Processor.
// It reports every failure in one error, each naming its field.
func checkSettings(settings Settings, p queue.Processor[Message]) error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
		}
	}
	check("Settings.Brokers", CheckBrokers(settings.Brokers))
	check("Settings.GroupID", CheckGroupID(settings.GroupID))
	check("Settings.Topics", CheckTopics(settings.Topics))
	check("Settings.Delivery", queue.CheckDelivery(settings.Delivery))
	check("Settings.SessionTimeout", CheckSessionTimeout(settings.SessionTimeout))
	check("Settings.RebalanceTimeout", CheckRebalanceTimeout(settings.RebalanceTimeout))
	check("Settings.FetchMaxBytes", CheckFetchMaxBytes(settings.FetchMaxBytes))
	check("Settings.MaxPollRecords", CheckMaxPollRecords(settings.MaxPollRecords))
	if p == nil {
		errs = append(errs, errors.New("the Processor must not be nil"))
	}
	return errors.Join(errs...)
}

// Serve joins the group and processes records until ctx is done, Shutdown
// is called, or the Service fails. It then stops polling, lets each
// partition's record in flight finish, commits, leaves the group, closes
// the client and returns.
//
// It returns nil when it stopped because ctx was done or Shutdown was
// called, including when Shutdown's context ran out first. It returns an
// error when processing a record failed under queue.AtLeastOnce, when a
// commit failed for a reason other than the member being fenced out of the
// group, and when a poll returned an error franz-go doesn't retry.
//
// A Service is served once. A Serve called after Shutdown, or after an
// earlier Serve, returns nil at once.
func (s *Service) Serve(ctx context.Context) error {
	// work is the context of processing, committing and leaving the group:
	// it keeps ctx's values but not its cancellation, and only Shutdown
	// cancels it, when its own context is done.
	work, abort := context.WithCancel(context.WithoutCancel(ctx))
	defer abort()
	c := &call{abort: abort, served: make(chan struct{})}
	if !s.serving.CompareAndSwap(nil, c) {
		return nil
	}
	defer close(c.served)

	polling, stopPolling := context.WithCancel(ctx)
	defer stopPolling()
	unregister := context.AfterFunc(s.stopped, stopPolling)
	defer unregister()

	g := &group{
		svc:  s,
		call: c,
		work: work,
		// A record's span has no parent but the one its headers carry,
		// whatever span Serve's context has.
		// So is its baggage: Process sees the producer's, or none.
		spanBase:    baggage.ContextWithoutBaggage(trace.ContextWithSpanContext(work, trace.SpanContext{})),
		logCtx:      context.WithoutCancel(ctx),
		stopPolling: stopPolling,
		parts:       make(map[topicPartition]*partition),
	}
	cl, err := kgo.NewClient(s.clientOpts(g)...)
	if err != nil {
		return fmt.Errorf("kafka: building the client: %w", err)
	}

	g.poll(polling, cl)

	g.stopAll()
	cl.AllowRebalance()
	// Leaving makes the group rebalance at once, so the other members take
	// the partitions without waiting for the session timeout. A leave that
	// fails, or that Shutdown's context cuts short, ends in the same place:
	// the coordinator drops the member when its session times out.
	_ = cl.LeaveGroupContext(work)
	cl.Close()
	return g.err()
}

// clientOpts returns the options of the client that consumes for g.
func (s *Service) clientOpts(g *group) []kgo.Opt {
	opts := []kgo.Opt{
		kgo.SeedBrokers(s.settings.Brokers...),
		kgo.Dialer(s.dial),
		kgo.ConsumerGroup(s.settings.GroupID),
		kgo.ConsumeTopics(s.settings.Topics...),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.SessionTimeout(s.settings.SessionTimeout),
		kgo.RebalanceTimeout(s.settings.RebalanceTimeout),
		kgo.FetchMaxBytes(s.settings.FetchMaxBytes),
		kgo.OnPartitionsAssigned(g.assigned),
		kgo.OnPartitionsRevoked(g.revoked),
		kgo.OnPartitionsLost(g.lost),
		kgo.WithHooks(connectHook{onRefusal: func(_ kgo.BrokerMetadata, err error) { g.fail(err) }}, s.brokers),
		kgo.WithLogger(kgoLogger{ctx: g.logCtx, logger: s.kgoLogger}),
	}
	if s.settings.SASL != nil {
		opts = append(opts, kgo.SASL(s.settings.SASL))
	}
	return opts
}

// Joined returns a monitor that is healthy once the Service has joined its
// consumer group: unhealthy until franz-go first hands the member an
// assignment, empty or not, and healthy from then until Serve returns. A
// broker outage after that doesn't make it unhealthy. It never returns an
// error. A consumer is made ready once it has joined with
//
//	health.Readiness(consumer.Joined())
//
// It is for readiness only: restarting the pod doesn't bring the brokers
// back, and an error that a restart would fix already ends Serve.
//
// Joined may be called before Serve, in the end user's Init. The monitor
// reports the Service's state when it is checked, and is safe for
// concurrent use.
func (s *Service) Joined() health.Monitor {
	return health.MonitorFunc(func(context.Context) (bool, error) {
		c := s.serving.Load()
		if c == nil || c == notServed {
			return false, nil
		}
		select {
		case <-c.served:
			return false, nil
		default:
			return c.joined.Load(), nil
		}
	})
}

// Shutdown stops Serve and waits for it to return. Serve stops polling, lets
// each partition's record in flight finish, commits and leaves the group.
//
// If ctx is done first, or is done already, Shutdown cancels the context the
// records in flight are processed with, and the leave, waits for Serve to
// return, and returns an error that wraps ctx's error. The records whose
// processing was cut short are not committed.
//
// Shutdown returns nil at once when Serve was never called, after which
// Serve returns nil at once, and when Serve has already returned, so a
// second Shutdown returns nil.
func (s *Service) Shutdown(ctx context.Context) error {
	s.serving.CompareAndSwap(nil, notServed)
	s.stop()
	c := s.serving.Load()
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
	return fmt.Errorf("kafka: shutting down the Service: %w", ctx.Err())
}

// topicPartition names one partition of a topic.
type topicPartition struct {
	topic     string
	partition int32
}

// partition is one assigned partition, whose records one goroutine, run,
// processes in offset order.
type partition struct {
	topicPartition

	// batches hands run the records of each poll. It holds one batch, so a
	// partition can have one batch waiting while it processes another.
	batches chan []*kgo.Record

	// quit is closed to stop run after its record in flight.
	quit     chan struct{}
	quitOnce sync.Once

	// done is closed when run has returned.
	done chan struct{}

	// lost is set before quit is closed when the partition was lost, so
	// run commits nothing more.
	lost atomic.Bool

	// member is the member ID the partition is held under: that of the
	// session that assigned it, and of each later session that kept it.
	// A commit franz-go would send under another member ID is not sent.
	member atomic.Pointer[string]

	// pending is the last record run processed and has not committed. Only
	// run uses it.
	pending *kgo.Record

	// attrs are the attributes of every measurement of the partition,
	// besides messaging.system and messaging.operation.name.
	attrs []attribute.KeyValue
}

func (p *partition) close() {
	p.quitOnce.Do(func() { close(p.quit) })
}

func (p *partition) quitting() bool {
	select {
	case <-p.quit:
		return true
	default:
		return false
	}
}

// group is one Serve's membership of the consumer group: the partitions it
// is assigned and the first error that ends it.
type group struct {
	svc *Service
	// call is the Serve the group belongs to.
	call *call
	work context.Context
	// spanBase is work with no span and no baggage: the context a record's
	// headers are extracted into.
	spanBase    context.Context
	logCtx      context.Context
	stopPolling context.CancelFunc

	mu    sync.Mutex
	parts map[topicPartition]*partition
	first error
	// closing is set by stopAll: a partition assigned after it, while the
	// group is being left, is never started, so nothing is left running
	// once Serve returns.
	closing bool
}

// poll polls until ctx is done or a poll returns an error franz-go doesn't
// retry, and hands each partition's records to that partition.
//
// The records are taken from the poll while rebalancing is blocked, so they
// all belong to partitions that are still assigned. They are handed out
// after rebalancing is allowed again, to the partitions as they were at the
// poll: a partition revoked in between has stopped, and its records are
// dropped, uncommitted, for its new owner to process.
func (g *group) poll(ctx context.Context, cl *kgo.Client) {
	type delivery struct {
		part    *partition
		records []*kgo.Record
	}
	for {
		fetches := cl.PollRecords(ctx, g.svc.settings.MaxPollRecords)
		if ctx.Err() != nil {
			cl.AllowRebalance()
			return
		}
		if err := fatalPollError(fetches); err != nil {
			cl.AllowRebalance()
			g.fail(err)
			return
		}
		var deliveries []delivery
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			if len(p.Records) == 0 {
				return
			}
			if part := g.partition(topicPartition{p.Topic, p.Partition}); part != nil {
				deliveries = append(deliveries, delivery{part: part, records: p.Records})
			}
		})
		cl.AllowRebalance()

		for _, d := range deliveries {
			select {
			case d.part.batches <- d.records:
			case <-d.part.done:
			case <-ctx.Done():
				return
			}
		}
	}
}

// fatalPollError returns the first error of fetches that ends the Service,
// or nil when there is none.
func fatalPollError(fetches kgo.Fetches) error {
	var fatal error
	fetches.EachError(func(topic string, partition int32, err error) {
		if fatal != nil || !isFatal(err) {
			return
		}
		if topic == "" {
			fatal = fmt.Errorf("kafka: consuming: %w", err)
			return
		}
		fatal = fmt.Errorf("kafka: fetching %s, partition %d: %w", topic, partition, err)
	})
	return fatal
}

// isFatal reports whether an error from a poll ends the Service: an error
// code from Kafka that franz-go doesn't retry, such as an authorization
// failure.
//
// Everything else is franz-go's to recover from. A connection error is
// retried, with backoff, which is how the Service keeps serving while the
// brokers are unreachable. A fenced-out member rejoins the group, and
// franz-go reports the partitions it held as lost or as revoked; either way
// the Service commits nothing more for them under the old member's claim. A
// cancelled context or a closed client is the Service stopping.
func isFatal(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, kgo.ErrClientClosed) {
		return false
	}
	ke, ok := errors.AsType[*kerr.Error](err)
	if !ok {
		return false
	}
	return !ke.Retriable && !notMember(err)
}

// connectHook calls onRefusal when a broker refuses the client's
// credentials, with the broker and an error that names it as host:port and
// wraps the refusal. franz-go retries every connection that fails, and it would
// retry a password that will never be accepted for as long as the client
// ran, without a poll or a produce ever returning the error. The Service
// stops on the refusal, and the Producer fails every call with it.
//
// onRefusal may be called more than once, once for each refused connection,
// and from franz-go's goroutines.
type connectHook struct {
	onRefusal func(meta kgo.BrokerMetadata, err error)
}

// OnBrokerConnect implements kgo.HookBrokerConnect.
func (h connectHook) OnBrokerConnect(meta kgo.BrokerMetadata, _ time.Duration, _ net.Conn, err error) {
	if err == nil || !refused(err) {
		return
	}
	h.onRefusal(meta, fmt.Errorf("kafka: authenticating to %s: %w", net.JoinHostPort(meta.Host, strconv.Itoa(int(meta.Port))), err))
}

// refused reports whether err is a broker refusing to authenticate the
// client.
func refused(err error) bool {
	return errors.Is(err, kerr.SaslAuthenticationFailed) ||
		errors.Is(err, kerr.UnsupportedSaslMechanism) ||
		errors.Is(err, kerr.IllegalSaslState)
}

// notMember reports whether err says the member is no longer in the group's
// current generation, or no longer holds the partition: it was fenced out,
// or the group is rebalancing. Kafka refused the commit, or, with
// errNotHeld, the Service did not send it. franz-go rejoins the group on
// it, so it is not the Service's failure.
func notMember(err error) bool {
	return errors.Is(err, errNotHeld) ||
		errors.Is(err, kerr.UnknownMemberID) ||
		errors.Is(err, kerr.IllegalGeneration) ||
		errors.Is(err, kerr.FencedInstanceID) ||
		errors.Is(err, kerr.RebalanceInProgress)
}

// errNotHeld is a commit the Service did not send, because franz-go would
// have sent it under a member ID the partition is not held under: the member
// was fenced out and rejoined the group as a new member. Kafka checks a
// commit's member ID and generation, but not who owns the partition, so it
// would have accepted the commit for a partition another member may own.
var errNotHeld = errors.New("the partition is not held under the group's current member ID")

// fail records err as the error Serve returns, if it is the first, and
// stops polling, so the Service stops as on shutdown.
func (g *group) fail(err error) {
	g.mu.Lock()
	if g.first == nil {
		g.first = err
	}
	g.mu.Unlock()
	g.stopPolling()
}

func (g *group) err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.first
}

func (g *group) partition(tp topicPartition) *partition {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.parts[tp]
}

// assigned starts a goroutine for each partition newly assigned, and holds
// every partition the Service still has under the session's member ID.
// franz-go calls it at the end of every join, even when nothing new is
// assigned, so a partition a member keeps after rejoining as a new member is
// held under the new member ID. Its first call is what turns Joined's
// monitor healthy.
func (g *group) assigned(_ context.Context, cl *kgo.Client, assigned map[string][]int32) {
	g.call.joined.Store(true)
	g.logPartitions("partitions assigned", assigned)
	member, _ := cl.GroupMetadata()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing {
		return
	}
	for _, p := range g.parts {
		p.member.Store(&member)
	}
	for topic, partitions := range assigned {
		for _, id := range partitions {
			tp := topicPartition{topic, id}
			if _, ok := g.parts[tp]; ok {
				continue
			}
			p := &partition{
				topicPartition: tp,
				batches:        make(chan []*kgo.Record, 1),
				quit:           make(chan struct{}),
				done:           make(chan struct{}),
				attrs: []attribute.KeyValue{
					semconv.MessagingConsumerGroupName(g.svc.settings.GroupID),
					semconv.MessagingDestinationName(topic),
					semconv.MessagingDestinationPartitionID(strconv.FormatInt(int64(id), 10)),
				},
			}
			p.member.Store(&member)
			g.parts[tp] = p
			go g.run(cl, p)
		}
	}
}

// revoked stops each revoked partition after its record in flight, which
// commits what it processed, and returns once they have all stopped. When
// the member was fenced out and rejoined as a new member, franz-go reports
// the partitions it held as revoked, and their commit is not sent: they are
// not held under the new member ID.
func (g *group) revoked(_ context.Context, _ *kgo.Client, revoked map[string][]int32) {
	g.logPartitions("partitions revoked", revoked)
	g.stop(revoked, false)
}

// lost stops each lost partition after its record in flight, without
// committing: another member may own it already, and its new owner
// processes the uncommitted records again.
func (g *group) lost(_ context.Context, _ *kgo.Client, lost map[string][]int32) {
	g.logPartitions("partitions lost", lost)
	g.stop(lost, true)
}

// stopAll stops every partition after its record in flight, which commits
// what it processed.
func (g *group) stopAll() {
	g.mu.Lock()
	g.closing = true
	all := make(map[string][]int32)
	for tp := range g.parts {
		all[tp.topic] = append(all[tp.topic], tp.partition)
	}
	g.mu.Unlock()
	g.stop(all, false)
}

// stop stops the partitions in m and waits for each to return.
func (g *group) stop(m map[string][]int32, lost bool) {
	var stopping []*partition
	g.mu.Lock()
	for topic, partitions := range m {
		for _, id := range partitions {
			tp := topicPartition{topic, id}
			p, ok := g.parts[tp]
			if !ok {
				continue
			}
			delete(g.parts, tp)
			if lost {
				p.lost.Store(true)
			}
			p.close()
			stopping = append(stopping, p)
		}
	}
	g.mu.Unlock()
	for _, p := range stopping {
		<-p.done
	}
}

// run processes p's batches, one at a time, until p is stopped or a batch
// ends it.
func (g *group) run(cl *kgo.Client, p *partition) {
	defer close(p.done)
	for {
		// A partition being stopped takes no more batches, even when one
		// is waiting.
		if p.quitting() {
			g.commit(cl, p)
			return
		}
		select {
		case <-p.quit:
			g.commit(cl, p)
			return
		case records := <-p.batches:
			var ok bool
			if g.svc.settings.Delivery == queue.AtMostOnce {
				ok = g.atMostOnce(cl, p, records)
			} else {
				ok = g.atLeastOnce(cl, p, records)
			}
			if !ok {
				return
			}
		}
	}
}

// atLeastOnce processes records in order, and then commits the last one it
// processed. It stops after the record in flight when p is stopped. It
// reports whether p goes on.
func (g *group) atLeastOnce(cl *kgo.Client, p *partition, records []*kgo.Record) bool {
	for _, r := range records {
		if p.quitting() {
			break
		}
		if err := g.process(p, r); err != nil {
			if g.work.Err() != nil {
				// Shutdown's context cut the record short: nothing more
				// is committed.
				return false
			}
			g.fail(fmt.Errorf("kafka: processing %s, partition %d, offset %d: %w", r.Topic, r.Partition, r.Offset, err))
			// The records before it were processed, so they are
			// committed, and it and the rest of the batch are not.
			g.commit(cl, p)
			return false
		}
		p.pending = r
	}
	return g.commit(cl, p)
}

// atMostOnce commits records before it processes any of them, and then
// processes every one: they are committed, so stopping between them would
// lose them. A processing error is logged, and processing goes on. It
// reports whether p goes on.
func (g *group) atMostOnce(cl *kgo.Client, p *partition, records []*kgo.Record) bool {
	if p.quitting() || p.lost.Load() {
		return false
	}
	last := records[len(records)-1]
	if err := g.commitRecord(cl, p, last); err != nil {
		switch {
		case g.work.Err() != nil:
			return false
		case notMember(err):
			// The batch isn't committed, so it isn't processed either: a
			// record processed here and again by the partition's new
			// owner would be processed twice. If the partition stays
			// with this member, the batch is lost, so it is logged.
			g.logSkippedBatch(records[0], last, err)
			return true
		default:
			g.fail(fmt.Errorf("kafka: committing %s, partition %d, offset %d: %w", last.Topic, last.Partition, last.Offset, err))
			return false
		}
	}
	for _, r := range records {
		if g.work.Err() != nil {
			return false
		}
		// process logs a failure itself, inside the record's span, so
		// processing goes on.
		_ = g.process(p, r)
	}
	return true
}

// commit commits p.pending, unless p was lost, and reports whether p goes
// on. A commit refused because the member is no longer in the group's
// current generation, or not sent because p is not held under the member ID
// franz-go would send it under, is not a failure: franz-go rejoins and
// p.pending stays pending. A later commit covers it only while p stays with
// this member; once p is revoked or lost, nothing more is committed for it,
// and its new owner decides its committed offset.
func (g *group) commit(cl *kgo.Client, p *partition) bool {
	if p.pending == nil || p.lost.Load() {
		return true
	}
	r := p.pending
	err := g.commitRecord(cl, p, r)
	switch {
	case err == nil:
		p.pending = nil
		return true
	case g.work.Err() != nil:
		// Shutdown's context cut the commit short.
		return false
	case notMember(err):
		return true
	default:
		g.fail(fmt.Errorf("kafka: committing %s, partition %d, offset %d: %w", r.Topic, r.Partition, r.Offset, err))
		return false
	}
}

// commitRecord commits r, which commits every record of p before it, and
// records how long the commit took.
//
// The commit is sent only under the member ID p is held under, and it
// returns errNotHeld otherwise. The check runs as franz-go sends the commit,
// not as CommitRecords is called: a commit called while franz-go joins the
// group waits for the join to finish, and then goes out under the member ID
// the join returned.
func (g *group) commitRecord(cl *kgo.Client, p *partition, r *kgo.Record) error {
	start := time.Now()
	ctx := kgo.PreCommitFnContext(g.work, func(req *kmsg.OffsetCommitRequest) error {
		if m := p.member.Load(); m == nil || req.MemberID != *m {
			return errNotHeld
		}
		return nil
	})
	err := cl.CommitRecords(ctx, r)
	attrs := append(p.attrs[:len(p.attrs):len(p.attrs)], semconv.MessagingOperationTypeSettle)
	if err != nil {
		attrs = append(attrs, semconv.ErrorType(err))
	}
	g.svc.operations.Record(g.work, time.Since(start).Seconds(), commitOperation, messagingconv.SystemKafka, attrs...)
	return err
}

// process hands r, a record of p, to the Processor, under the record's
// span, and records the record as consumed and how long processing it
// took. No panic is recovered.
//
// The span's parent is the trace context the propagator extracts from r's
// headers, whose baggage is on Process's context too. When the headers
// carry none, the span is the root of a new trace.
//
// Under queue.AtMostOnce, process logs a failure itself, with the record's
// span context, unless Shutdown's context cut processing short.
func (g *group) process(p *partition, r *kgo.Record) error {
	svc := g.svc
	parent := svc.propagator.Extract(g.spanBase, (*headerCarrier)(&r.Headers))
	ctx, span := svc.tracer.Start(parent, "process "+r.Topic,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			semconv.MessagingSystemKafka,
			semconv.MessagingOperationTypeProcess,
			semconv.MessagingOperationName(processOperation),
			semconv.MessagingDestinationName(r.Topic),
			semconv.MessagingDestinationPartitionID(strconv.FormatInt(int64(r.Partition), 10)),
			semconv.MessagingConsumerGroupName(svc.settings.GroupID),
			semconv.MessagingKafkaOffset(int(r.Offset)),
		),
	)
	defer span.End()

	svc.consumed.Add(ctx, 1, processOperation, messagingconv.SystemKafka, p.attrs...)
	start := time.Now()
	err := svc.proc.Process(ctx, newMessage(r))
	elapsed := time.Since(start).Seconds()

	attrs := p.attrs
	if err != nil {
		errType := semconv.ErrorType(err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(errType)
		attrs = append(attrs[:len(attrs):len(attrs)], errType)
		if svc.settings.Delivery == queue.AtMostOnce && g.work.Err() == nil {
			g.logProcessingError(ctx, r, err)
		}
	}
	svc.processing.Record(ctx, elapsed, processOperation, messagingconv.SystemKafka, attrs...)
	return err
}

// logPartitions emits one INFO record per topic of m with body, naming the
// topic and its partitions.
func (g *group) logPartitions(body string, m map[string][]int32) {
	topics := make([]string, 0, len(m))
	for topic, partitions := range m {
		if len(partitions) > 0 {
			topics = append(topics, topic)
		}
	}
	slices.Sort(topics)
	for _, topic := range topics {
		ids := make([]int64, len(m[topic]))
		for i, id := range m[topic] {
			ids[i] = int64(id)
		}
		slices.Sort(ids)
		g.emit(g.logCtx, log.SeverityInfo, body,
			semconv.MessagingSystemKafka,
			semconv.MessagingConsumerGroupName(g.svc.settings.GroupID),
			semconv.MessagingDestinationName(topic),
			partitionsKey.Int64Slice(ids),
		)
	}
}

// logProcessingError emits an ERROR record for the error processing r
// returned under queue.AtMostOnce, with ctx, which carries r's span.
func (g *group) logProcessingError(ctx context.Context, r *kgo.Record, err error) {
	g.emit(ctx, log.SeverityError, "processing a record failed",
		semconv.MessagingSystemKafka,
		semconv.MessagingConsumerGroupName(g.svc.settings.GroupID),
		semconv.MessagingDestinationName(r.Topic),
		semconv.MessagingDestinationPartitionID(strconv.FormatInt(int64(r.Partition), 10)),
		semconv.MessagingKafkaOffset(int(r.Offset)),
		semconv.ErrorType(err),
		semconv.ExceptionMessage(err.Error()),
	)
}

// logSkippedBatch emits a WARN record for the batch from first to last that
// is not processed under queue.AtMostOnce, because its commit was refused
// with err, or, with errNotHeld, not sent.
func (g *group) logSkippedBatch(first, last *kgo.Record, err error) {
	g.emit(g.logCtx, log.SeverityWarn, "batch skipped: its commit was refused",
		semconv.MessagingSystemKafka,
		semconv.MessagingConsumerGroupName(g.svc.settings.GroupID),
		semconv.MessagingDestinationName(first.Topic),
		semconv.MessagingDestinationPartitionID(strconv.FormatInt(int64(first.Partition), 10)),
		firstOffsetKey.Int64(first.Offset),
		lastOffsetKey.Int64(last.Offset),
		semconv.ErrorType(err),
		semconv.ExceptionMessage(err.Error()),
	)
}

// emit emits one record at sev, with ctx, body and attrs.
func (g *group) emit(ctx context.Context, sev log.Severity, body string, attrs ...attribute.KeyValue) {
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(sev.String())
	r.SetBody(attribute.StringValue(body))
	r.AddAttributes(attrs...)
	g.svc.logger.Emit(ctx, r)
}
