// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"crypto/tls"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/stats"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts,
// with certificates from a CA generated for each test. The backend is a
// grpcserver.Service in an App of its own, so a test can stop it while the
// Client's App runs. They pin what grpc-go's experimental Connect does: a
// grpc-go upgrade that changes it fails them.

// probeTimeout is the timeout of a check, as a probe's own timeout would
// end it.
const probeTimeout = 2 * time.Second

// rpcCounter is a stats handler that counts the RPCs the Client starts, and
// the connections it opens and that end.
type rpcCounter struct {
	rpcs, connsBegun, connsEnded atomic.Int32
}

func (c *rpcCounter) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }

func (c *rpcCounter) HandleRPC(_ context.Context, s stats.RPCStats) {
	if _, ok := s.(*stats.Begin); ok {
		c.rpcs.Add(1)
	}
}

func (c *rpcCounter) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }

func (c *rpcCounter) HandleConn(_ context.Context, s stats.ConnStats) {
	switch s.(type) {
	case *stats.ConnBegin:
		c.connsBegun.Add(1)
	case *stats.ConnEnd:
		c.connsEnded.Add(1)
	}
}

// startBackend serves a backend at backend:9090 on h, in an App of its own,
// with serverTLS, and waits until it listens. It returns a function that
// stops the backend, sending its client a GOAWAY, and requires it to exit
// 0.
func startBackend(t *testing.T, h *humustest.Host, serverTLS *tls.Config) (stop func()) {
	t.Helper()
	p, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, grpcserver.Settings{Addr: "backend:9090", TLS: serverTLS})
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, &backend{})
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening("backend:9090")
	return func() {
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}

// probed is the end user's App as the tests in this file run it: its Init
// builds a Client with settings, and a stats handler that counts its RPCs,
// and returns a health Service whose readiness is the Client's Connected.
type probed struct {
	client  *grpcclient.Client
	health  *health.Service
	counter *rpcCounter
	tel     *humustest.Telemetry

	stop func()
}

// startProbed starts the App on host, and waits until it runs.
func startProbed(t *testing.T, host humus.Host, settings grpcclient.Settings) *probed {
	t.Helper()
	a := &probed{counter: &rpcCounter{}, tel: humustest.NewTelemetry()}
	p, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		c, err := grpcclient.NewClient(s, settings, grpc.WithStatsHandler(a.counter))
		if err != nil {
			return nil, err
		}
		a.client = c
		a.health = health.NewService(health.Readiness(c.Connected()))
		return []humus.Service{a.health}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(a.tel.Builder())))
	<-p.Notified(syscall.SIGTERM)
	synctest.Wait()
	a.stop = func() {
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
	return a
}

// check checks m with a context that ends after probeTimeout, and returns
// what it reported and how long it took.
func check(t *testing.T, m health.Monitor) (bool, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), probeTimeout)
	defer cancel()
	start := time.Now()
	ok, err := m.Healthy(ctx)
	require.NoError(t, err, "Connected never returns an error")
	return ok, time.Since(start)
}

// requireNoRPC requires that a has started no RPC, no span, and no log
// record beyond the logs it had already emitted.
func (a *probed) requireNoRPC(t *testing.T, logs int) {
	t.Helper()
	require.Zero(t, a.counter.rpcs.Load(), "the Client started an RPC")
	for _, s := range a.tel.Spans() {
		require.NotEqual(t, otelgrpc.ScopeName, s.InstrumentationScope().Name, "otelgrpc started a span")
	}
	require.Len(t, a.tel.Logs(), logs, "the check logged")
}

func TestClient_Connected(t *testing.T) {
	t.Run("with nothing listening, the first check starts the connection, and is false when its context ends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			host, dials := recordDials(h)
			ca := humustest.NewCA(t)
			a := startProbed(t, host, settingsFor(ca, "backend:9090"))
			defer a.stop()

			require.Equal(t, connectivity.Idle, grpcclient.State(a.client), "NewClient dials nothing")
			require.Empty(t, dials.all())

			logs := len(a.tel.Logs())
			ok, took := check(t, a.client.Connected())
			require.False(t, ok)
			require.Equal(t, probeTimeout, took, "the check waits until its context ends")
			require.Contains(t, dials.all(), "backend:9090", "the check started the connection")
			a.requireNoRPC(t, logs)
		})
	})

	t.Run("with a server listening, the first check is true at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			stop := startBackend(t, h, serving(t, ca, "backend"))
			defer stop()
			a := startProbed(t, h.Host(), settingsFor(ca, "backend:9090"))
			defer a.stop()

			require.Equal(t, connectivity.Idle, grpcclient.State(a.client))
			logs := len(a.tel.Logs())
			ok, took := check(t, a.client.Connected())
			require.True(t, ok)
			require.Zero(t, took)
			require.Equal(t, connectivity.Ready, grpcclient.State(a.client))
			require.EqualValues(t, 1, a.counter.connsBegun.Load())
			a.requireNoRPC(t, logs)
		})
	})

	t.Run("it latches: once the server's GOAWAY ends the connection, it is still true, and so is a second monitor", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			stop := startBackend(t, h, serving(t, ca, "backend"))
			a := startProbed(t, h.Host(), settingsFor(ca, "backend:9090"))
			defer a.stop()
			logs := len(a.tel.Logs())

			first := a.client.Connected()
			ok, _ := check(t, first)
			require.True(t, ok)

			stop()
			synctest.Wait()
			require.EqualValues(t, 1, a.counter.connsEnded.Load(), "the GOAWAY ended the connection")
			require.NotEqual(t, connectivity.Ready, grpcclient.State(a.client))

			ok, took := check(t, first)
			require.True(t, ok, "a later outage doesn't make it unhealthy")
			require.Zero(t, took)

			ok, took = check(t, a.client.Connected())
			require.True(t, ok, "a second monitor reads the same latch")
			require.Zero(t, took)
			require.NotEqual(t, connectivity.Ready, grpcclient.State(a.client))
			a.requireNoRPC(t, logs)
		})
	})

	t.Run("against a server it doesn't trust, it is false", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			stop := startBackend(t, h, serving(t, humustest.NewCA(t), "backend"))
			defer stop()
			a := startProbed(t, h.Host(), settingsFor(humustest.NewCA(t), "backend:9090"))
			defer a.stop()

			ok, took := check(t, a.client.Connected())
			require.False(t, ok)
			require.Equal(t, probeTimeout, took)
			require.Equal(t, connectivity.TransientFailure, grpcclient.State(a.client))
			require.Zero(t, a.counter.rpcs.Load())
		})
	})

	t.Run("in plain text to a TLS server, it is false", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			stop := startBackend(t, h, serving(t, ca, "backend"))
			defer stop()
			settings := grpcclient.DefaultSettings()
			settings.Addr = "backend:9090"
			a := startProbed(t, h.Host(), settings)
			defer a.stop()

			ok, took := check(t, a.client.Connected())
			require.False(t, ok)
			require.Equal(t, probeTimeout, took)
			require.Equal(t, connectivity.TransientFailure, grpcclient.State(a.client))
			require.Zero(t, a.counter.rpcs.Load())
		})
	})

	t.Run("once OnStop has closed the connection, it is false", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			stop := startBackend(t, h, serving(t, ca, "backend"))
			defer stop()
			a := startProbed(t, h.Host(), settingsFor(ca, "backend:9090"))

			m := a.client.Connected()
			ok, _ := check(t, m)
			require.True(t, ok)

			a.stop()
			require.Equal(t, connectivity.Shutdown, grpcclient.State(a.client))
			ok, took := check(t, m)
			require.False(t, ok, "a monitor that was healthy")
			require.Zero(t, took)
			ok, _ = check(t, a.client.Connected())
			require.False(t, ok, "a monitor taken after the close")
		})
	})

	t.Run("closed before it ever connected, it is false at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			a := startProbed(t, h.Host(), settingsFor(humustest.NewCA(t), "backend:9090"))
			a.stop()

			ok, took := check(t, a.client.Connected())
			require.False(t, ok)
			require.Zero(t, took)
		})
	})

	t.Run("concurrent checks agree, before and after the connection is ready", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			a := startProbed(t, h.Host(), settingsFor(ca, "backend:9090"))
			defer a.stop()

			// checkAll runs n checks at once, each with its own monitor
			// and a context that ends after timeout, and returns what each
			// reported. start, if it isn't nil, runs while they wait.
			checkAll := func(timeout time.Duration, start func()) []bool {
				const n = 8
				got := make([]bool, n)
				var wg sync.WaitGroup
				for i := range n {
					wg.Go(func() {
						ctx, cancel := context.WithTimeout(t.Context(), timeout)
						defer cancel()
						ok, err := a.client.Connected().Healthy(ctx)
						require.NoError(t, err)
						got[i] = ok
					})
				}
				if start != nil {
					synctest.Wait()
					start()
				}
				wg.Wait()
				return got
			}

			for _, ok := range checkAll(probeTimeout, nil) {
				require.False(t, ok, "nothing listens yet")
			}

			var stop func()
			for _, ok := range checkAll(time.Minute, func() { stop = startBackend(t, h, serving(t, ca, "backend")) }) {
				require.True(t, ok, "the backend started while the checks waited")
			}
			defer stop()

			for _, ok := range checkAll(probeTimeout, nil) {
				require.True(t, ok, "latched")
			}
			require.Zero(t, a.counter.rpcs.Load())
		})
	})
}

func TestInit_with_Connected_in_its_readiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		a := startProbed(t, h.Host(), settingsFor(ca, "backend:9090"))
		defer a.stop()
		readiness := a.health.Probes().Readiness

		ok, _ := check(t, readiness)
		require.False(t, ok, "unready while nothing listens on Addr")

		stop := startBackend(t, h, serving(t, ca, "backend"))
		defer stop()
		// grpc-go dials again after its connection backoff, which starts
		// at 1s and grows, so the probes that follow are a second apart,
		// as a kubelet's are a period apart, until one finds it ready.
		for range 120 {
			if ok, _ = check(t, readiness); ok {
				break
			}
			time.Sleep(time.Second)
		}
		require.True(t, ok, "ready once the backend serves")
	})
}
