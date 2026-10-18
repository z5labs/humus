// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/z5labs/humus/queue/rabbitmq"
)

// pubStandIn stands in for amqp091's connections and channels in the
// Publisher's unit tests. It implements no part of AMQP and keeps no queue:
// it hands the test each publish the Publisher makes, and confirms, nacks
// or returns it as the test says. A channel fails with a channel error, and
// a connection closes as a lost connection does, when the test says.
type pubStandIn struct {
	// published carries each publish the Publisher makes, in order.
	published chan *published

	// autoAck makes each channel confirm each publish as it is made.
	autoAck bool

	// hold makes each dial wait until the test sends to it, or its
	// context ends.
	hold chan struct{}

	mu    sync.Mutex
	conns []*pubConn
}

func newPubStandIn() *pubStandIn {
	return &pubStandIn{published: make(chan *published, 100)}
}

// dial is the Publisher's dial function: it returns a new stand-in
// connection.
func (s *pubStandIn) dial(ctx context.Context) (rabbitmq.PublishConn, error) {
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &pubConn{s: s}
	s.conns = append(s.conns, c)
	return c, nil
}

// dials returns how many connections the Publisher has made.
func (s *pubStandIn) dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// conn returns the i-th connection the Publisher made, from 0.
func (s *pubStandIn) conn(i int) *pubConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[i]
}

// published is one publish the Publisher made: where, what, and the
// channel it was made on.
type published struct {
	ch        *pubChan
	exchange  string
	key       string
	mandatory bool
	immediate bool
	msg       amqp.Publishing
}

// ack confirms the publish.
func (p *published) ack() { p.ch.confirm(true) }

// nack nacks the publish.
func (p *published) nack() { p.ch.confirm(false) }

// unroutable returns the publish, as the broker does for a mandatory
// message no queue is bound to receive, and then confirms it.
func (p *published) unroutable() {
	p.ch.returns <- amqp.Return{
		ReplyCode:  amqp.NoRoute,
		ReplyText:  "NO_ROUTE",
		Exchange:   p.exchange,
		RoutingKey: p.key,
	}
	p.ch.confirm(true)
}

// pubConn is a stand-in connection.
type pubConn struct {
	s *pubStandIn

	mu       sync.Mutex
	closed   bool
	closedBy string // "the Publisher" or "lost"
	channels []*pubChan
}

func (c *pubConn) OpenChannel() (rabbitmq.PublishChannel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, amqp.ErrClosed
	}
	ch := &pubChan{conn: c}
	c.channels = append(c.channels, ch)
	return ch, nil
}

func (c *pubConn) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *pubConn) CloseDeadline(time.Time) error {
	c.shutdown("the Publisher", nil)
	return nil
}

// lose closes the connection, and each of its channels, with err, as a lost
// connection does.
func (c *pubConn) lose(err *amqp.Error) {
	c.shutdown("lost", err)
}

func (c *pubConn) shutdown(by string, err *amqp.Error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closedBy = by
	channels := c.channels
	c.mu.Unlock()
	for _, ch := range channels {
		ch.shutdown(err)
	}
}

// closer returns who closed the connection: "" while it is open.
func (c *pubConn) closer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closedBy
}

// channelCount returns how many channels the Publisher opened on c.
func (c *pubConn) channelCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.channels)
}

// pubChan is a stand-in channel.
type pubChan struct {
	conn *pubConn

	mu         sync.Mutex
	confirming bool
	confirms   chan amqp.Confirmation
	returns    chan amqp.Return
	closes     chan *amqp.Error
	closed     bool
	closedByUs bool
	tag        uint64
}

func (ch *pubChan) Confirm(bool) error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed {
		return amqp.ErrClosed
	}
	ch.confirming = true
	return nil
}

func (ch *pubChan) NotifyPublish(c chan amqp.Confirmation) chan amqp.Confirmation {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.confirms = c
	return c
}

func (ch *pubChan) NotifyReturn(c chan amqp.Return) chan amqp.Return {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.returns = c
	return c
}

func (ch *pubChan) NotifyClose(c chan *amqp.Error) chan *amqp.Error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.closes = c
	return c
}

func (ch *pubChan) PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ch.mu.Lock()
	if ch.closed {
		ch.mu.Unlock()
		return amqp.ErrClosed
	}
	ch.tag++
	ch.mu.Unlock()
	if ch.conn.s.autoAck {
		ch.confirm(true)
	}
	ch.conn.s.published <- &published{ch: ch, exchange: exchange, key: key, mandatory: mandatory, immediate: immediate, msg: msg}
	return nil
}

// confirm acks, or nacks, the publish in flight on ch.
func (ch *pubChan) confirm(ack bool) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed {
		return
	}
	ch.confirms <- amqp.Confirmation{DeliveryTag: ch.tag, Ack: ack}
}

// fail closes ch with err, as the broker closes a channel with an error.
func (ch *pubChan) fail(err *amqp.Error) {
	ch.shutdown(err)
}

func (ch *pubChan) Close() error {
	ch.mu.Lock()
	if !ch.closed {
		ch.closedByUs = true
	}
	ch.mu.Unlock()
	ch.shutdown(nil)
	return nil
}

// shutdown sends err, if any, to the close listener, and closes every
// listener, in the order amqp091's Channel.shutdown does.
func (ch *pubChan) shutdown(err *amqp.Error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed {
		return
	}
	ch.closed = true
	if err != nil && ch.closes != nil {
		ch.closes <- err
	}
	if ch.confirms != nil {
		close(ch.confirms)
	}
	if ch.returns != nil {
		close(ch.returns)
	}
	if ch.closes != nil {
		close(ch.closes)
	}
}

// isClosed reports whether ch is closed, and whether the Publisher closed
// it.
func (ch *pubChan) isClosed() (closed, byPublisher bool) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.closed, ch.closedByUs
}
