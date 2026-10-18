// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcauth

import (
	"context"
	"crypto/x509"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// source is where an RPC's credentials are read from: its incoming
// metadata, and its peer's TLS state.
type source struct {
	md   metadata.MD
	peer *peer.Peer
}

// newSource returns the source of the RPC whose context is ctx.
func newSource(ctx context.Context) source {
	md, _ := metadata.FromIncomingContext(ctx)
	p, _ := peer.FromContext(ctx)
	return source{md: md, peer: p}
}

// Header returns the values of the metadata key name, lowercased, as HTTP/2
// carries every header: authorization for the Authorization header.
func (s source) Header(name string) []string { return s.md.Get(name) }

// Query returns nothing: an RPC has no query parameter. Interceptors
// rejects a scheme that reads one.
func (source) Query(string) []string { return nil }

// Cookie returns nothing: an RPC has no cookie. Interceptors rejects a
// scheme that reads one.
func (source) Cookie(string) []string { return nil }

// VerifiedChains returns the client certificate chains the TLS handshake
// verified, none when it verified none: a certificate that was only
// requested is no credential.
func (s source) VerifiedChains() [][]*x509.Certificate {
	if s.peer == nil {
		return nil
	}
	info, ok := s.peer.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}
	return info.State.VerifiedChains
}
