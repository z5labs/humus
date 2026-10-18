// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/z5labs/humus"
)

// locale is the locale every connection asks for. amqp091's DialConfig sets
// it when a Config leaves it empty, and amqp.Open doesn't.
const locale = "en_US"

// connect opens an AMQP connection to c.Addr through host, in three steps,
// all bounded by c.ConnectTimeout: the dial, the TLS handshake when c.TLS is
// set, and the AMQP handshake.
//
// It dials with host.Dial and hands the connection to amqp.Open, so the
// broker is reached only through the Host, and no URL is ever built: amqp091's
// Dial, DialConfig, DialTLS and DefaultDial all take or make one, and dial the
// real network. The TLS handshake uses a clone of c.TLS, whose ServerName is
// Addr's host when c.TLS leaves it empty, so the end user's config is never
// changed.
//
// On any failure the network connection is closed, so nothing stays open.
// When a step fails because ConnectTimeout ran out, the error wraps
// context.DeadlineExceeded.
func connect(ctx context.Context, host humus.Host, c Connection) (*amqp.Connection, error) {
	var cfg *tls.Config
	if c.TLS != nil {
		cfg = c.TLS.Clone()
		if cfg.ServerName == "" {
			name, _, err := net.SplitHostPort(c.Addr)
			if err != nil {
				return nil, fmt.Errorf("rabbitmq: dialling %s: %w", c.Addr, err)
			}
			cfg.ServerName = name
		}
	}

	deadline := time.Now().Add(c.ConnectTimeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

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
	// bounds the AMQP handshake. amqp091 clears it once the connection is
	// open, and its heartbeats take over.
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rabbitmq: AMQP handshake with %s: %w", c.Addr, err)
	}
	ac, err := amqp.Open(conn, amqp.Config{
		SASL:      []amqp.Authentication{c.SASL},
		Vhost:     c.Vhost,
		Heartbeat: c.Heartbeat,
		Locale:    locale,
	})
	if err != nil {
		// Closing the connection also stops the goroutines amqp.Open
		// started, which exit once their reads fail.
		_ = conn.Close()
		if !time.Now().Before(deadline) {
			// amqp091 reports a deadline as a frame error, which doesn't
			// say that ConnectTimeout ran out.
			return nil, fmt.Errorf("rabbitmq: AMQP handshake with %s: %w: %w", c.Addr, context.DeadlineExceeded, err)
		}
		return nil, fmt.Errorf("rabbitmq: AMQP handshake with %s: %w", c.Addr, err)
	}
	return ac, nil
}
