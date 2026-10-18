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
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/z5labs/humus"
)

// Publishing is a message to publish. Its zero value is persistent.
type Publishing struct {
	// Exchange is the exchange to publish to, "" for the default exchange.
	// It must already exist: the Publisher declares nothing.
	Exchange string

	// RoutingKey is the routing key to publish with.
	RoutingKey string

	// Headers are its headers. Publish never changes the map.
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
// Transient is set, with a copy of m.Headers, and every other field as it
// is.
func (m Publishing) amqpPublishing() amqp.Publishing {
	mode := amqp.Persistent
	if m.Transient {
		mode = amqp.Transient
	}
	return amqp.Publishing{
		Headers:         amqp.Table(maps.Clone(m.Headers)),
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
// names each one. The Publisher reaches the broker only through s.Host.
//
// The function NewPublisher registers with s.OnStop closes the connection,
// if one is open, within the grace period. It runs once every Service has
// stopped, so a Processor finishing its delivery during the drain can still
// publish, and it also runs when Init fails after NewPublisher. After it
// has run, Publish fails at once.
func NewPublisher(s *humus.Setup, settings PublisherSettings) (*Publisher, error) {
	if err := checkPublisherSettings(settings); err != nil {
		return nil, fmt.Errorf("rabbitmq: NewPublisher: %w", err)
	}
	stopped, stop := context.WithCancel(context.Background())
	p := &Publisher{
		settings: settings,
		slots:    make(chan struct{}, settings.Channels),
		connLock: make(chan struct{}, 1),
		stopped:  stopped,
		stop:     stop,
	}
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
	return errors.Join(errs...)
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
// It publishes m with mandatory set, on a channel in confirm mode, and
// waits for the broker's confirm. It returns nil once the broker has
// confirmed m, and an error matching ErrUnroutable when the broker
// returned m because no queue was bound to receive it. It returns an error
// when the broker nacked m, and the broker's *amqp.Error when it closed the
// channel, as with 404 NOT_FOUND for an exchange that doesn't exist or 403
// ACCESS_REFUSED. A lost connection, or a failed dial or handshake, is
// returned as it is, and the next Publish connects again.
//
// Settings.Timeout bounds the whole Publish, its confirm included, unless
// ctx has an earlier deadline, which then wins. When ctx is done first,
// Publish returns an error wrapping ctx's error.
func (p *Publisher) Publish(ctx context.Context, m Publishing) error {
	if p.stopped.Err() != nil {
		return errStopped
	}
	ctx, cancel := context.WithTimeout(ctx, p.settings.Timeout)
	defer cancel()

	cc, err := p.take(ctx, m)
	if err != nil {
		return err
	}
	reuse, err := cc.publish(ctx, m)
	p.give(cc, reuse)
	return err
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
			return nil, fmt.Errorf("rabbitmq: opening a channel to %s: %w", p.settings.Connection.Addr, o.err)
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
		return nil, err
	}
	p.conn = conn
	return conn, nil
}

// open reports whether cc and its connection are still open.
func (cc *confirmChannel) open() bool {
	return !cc.conn.IsClosed() && !cc.ch.IsClosed()
}

// publish publishes m on cc with mandatory set, and waits for the broker's
// confirm, or for ctx. It returns whether cc can carry another publish,
// and the publish's error.
//
// amqp091 hands a basic.return to the channel's listener before it hands
// over the confirm that follows it, so a return that is waiting once the
// confirm arrives belongs to this publish.
func (cc *confirmChannel) publish(ctx context.Context, m Publishing) (reuse bool, err error) {
	dest := destination(m)
	if err := cc.ch.PublishWithContext(ctx, m.Exchange, m.RoutingKey, true, false, m.amqpPublishing()); err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("rabbitmq: publishing to %s: %w", dest, ctx.Err())
		}
		return false, fmt.Errorf("rabbitmq: publishing to %s: %w", dest, err)
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
			return true, fmt.Errorf("rabbitmq: publishing to %s: the broker nacked the message", dest)
		}
		return true, nil
	case e, ok := <-cc.closes:
		return false, closedError(dest, e, ok)
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
		return closedError(dest, e, ok)
	default:
		return closedError(dest, nil, false)
	}
}

// closedError is the error of a publish whose channel closed with e: the
// broker's error, either the channel's or the lost connection's, or
// amqp.ErrClosed when the channel closed without one.
func closedError(dest string, e *amqp.Error, ok bool) error {
	if ok && e != nil {
		return fmt.Errorf("rabbitmq: publishing to %s: %w", dest, e)
	}
	return fmt.Errorf("rabbitmq: publishing to %s: %w", dest, amqp.ErrClosed)
}
