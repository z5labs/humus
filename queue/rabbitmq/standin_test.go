// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/z5labs/humus/queue/rabbitmq"
)

// standIn stands in for amqp091's channel, and the connection it is on, in
// the Service's unit tests. It implements no part of AMQP and keeps no
// queue: it hands the Service the deliveries the test gives it, records
// each call the Service makes, and answers a Qos made after consuming has
// started only when the test says, if the test holds them. It closes as a
// lost connection does, failing every call after, and ends its deliveries
// as a consumer the broker cancelled does, when the test says.
type standIn struct {
	mu    sync.Mutex
	calls []string

	// deliveries are the deliveries the test has given and the Service has
	// not yet taken. It is closed when the consumer is cancelled or the
	// channel or the connection is closed.
	deliveries chan amqp.Delivery
	ended      bool
	nextTag    uint64

	consuming bool
	// holdConsume makes ConsumeWithContext wait for answerConsume before it
	// answers, as a broker that has yet to send basic.consume-ok.
	holdConsume bool
	consume     chan struct{}
	// holdQos makes each Qos made once consuming has started wait for
	// answerQos.
	holdQos bool
	qos     chan struct{}

	// gone is closed once the channel or the connection is closed, or lost.
	gone     chan struct{}
	goneOnce sync.Once
	notify   []chan *amqp.Error

	// connClosed is set once the Service has closed the connection.
	connClosed bool
}

func newStandIn() *standIn {
	return &standIn{
		deliveries: make(chan amqp.Delivery, 1000),
		qos:        make(chan struct{}),
		consume:    make(chan struct{}),
		gone:       make(chan struct{}),
	}
}

// open is the Service's open function: it returns the stand-in, as the
// channel and as the connection.
func (s *standIn) open(context.Context) (rabbitmq.Channel, io.Closer, error) {
	return s, standInConn{s}, nil
}

// deliver gives the Service a delivery for each ID, as the broker would,
// with the next delivery tags, from 1.
func (s *standIn) deliver(ids ...string) {
	s.give(false, ids)
}

// redeliver gives the Service a delivery for each ID, as deliver does, with
// Redelivered set, as the broker delivers again what it requeued.
func (s *standIn) redeliver(ids ...string) {
	s.give(true, ids)
}

func (s *standIn) give(redelivered bool, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.nextTag++
		s.deliveries <- amqp.Delivery{
			DeliveryTag: s.nextTag,
			MessageId:   id,
			RoutingKey:  queueName,
			Redelivered: redelivered,
		}
	}
}

// send gives the Service each delivery, as the broker would, with the next
// delivery tags, from 1, in place of the tags they carry.
func (s *standIn) send(ds ...amqp.Delivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range ds {
		s.nextTag++
		d.DeliveryTag = s.nextTag
		s.deliveries <- d
	}
}

// holdConsumes makes the stand-in hold each ConsumeWithContext until
// answerConsume.
func (s *standIn) holdConsumes() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdConsume = true
}

// answerConsume answers a ConsumeWithContext the stand-in is holding.
func (s *standIn) answerConsume() {
	s.consume <- struct{}{}
}

// answerQos answers a Qos the stand-in is holding.
func (s *standIn) answerQos() {
	s.qos <- struct{}{}
}

// lose closes the stand-in as a lost connection does, with err.
func (s *standIn) lose(err *amqp.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("connection lost")
	s.close(err)
}

// cancelConsumer ends the deliveries as the broker cancelling the consumer
// does, as when its queue is deleted. The channel stays open.
func (s *standIn) cancelConsumer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("consumer cancelled")
	s.endDeliveries()
}

// Calls returns the calls the Service has made, in order.
func (s *standIn) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *standIn) record(format string, args ...any) {
	s.calls = append(s.calls, fmt.Sprintf(format, args...))
}

// endDeliveries closes the deliveries, once. s.mu is held.
func (s *standIn) endDeliveries() {
	if !s.ended {
		s.ended = true
		close(s.deliveries)
	}
}

// close sends err, if any, to every NotifyClose listener, ends the
// deliveries, and closes the listeners, in the order amqp091's
// Channel.shutdown does, so a listener has the error by the time the
// deliveries end. s.mu is held.
func (s *standIn) close(err *amqp.Error) {
	s.goneOnce.Do(func() {
		if err != nil {
			for _, c := range s.notify {
				c <- err
			}
		}
		close(s.gone)
		s.endDeliveries()
		for _, c := range s.notify {
			close(c)
		}
	})
	s.endDeliveries()
}

// closed reports whether the channel is closed. s.mu is held.
func (s *standIn) closed() bool {
	select {
	case <-s.gone:
		return true
	default:
		return false
	}
}

func (s *standIn) Qos(prefetchCount, _ int, _ bool) error {
	s.mu.Lock()
	s.record("qos %d", prefetchCount)
	if s.closed() {
		s.mu.Unlock()
		return amqp.ErrClosed
	}
	hold := s.holdQos && s.consuming
	s.mu.Unlock()
	if !hold {
		return nil
	}
	select {
	case <-s.qos:
		return nil
	case <-s.gone:
		return amqp.ErrClosed
	}
}

func (s *standIn) ConsumeWithContext(_ context.Context, queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("consume %s %s autoAck=%t exclusive=%t noLocal=%t noWait=%t args=%v", queue, consumer, autoAck, exclusive, noLocal, noWait, args)
	if s.closed() {
		return nil, amqp.ErrClosed
	}
	if s.holdConsume {
		s.mu.Unlock()
		select {
		case <-s.consume:
		case <-s.gone:
		}
		s.mu.Lock()
		if s.closed() {
			return nil, amqp.ErrClosed
		}
	}
	s.consuming = true
	return s.deliveries, nil
}

func (s *standIn) Ack(tag uint64, multiple bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if multiple {
		s.record("ack %d multiple", tag)
	} else {
		s.record("ack %d", tag)
	}
	if s.closed() {
		return amqp.ErrClosed
	}
	return nil
}

func (s *standIn) Nack(tag uint64, multiple, requeue bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("nack %d multiple=%t requeue=%t", tag, multiple, requeue)
	if s.closed() {
		return amqp.ErrClosed
	}
	return nil
}

func (s *standIn) Reject(tag uint64, requeue bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("reject %d requeue=%t", tag, requeue)
	if s.closed() {
		return amqp.ErrClosed
	}
	return nil
}

func (s *standIn) Cancel(consumer string, noWait bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("cancel %s noWait=%t", consumer, noWait)
	if s.closed() {
		return amqp.ErrClosed
	}
	s.endDeliveries()
	return nil
}

func (s *standIn) NotifyClose(c chan *amqp.Error) chan *amqp.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed() {
		close(c)
		return c
	}
	s.notify = append(s.notify, c)
	return c
}

func (s *standIn) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed() {
		return nil
	}
	s.record("close channel")
	s.close(nil)
	return nil
}

// standInConn is the stand-in's connection. Closing it closes the channel.
type standInConn struct{ s *standIn }

func (c standInConn) Close() error {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connClosed {
		s.connClosed = true
		s.record("close connection")
	}
	s.close(nil)
	return nil
}
