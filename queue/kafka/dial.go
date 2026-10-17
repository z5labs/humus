// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
)

// dialFunc dials an address, like humus.Host.Dial and kgo.Dialer's function.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// tlsDialer returns dial when cfg is nil. Otherwise it returns a dialer that
// runs the TLS handshake over the connection dial returns.
//
// franz-go refuses kgo.Dialer beside kgo.DialTLSConfig, so the handshake is
// the dialer's to run. Each dial uses a clone of cfg, whose ServerName is the
// broker's host when cfg leaves it empty, as kgo.DialTLSConfig does: cfg
// itself is never changed.
func tlsDialer(dial dialFunc, cfg *tls.Config) dialFunc {
	if cfg == nil {
		return dial
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		c := cfg.Clone()
		if c.ServerName == "" {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("kafka: dialling %s: %w", address, err)
			}
			c.ServerName = host
		}
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tc := tls.Client(conn, c)
		if err := tc.HandshakeContext(ctx); err != nil {
			// The handshake's error is the one worth returning; the
			// connection is broken whatever Close says.
			_ = conn.Close()
			return nil, fmt.Errorf("kafka: TLS handshake with %s: %w", address, err)
		}
		return tc, nil
	}
}
