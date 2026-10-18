// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/z5labs/humus"
)

// Client is a grpc.ClientConnInterface, so generated code takes it:
//
//	cartpb.NewCartServiceClient(client)
//
// It is safe for concurrent use. One Client is one dependency, over one
// connection. It has no Close: NewClient registers the connection's close
// with the Setup's OnStop.
type Client struct {
	cc      *grpc.ClientConn
	timeout time.Duration
}

var _ grpc.ClientConnInterface = (*Client)(nil)

// NewClient runs every check on settings, reporting every bad field in one
// error that names each one, and builds the Client. It opens no connection:
// the first RPC dials. It also rejects a nil s, and an s whose Host has no
// Dial, rather than let grpc-go fall back to dialling the real network.
//
// The Client dials settings.Addr through s.Host, with grpc-go's passthrough
// resolver, so no name is looked up but by the Host.
//
// humus's first dial option is otelgrpc's client stats handler, given the
// TracerProvider, the MeterProvider and the Propagator of s.Telemetry, so
// each attempt is a client span and is measured, and nothing falls back to
// OpenTelemetry's globals. A stats handler in opts is a second one, which
// grpc-go calls as well.
//
// opts come after humus's own dial options, and the transport credentials
// built from settings.TLS come after opts, so a
// grpc.WithTransportCredentials in opts has no effect.
//
// NewClient registers the connection's close with s.OnStop.
func NewClient(s *humus.Setup, settings Settings, opts ...grpc.DialOption) (*Client, error) {
	if s == nil {
		return nil, errors.New("grpcclient: NewClient: the Setup must not be nil")
	}
	err := checkSettings(settings)
	if s.Host.Dial == nil {
		err = errors.Join(errors.New("the Setup's Host.Dial must not be nil"), err)
	}
	if err != nil {
		return nil, fmt.Errorf("grpcclient: NewClient: %w", err)
	}

	dial := s.Host.Dial
	creds := insecure.NewCredentials()
	if settings.TLS != nil {
		// credentials.NewTLS clones the config, so the end user's is never
		// changed, and adds h2 to its ALPN.
		creds = credentials.NewTLS(settings.TLS)
	}

	// The stats handler comes first, so it is the first one grpc-go calls,
	// and a stats handler in opts is a second one. Every provider and the
	// propagator are passed, so otelgrpc never falls back to
	// OpenTelemetry's globals.
	dopts := make([]grpc.DialOption, 0, len(opts)+5)
	dopts = append(dopts,
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(s.Telemetry.TracerProvider),
			otelgrpc.WithMeterProvider(s.Telemetry.MeterProvider),
			otelgrpc.WithPropagators(s.Telemetry.Propagator),
		)),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return dial(ctx, "tcp", addr)
		}),
		grpc.WithNoProxy(),
		grpc.WithDisableRetry(),
	)
	dopts = append(dopts, opts...)
	dopts = append(dopts, grpc.WithTransportCredentials(creds))

	cc, err := grpc.NewClient("passthrough:///"+settings.Addr, dopts...)
	if err != nil {
		return nil, fmt.Errorf("grpcclient: NewClient: %w", err)
	}
	s.OnStop(func(context.Context) error {
		return cc.Close()
	})
	return &Client{cc: cc, timeout: settings.Timeout}, nil
}

// Invoke makes a unary RPC. Settings.Timeout bounds it, unless ctx has an
// earlier deadline, which then wins. It is called by generated code.
func (c *Client) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.cc.Invoke(ctx, method, args, reply, opts...)
}

// NewStream opens a stream. Settings.Timeout doesn't apply to it: a stream
// lives as long as ctx, which its caller ends. It is called by generated
// code.
func (c *Client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return c.cc.NewStream(ctx, desc, method, opts...)
}
