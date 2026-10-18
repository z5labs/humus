// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts,
// with certificates from a CA generated for each test. The end user's App
// serves a backend with grpcserver, builds a Client of it, and runs a job
// that calls the backend through the Client. They use grpc-go's
// interop/grpc_testing service, as testpb.

// rpc is what the backend saw of one RPC.
type rpc struct {
	authority string
	tls       bool
	clientCN  string // the common name of the client's certificate, under mTLS
}

// backend is the end user's backend. Its EmptyCall answers with answer, or
// OK when answer is nil, and its StreamingOutputCall sends a message for
// each of the request's ResponseParameters, after its IntervalUs. It counts
// its calls, and records what it saw of each.
type backend struct {
	testpb.UnimplementedTestServiceServer

	answer func(ctx context.Context) error

	calls atomic.Int32
	mu    sync.Mutex
	rpcs  []rpc
}

func (b *backend) record(ctx context.Context) {
	b.calls.Add(1)
	var r rpc
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if a := md.Get(":authority"); len(a) > 0 {
			r.authority = a[0]
		}
	}
	if p, ok := peer.FromContext(ctx); ok {
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok {
			r.tls = true
			if certs := info.State.PeerCertificates; len(certs) > 0 {
				r.clientCN = certs[0].Subject.CommonName
			}
		}
	}
	b.mu.Lock()
	b.rpcs = append(b.rpcs, r)
	b.mu.Unlock()
}

func (b *backend) seen() []rpc {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.rpcs)
}

func (b *backend) EmptyCall(ctx context.Context, _ *testpb.Empty) (*testpb.Empty, error) {
	b.record(ctx)
	if b.answer != nil {
		if err := b.answer(ctx); err != nil {
			return nil, err
		}
	}
	return &testpb.Empty{}, nil
}

func (b *backend) StreamingOutputCall(req *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	b.record(stream.Context())
	for i, p := range req.GetResponseParameters() {
		time.Sleep(time.Duration(p.GetIntervalUs()) * time.Microsecond)
		if err := stream.Send(&testpb.StreamingOutputCallResponse{Payload: &testpb.Payload{Body: []byte{byte(i)}}}); err != nil {
			return err
		}
	}
	return nil
}

// app is the end user's App, as these tests run it.
type app struct {
	// host is the Host the App runs on.
	host humus.Host

	// backend, if it is set, is served by a grpcserver.Service at
	// backendAddr, backend:9090 if it is empty, with serverTLS.
	backend     *backend
	backendAddr string
	serverTLS   *tls.Config

	// tel builds the App's telemetry, telemetry.NoopBuilder if it is nil.
	tel telemetry.Builder

	// settings and opts build the Client.
	settings grpcclient.Settings
	opts     []grpc.DialOption

	// work is the job's run, given a client of the backend over the
	// Client.
	work func(ctx context.Context, c testpb.TestServiceClient) error
}

// run runs the App, requires it to exit 0, and returns the Client it built
// and what work returned. The job's run returns nil whatever work returns,
// so the App exits once work does.
func (a app) run(t *testing.T) (*grpcclient.Client, error) {
	t.Helper()
	var (
		client  *grpcclient.Client
		workErr error
	)
	init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		var services []humus.Service
		if a.backend != nil {
			addr := a.backendAddr
			if addr == "" {
				addr = "backend:9090"
			}
			srv, err := grpcserver.NewService(s, grpcserver.Settings{Addr: addr, TLS: a.serverTLS})
			if err != nil {
				return nil, err
			}
			testpb.RegisterTestServiceServer(srv, a.backend)
			services = append(services, srv)
		}

		c, err := grpcclient.NewClient(s, a.settings, a.opts...)
		if err != nil {
			return nil, err
		}
		client = c
		j := job.NewService(s, func(ctx context.Context) error {
			workErr = a.work(ctx, testpb.NewTestServiceClient(c))
			return nil
		})
		return append(services, j), nil
	}

	tel := a.tel
	if tel == nil {
		tel = telemetry.NoopBuilder
	}
	_, status := humustest.Start(t, init, humustest.WithApp(humus.WithHost(a.host), humus.WithTelemetry(tel)))
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	return client, workErr
}

// serving returns a server's TLS config with a certificate for name from
// ca.
func serving(t *testing.T, ca *humustest.CA, name string) *tls.Config {
	t.Helper()
	return &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, name).TLS()}}
}

// settingsFor returns DefaultSettings for addr, trusting only ca.
func settingsFor(ca *humustest.CA, addr string) grpcclient.Settings {
	settings := grpcclient.DefaultSettings()
	settings.Addr = addr
	settings.TLS = &tls.Config{RootCAs: ca.Pool()}
	return settings
}

// emptyCall makes one EmptyCall.
func emptyCall(ctx context.Context, c testpb.TestServiceClient) error {
	_, err := c.EmptyCall(ctx, &testpb.Empty{})
	return err
}

// dialled records the address of every Dial through a Host.
type dialled struct {
	mu    sync.Mutex
	addrs []string
}

// recordDials returns h's Host, with a Dial that records each address in
// the returned dialled before dialling it.
func recordDials(h *humustest.Host) (humus.Host, *dialled) {
	d := &dialled{}
	host := h.Host()
	dial := host.Dial
	host.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		d.mu.Lock()
		d.addrs = append(d.addrs, address)
		d.mu.Unlock()
		return dial(ctx, network, address)
	}
	return host, d
}

func (d *dialled) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.addrs)
}

func TestClient_makes_a_unary_call(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		b := &backend{}
		settings := settingsFor(ca, "backend:9090")

		_, err := app{
			host:      h.Host(),
			backend:   b,
			serverTLS: serving(t, ca, "backend"),
			settings:  settings,
			work:      emptyCall,
		}.run(t)
		require.NoError(t, err)

		require.Equal(t, []rpc{{authority: "backend:9090", tls: true}}, b.seen(), "the call reaches the backend over TLS, with Addr as its :authority")
		require.Nil(t, settings.TLS.NextProtos, "the end user's TLS config is not changed")
	})
}

func TestClient_reads_a_stream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		b := &backend{}

		var (
			got  []byte
			took time.Duration
		)
		_, err := app{
			host:      h.Host(),
			backend:   b,
			serverTLS: serving(t, ca, "backend"),
			settings:  settingsFor(ca, "backend:9090"),
			work: func(ctx context.Context, c testpb.TestServiceClient) error {
				// 6s between messages: the stream outlives the 10s Timeout,
				// which applies only to unary calls.
				const interval = int32(6 * time.Second / time.Microsecond)
				start := time.Now()
				stream, err := c.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{
					ResponseParameters: []*testpb.ResponseParameters{{IntervalUs: interval}, {IntervalUs: interval}, {IntervalUs: interval}},
				})
				if err != nil {
					return err
				}
				for {
					resp, err := stream.Recv()
					if err != nil {
						took = time.Since(start)
						return err
					}
					got = append(got, resp.GetPayload().GetBody()...)
				}
			},
		}.run(t)

		require.ErrorIs(t, err, io.EOF)
		require.Equal(t, []byte{0, 1, 2}, got, "three messages, then io.EOF")
		require.Equal(t, 18*time.Second, took)
		require.EqualValues(t, 1, b.calls.Load())
	})
}

// servePlain serves b in plain text with a *grpc.Server, on a listener from
// h for address. The server is stopped when the test ends.
func servePlain(t *testing.T, h *humustest.Host, address string, b *backend) {
	t.Helper()
	l, err := h.Listen("tcp", address)
	require.NoError(t, err)

	srv := grpc.NewServer(grpc.Creds(insecure.NewCredentials()))
	testpb.RegisterTestServiceServer(srv, b)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		srv.Stop()
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("Serve: %v", err)
		}
	})
}

func TestClient_in_plain_text(t *testing.T) {
	t.Run("a nil TLS reaches a plaintext server", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			b := &backend{}
			servePlain(t, h, "sidecar:15001", b)

			settings := grpcclient.DefaultSettings()
			settings.Addr = "sidecar:15001"
			_, err := app{host: h.Host(), settings: settings, work: emptyCall}.run(t)
			require.NoError(t, err)

			require.Equal(t, []rpc{{authority: "sidecar:15001", tls: false}}, b.seen())
		})
	})

	t.Run("a nil TLS fails against a server that requires TLS", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			b := &backend{}

			settings := grpcclient.DefaultSettings()
			settings.Addr = "backend:9090"
			_, err := app{host: h.Host(), backend: b, serverTLS: serving(t, ca, "backend"), settings: settings, work: emptyCall}.run(t)
			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)
			require.Zero(t, b.calls.Load())
		})
	})
}

func TestClient_with_mTLS(t *testing.T) {
	// requiring returns a server's TLS config for backend that requires a
	// client certificate from ca, and verifies it.
	requiring := func(t *testing.T, ca *humustest.CA) *tls.Config {
		c := serving(t, ca, "backend")
		c.ClientAuth = tls.RequireAndVerifyClientCert
		c.ClientCAs = ca.Pool()
		return c
	}

	t.Run("a config with a client certificate reaches the server", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			b := &backend{}

			settings := settingsFor(ca, "backend:9090")
			settings.TLS.Certificates = []tls.Certificate{ca.Issue(t, "orders").TLS()}
			_, err := app{host: h.Host(), backend: b, serverTLS: requiring(t, ca), settings: settings, work: emptyCall}.run(t)
			require.NoError(t, err)

			require.Equal(t, []rpc{{authority: "backend:9090", tls: true, clientCN: "orders"}}, b.seen())
		})
	})

	t.Run("a config without one doesn't", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			b := &backend{}

			_, err := app{host: h.Host(), backend: b, serverTLS: requiring(t, ca), settings: settingsFor(ca, "backend:9090"), work: emptyCall}.run(t)
			require.Error(t, err)
			require.Zero(t, b.calls.Load())
		})
	})
}

// TestClient_dials_names_only_through_the_Host dials a name under .invalid,
// which RFC 6761 reserves so that it resolves nowhere: the call reaches the
// backend only because the name exists on the test Host. Were the name
// looked up with grpc-go's dns resolver, the lookup would crash the test
// binary inside the bubble.
func TestClient_dials_names_only_through_the_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		host, dials := recordDials(h)
		b := &backend{}

		_, err := app{
			host:        host,
			backend:     b,
			backendAddr: "carts.humus.invalid:9090",
			serverTLS:   serving(t, ca, "carts.humus.invalid"),
			settings:    settingsFor(ca, "carts.humus.invalid:9090"),
			work:        emptyCall,
		}.run(t)
		require.NoError(t, err)

		require.Equal(t, []string{"carts.humus.invalid:9090"}, dials.all())
		require.Equal(t, []rpc{{authority: "carts.humus.invalid:9090", tls: true}}, b.seen())
	})
}

// TestClient_uses_no_proxy shows a call reaching the backend directly, with
// the proxy variables set in the real environment. The Host's Dial sees
// the backend's address, and nothing else. In grpc-go v1.84.0 the Host's
// dialler alone already rules a proxy out, since grpc-go consults the
// proxy variables only for a client with no dialler of its own, so this
// test guards the behaviour rather than grpc.WithNoProxy: the option keeps
// it so should grpc-go change that rule.
func TestClient_uses_no_proxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("HTTP_PROXY", "http://proxy:3128")
	t.Setenv("NO_PROXY", "")

	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		host, dials := recordDials(h)
		b := &backend{}

		_, err := app{host: host, backend: b, serverTLS: serving(t, ca, "backend"), settings: settingsFor(ca, "backend:9090"), work: emptyCall}.run(t)
		require.NoError(t, err)

		require.EqualValues(t, 1, b.calls.Load())
		require.Equal(t, []string{"backend:9090"}, dials.all(), "the Client dials the backend directly")
	})
}

// retryPolicy is a service config that has grpc-go retry every
// TestService method answered UNAVAILABLE, up to 5 attempts.
const retryPolicy = `{"methodConfig": [{
	"name": [{"service": "grpc.testing.TestService"}],
	"retryPolicy": {
		"maxAttempts": 5,
		"initialBackoff": "0.1s",
		"maxBackoff": "1s",
		"backoffMultiplier": 2,
		"retryableStatusCodes": ["UNAVAILABLE"]
	}
}]}`

// unavailable answers every call with codes.Unavailable.
func unavailable(context.Context) error {
	return status.Error(codes.Unavailable, "not now")
}

func TestNewClient_opts(t *testing.T) {
	t.Run("transport credentials in opts have no effect", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			b := &backend{}

			_, err := app{
				host:      h.Host(),
				backend:   b,
				serverTLS: serving(t, ca, "backend"),
				settings:  settingsFor(ca, "backend:9090"),
				opts:      []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
				work:      emptyCall,
			}.run(t)
			require.NoError(t, err)

			require.Equal(t, []rpc{{authority: "backend:9090", tls: true}}, b.seen())
		})
	})

	t.Run("a service config with a retryPolicy adds no retries", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			b := &backend{answer: unavailable}

			_, err := app{
				host:      h.Host(),
				backend:   b,
				serverTLS: serving(t, ca, "backend"),
				settings:  settingsFor(ca, "backend:9090"),
				opts:      []grpc.DialOption{grpc.WithDefaultServiceConfig(retryPolicy)},
				work:      emptyCall,
			}.run(t)
			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)

			require.EqualValues(t, 1, b.calls.Load(), "the backend sees one call")
		})
	})

	// The control: the same service config does retry on a connection
	// without the Client's options, so the subtest above would catch a
	// Client that let it.
	t.Run("the same service config retries on a plain *grpc.ClientConn", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			b := &backend{answer: unavailable}
			p, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				srv, err := grpcserver.NewService(s, grpcserver.Settings{Addr: "backend:9090", TLS: serving(t, ca, "backend")})
				if err != nil {
					return nil, err
				}
				testpb.RegisterTestServiceServer(srv, b)
				return []humus.Service{srv}, nil
			}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
			<-h.Listening("backend:9090")

			conn := grpcservertest.NewClient(t, h, "backend:9090", ca.Pool(), grpc.WithDefaultServiceConfig(retryPolicy))
			require.Error(t, emptyCall(t.Context(), testpb.NewTestServiceClient(conn)))
			require.EqualValues(t, 5, b.calls.Load())

			require.NoError(t, conn.Close())
			<-p.Notified(syscall.SIGTERM)
			p.Signal(syscall.SIGTERM)
			st := <-exit
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
		})
	})
}

// blocked answers once its context is done, with the context's error.
func blocked(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestClient_Timeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		// deadline is the caller's, if it is not zero.
		deadline time.Duration
		want     time.Duration
	}{
		{name: "fails a call the backend never answers", want: 10 * time.Second},
		{name: "loses to a caller's earlier deadline", deadline: 3 * time.Second, want: 3 * time.Second},
		{name: "isn't extended by a caller's later deadline", deadline: time.Minute, want: 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				b := &backend{answer: blocked}

				var took time.Duration
				_, err := app{
					host:      h.Host(),
					backend:   b,
					serverTLS: serving(t, ca, "backend"),
					settings:  settingsFor(ca, "backend:9090"),
					work: func(ctx context.Context, c testpb.TestServiceClient) error {
						if tc.deadline != 0 {
							var cancel context.CancelFunc
							ctx, cancel = context.WithTimeout(ctx, tc.deadline)
							defer cancel()
						}
						start := time.Now()
						err := emptyCall(ctx, c)
						took = time.Since(start)
						return err
					},
				}.run(t)

				require.Equal(t, codes.DeadlineExceeded, status.Code(err), "err = %v", err)
				require.Equal(t, tc.want, took)
				require.EqualValues(t, 1, b.calls.Load())
			})
		})
	}
}

func TestClient_with_nothing_listening(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		host, dials := recordDials(h)

		settings := grpcclient.DefaultSettings()
		settings.Addr = "nowhere:9090"
		_, err := app{host: host, settings: settings, work: emptyCall}.run(t) // which requires exit 0

		require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)
		require.Contains(t, dials.all(), "nowhere:9090")
	})
}

// requireClosing requires err to be the error of a call on a Client whose
// connection is closed.
func requireClosing(t *testing.T, err error) {
	t.Helper()
	require.Equal(t, codes.Canceled, status.Code(err), "err = %v", err)
	require.ErrorContains(t, err, "grpc: the client connection is closing")
}

func TestClient_closes_its_connection_at_OnStop(t *testing.T) {
	t.Run("after the App exits", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			b := &backend{}

			client, err := app{host: h.Host(), backend: b, serverTLS: serving(t, ca, "backend"), settings: settingsFor(ca, "backend:9090"), work: emptyCall}.run(t)
			require.NoError(t, err, "the call while the App runs")

			requireClosing(t, emptyCall(t.Context(), testpb.NewTestServiceClient(client)))
			stream, err := testpb.NewTestServiceClient(client).StreamingOutputCall(t.Context(), &testpb.StreamingOutputCallRequest{})
			if err == nil {
				_, err = stream.Recv()
			}
			requireClosing(t, err)
			require.EqualValues(t, 1, b.calls.Load())
		})
	})

	t.Run("when Init fails after NewClient", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			errInit := errors.New("init failed")
			settings := grpcclient.DefaultSettings()
			settings.Addr = "backend:9090"

			var client *grpcclient.Client
			_, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				c, err := grpcclient.NewClient(s, settings)
				if err != nil {
					return nil, err
				}
				client = c
				return nil, errInit
			}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
			st := <-exit
			require.ErrorIs(t, st.Cause(), errInit)

			require.NotNil(t, client)
			requireClosing(t, emptyCall(t.Context(), testpb.NewTestServiceClient(client)))
		})
	})
}
