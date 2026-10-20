// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/z5labs/humus"
)

// locale is the locale every connection asks for. amqp091's DialConfig sets
// it when a Config leaves it empty, and amqp.Open doesn't.
const locale = "en_US"

// connect opens an AMQP connection to c.Addr through host, in three steps,
// all bounded by c.ConnectTimeout and by ctx: the dial, the TLS handshake
// when c.TLS is set, and the AMQP handshake.
//
// It dials with host.Dial and hands the connection to amqp.Open, so the
// broker is reached only through the Host, and no URL is ever built: amqp091's
// Dial, DialConfig, DialTLS and DefaultDial all take or make one, and dial the
// real network. The TLS handshake uses a clone of c.TLS, whose ServerName is
// Addr's host when c.TLS leaves it empty, so the end user's config is never
// changed.
//
// On any failure the network connection is closed, so nothing stays open.
// When a step fails because ConnectTimeout ran out or ctx is done, the error
// wraps ctx's error, context.DeadlineExceeded or context.Canceled, and not
// amqp091's: amqp091 reports a failure after Connection.Start as
// ErrCredentials, and one during Connection.Open as ErrVhost, whenever the
// broker didn't say why, so a timeout would otherwise read as a wrong login.
// When the broker refuses Connection.Open with 530 NOT_ALLOWED, the error
// wraps a vhostRefusal.
func connect(ctx context.Context, host humus.Host, c Connection) (*amqp.Connection, error) {
	var cfg *tls.Config
	if c.TLS != nil {
		cfg = c.TLS.Clone()
		if cfg.ServerName == "" {
			name, _, err := net.SplitHostPort(c.Addr)
			if err != nil {
				return nil, fmt.Errorf("rabbitmq: TLS server name from %s: %w", c.Addr, err)
			}
			cfg.ServerName = name
		}
	}

	ctx, cancel := context.WithTimeout(ctx, c.ConnectTimeout)
	defer cancel()
	// The earlier of ConnectTimeout's end and ctx's own deadline.
	deadline, _ := ctx.Deadline()

	nc, err := host.Dial(ctx, "tcp", c.Addr)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: dialling %s: %w", c.Addr, err)
	}

	conn := nc
	if cfg != nil {
		tc := tls.Client(nc, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			// The handshake's error is the one worth returning; the
			// connection is broken whatever Close says.
			_ = nc.Close()
			return nil, fmt.Errorf("rabbitmq: TLS handshake with %s: %w", c.Addr, err)
		}
		conn = tc
	}

	// amqp.Open reads and writes without a context, so the deadline is what
	// bounds the AMQP handshake, and ctx ending moves it to now. amqp091
	// clears it once the connection is open, and its heartbeats take over.
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rabbitmq: AMQP handshake with %s: %w", c.Addr, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	ac, err := amqp.Open(conn, amqp.Config{
		SASL:      []amqp.Authentication{c.SASL},
		Vhost:     c.Vhost,
		Heartbeat: c.Heartbeat,
		Locale:    locale,
	})
	stopped := stop()
	if err == nil && stopped {
		return ac, nil
	}
	// Closing the connection also stops the goroutines amqp.Open started,
	// which exit once their reads fail. A connection that opened as ctx
	// ended is closed too: the deadline ctx set may have landed after
	// amqp091 cleared it.
	_ = conn.Close()
	if cause := ended(ctx, deadline); cause != nil {
		if err == nil {
			return nil, fmt.Errorf("rabbitmq: AMQP handshake with %s: %w", c.Addr, cause)
		}
		return nil, fmt.Errorf("rabbitmq: AMQP handshake with %s: %w (%v)", c.Addr, cause, err)
	}
	if ae, ok := errors.AsType[*amqp.Error](err); ok && ae.Server && ae.Code == amqp.NotAllowed {
		err = &vhostRefusal{err: ae}
	}
	return nil, fmt.Errorf("rabbitmq: AMQP handshake with %s: %w", c.Addr, err)
}

// vhostRefusal is the broker's refusal of Connection.Open with 530
// NOT_ALLOWED, the reply RabbitMQ gives a vhost that doesn't exist or that
// the login has no access to, and a connection limit that's been reached.
// amqp091 returns the reply as the broker sent it, and RabbitMQ sends 530
// during the handshake only at Connection.Open, so connect marks it: a 530
// on an open connection isn't one. Its message, and its error.type, are the
// reply's.
type vhostRefusal struct{ err *amqp.Error }

func (e *vhostRefusal) Error() string { return e.err.Error() }

func (e *vhostRefusal) Unwrap() error { return e.err }

// ErrorType is semconv's error.type of the reply, which semconv.ErrorType
// asks for when it finds a vhostRefusal in an error's chain.
func (e *vhostRefusal) ErrorType() string {
	return semconv.ErrorType(e.err).Value.AsString()
}

// ended returns ctx's error, or context.DeadlineExceeded once deadline has
// passed: the connection's deadline can fire before ctx's timer has.
func ended(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
