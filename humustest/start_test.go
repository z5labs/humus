// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	humusfault "github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// fakeService is a Service whose Serve and Shutdown are the functions it
// holds, as the sketches in Proposal: humus.Main write it.
type fakeService struct {
	serve    func(ctx context.Context) error
	shutdown func(ctx context.Context) error
}

func (s *fakeService) Serve(ctx context.Context) error    { return s.serve(ctx) }
func (s *fakeService) Shutdown(ctx context.Context) error { return s.shutdown(ctx) }

// noServices is an Init that returns no Services.
func noServices(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error) { return nil, nil }

// services returns an Init that returns svcs.
func services(svcs ...humus.Service) func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error) {
	return func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error) {
		return svcs, nil
	}
}

// untilShutdown returns a Service that keeps serving until Shutdown, as an
// HTTP server does, and calls shutdown, if it is not nil, with Shutdown's
// context.
func untilShutdown(shutdown func(ctx context.Context)) *fakeService {
	stopped := make(chan struct{})
	return &fakeService{
		serve: func(context.Context) error { <-stopped; return nil },
		shutdown: func(ctx context.Context) error {
			if shutdown != nil {
				shutdown(ctx)
			}
			close(stopped)
			return nil
		},
	}
}

// stopsOnContext is a Service that returns once Serve's context is done, as
// a job or a queue consumer does, and counts its Serve calls.
type stopsOnContext struct {
	served atomic.Int32
}

func (s *stopsOnContext) Serve(ctx context.Context) error {
	s.served.Add(1)
	<-ctx.Done()
	return nil
}

func (*stopsOnContext) Shutdown(context.Context) error { return nil }

// sigterm sends p SIGTERM once the App is listening for it and every
// goroutine in the bubble is blocked, and returns the fake instant it was
// sent.
func sigterm(p *bedrocktest.Process) time.Time {
	<-p.Notified(syscall.SIGTERM)
	synctest.Wait()
	at := time.Now()
	p.Signal(syscall.SIGTERM)
	return at
}

// The first revision of Proposal: humus.Main's two sketch tests, as
// written there.

func TestApp_the_drain_ends_once_every_Serve_has_returned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var shutdownAt time.Duration
		svc := &fakeService{
			serve: func(ctx context.Context) error {
				<-ctx.Done()
				time.Sleep(2 * time.Second) // finishing the work in flight
				return nil
			},
			shutdown: func(context.Context) error {
				shutdownAt = time.Since(start)
				return nil
			},
		}

		p, status := humustest.Start(t, humustest.NewHost(t).Host(),
			func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error) {
				return []humus.Service{svc}, nil
			},
			humustest.WithApp(humus.WithDrain(humus.DrainDelay(5*time.Second))),
		)
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		require.Zero(t, (<-status).Code())
		require.Equal(t, 2*time.Second, shutdownAt) // not the drain's 5s
	})
}

func TestApp_a_Service_that_serves_until_Shutdown_gets_the_whole_drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var shutdownAt time.Duration
		stopped := make(chan struct{})
		svc := &fakeService{
			serve: func(context.Context) error { <-stopped; return nil },
			shutdown: func(context.Context) error {
				shutdownAt = time.Since(start)
				close(stopped)
				return nil
			},
		}

		p, status := humustest.Start(t, humustest.NewHost(t).Host(),
			func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error) {
				return []humus.Service{svc}, nil
			},
			humustest.WithApp(humus.WithDrain(humus.DrainDelay(5*time.Second))),
		)
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		require.Zero(t, (<-status).Code())
		require.Equal(t, 5*time.Second, shutdownAt)
	})
}

// TestStart_with_no_options_runs_Mains_defaults_on_a_test_Host runs an App
// with Start's defaults alone: a 20s grace period, no drain, a humustest
// Host and humus.Main's telemetry default, whose log lines reach the
// Process's stdout as JSON.
func TestStart_with_no_options_runs_Mains_defaults_on_a_test_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var listener net.Listener
		var shutdownAt time.Time
		var grace time.Duration
		svc := untilShutdown(func(ctx context.Context) {
			shutdownAt = time.Now()
			deadline, ok := ctx.Deadline()
			require.True(t, ok, "Shutdown's context has the grace period's deadline")
			grace = time.Until(deadline)
		})
		init := func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
			ln, err := h.ListenTCP(ctx, "tcp", ":8080")
			if err != nil {
				return nil, err
			}
			s.OnStop(func(context.Context) error { return ln.Close() })
			listener = ln
			return []humus.Service{svc}, nil
		}

		p, status := humustest.Start(t, humustest.NewHost(t).Host(), init)
		at := sigterm(p)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, 20*time.Second, grace, "the grace period")
		require.Zero(t, shutdownAt.Sub(at), "no drain: Shutdown at the signal's instant")
		_, real := listener.(*net.TCPListener)
		require.False(t, real, "the App listened on a test Host, not the OS")
		require.Empty(t, p.Stderr())

		first, _, _ := strings.Cut(p.Stdout(), "\n")
		var rec struct {
			Severity string `json:"severity_text"`
			Body     string `json:"body"`
		}
		require.NoError(t, json.Unmarshal([]byte(first), &rec), "first stdout line %q", first)
		require.Equal(t, "INFO", rec.Severity)
		require.Equal(t, "serving", rec.Body)
	})
}

func TestStart_has_no_drain_by_default(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var shutdownAt time.Time
		p, status := humustest.Start(t, humustest.NewHost(t).Host(), services(untilShutdown(func(context.Context) {
			shutdownAt = time.Now()
		})), humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))
		at := sigterm(p)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Zero(t, shutdownAt.Sub(at), "Shutdown at the same fake instant as the signal")
	})
}

func TestStart_uses_the_host_and_the_telemetry_it_is_given(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		var served atomic.Bool
		svc := &fakeService{
			serve: func(ctx context.Context) error {
				served.Store(true)
				<-ctx.Done()
				return nil
			},
			shutdown: func(context.Context) error { return nil },
		}
		init := func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
			ln, err := h.ListenTCP(ctx, "tcp", "api.internal:8080")
			if err != nil {
				return nil, err
			}
			s.OnStop(func(context.Context) error { return ln.Close() })
			return []humus.Service{svc}, nil
		}
		listening := h.Listening("api.internal:8080")

		p, status := humustest.Start(t, h.Host(), init,
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
		)
		<-listening // on h, not on the Host Start built
		sigterm(p)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.True(t, served.Load())
		require.NotEmpty(t, tel.Logs(), "the lifecycle logged through tel")
		require.Empty(t, p.Stdout(), "nothing reached the telemetry default")
	})
}

func TestWithApp_given_twice_applies_both_in_order_and_a_later_option_wins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var grace time.Duration
		svc := untilShutdown(func(ctx context.Context) {
			deadline, _ := ctx.Deadline()
			grace = time.Until(deadline)
		})

		p, status := humustest.Start(t, humustest.NewHost(t).Host(), services(svc),
			humustest.WithApp(humus.WithShutdownGracePeriod(time.Second), humus.WithShutdownGracePeriod(2*time.Second)),
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder), humus.WithShutdownGracePeriod(3*time.Second)),
		)
		sigterm(p)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, 3*time.Second, grace, "the later option wins")
	})
}

// TestStart_runs_a_wrapped_init_on_a_wrapped_host wraps the host it gives
// Start, to see what the App listens on, and wraps init, to run a Service
// of its own next to the end user's, as a test does now that no Option can
// wrap either.
func TestStart_runs_a_wrapped_init_on_a_wrapped_host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		own := &stopsOnContext{}
		added := &stopsOnContext{}
		var listened []string
		th := humustest.NewHost(t)
		h := humus.Host{
			IPNetwork: hosttest.Seams{Network: th, OnListenTCP: func(ctx context.Context, network, address string) (net.Listener, error) {
				listened = append(listened, address)
				return th.ListenTCP(ctx, network, address)
			}},
			Filesystem: th.Filesystem(),
		}
		withAdded := func(init humus.Init[humus.Host]) humus.Init[humus.Host] {
			return func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
				svcs, err := init(ctx, s, h)
				if err != nil {
					return nil, err
				}
				return append(svcs, added), nil
			}
		}
		init := func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
			ln, err := h.ListenTCP(ctx, "tcp", ":9090")
			if err != nil {
				return nil, err
			}
			s.OnStop(func(context.Context) error { return ln.Close() })
			return []humus.Service{own}, nil
		}

		p, status := humustest.Start(t, h, withAdded(init),
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		sigterm(p)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, []string{":9090"}, listened, "the wrapped host")
		require.EqualValues(t, 1, own.served.Load(), "Serve calls on the end user's Service")
		require.EqualValues(t, 1, added.served.Load(), "Serve calls on the added Service")
	})
}

// ownHost is an App's own host: the test host's network, embedded so that
// ownHost is a humus.Network, and a name humus doesn't know.
type ownHost struct {
	humus.IPNetwork
	name string
}

// TestStart_runs_an_App_on_its_own_host gives Init and Faults the host
// Start is given, of the App's own type.
func TestStart_runs_an_App_on_its_own_host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var faultsName, initName string
		faults := func(_ context.Context, _ *humus.Setup, h ownHost) (humusfault.Settings, error) {
			faultsName = h.name
			return humusfault.Settings{}, errors.New("no failure injection")
		}
		init := func(_ context.Context, _ *humus.Setup, h ownHost) ([]humus.Service, error) {
			initName = h.name
			return nil, nil
		}

		_, status := humustest.Start(t, ownHost{IPNetwork: humustest.NewHost(t), name: "own"}, init,
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder), humus.WithFaults(faults)),
		)
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, "own", faultsName, "Faults got the App's host")
		require.Empty(t, initName, "Init never ran after Faults failed")
	})
}

// TestStart_fails_the_test_when_Apply_fails gives WithApp an Option made
// for an App on another host type, and the zero Option.
func TestStart_fails_the_test_when_Apply_fails(t *testing.T) {
	ownFaults := func(context.Context, *humus.Setup, ownHost) (humusfault.Settings, error) {
		return humusfault.DefaultSettings(), nil
	}
	cases := []struct {
		name string
		opt  humus.Option
		want string
	}{
		{"an Option for another host type", humus.WithFaults(ownFaults), "humustest: Start: humus: WithFaults was made for an App on humustest_test.ownHost, not on humus.Host"},
		{"the zero Option", humus.Option{}, "humustest: Start: humus: option 1 is the zero Option: only humus's With functions make an Option"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tb := &fatalTB{}
				done := make(chan struct{})
				go func() {
					defer close(done)
					humustest.Start(tb, humustest.NewHost(t).Host(), noServices,
						humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder), c.opt),
					)
					t.Error("Start returned after Apply failed")
				}()
				<-done

				require.Equal(t, c.want, tb.fatal)
			})
		})
	}
}

func TestStart_the_process_options_reach_Setup_Process(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var args []string
		var bucket, stdin string
		init := func(_ context.Context, s *humus.Setup, _ humus.Host) ([]humus.Service, error) {
			args = s.Process.Args
			v, err := s.Process.Environment.Value("EXPORT_BUCKET")
			if err != nil {
				return nil, err
			}
			bucket = v
			b, err := io.ReadAll(s.Process.Stdin)
			if err != nil {
				return nil, err
			}
			stdin = string(b)
			return nil, nil
		}

		_, status := humustest.Start(t, humustest.NewHost(t).Host(), init,
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{"EXPORT_BUCKET": "nightly"}),
			humustest.WithArgs("-dry-run", "rows.csv"),
			humustest.WithStdin(strings.NewReader("row 1\nrow 2\n")),
		)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, []string{bedrocktest.ProgramName, "-dry-run", "rows.csv"}, args)
		require.Equal(t, "nightly", bucket)
		require.Equal(t, "row 1\nrow 2\n", stdin)
	})
}

func TestStart_the_last_of_each_process_option_wins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var env bedrock.Environment
		var args []string
		var stdin string
		init := func(_ context.Context, s *humus.Setup, _ humus.Host) ([]humus.Service, error) {
			env = s.Process.Environment
			args = s.Process.Args
			b, err := io.ReadAll(s.Process.Stdin)
			if err != nil {
				return nil, err
			}
			stdin = string(b)
			return nil, nil
		}

		_, status := humustest.Start(t, humustest.NewHost(t).Host(), init,
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{"EXPORT_BUCKET": "nightly", "EXPORT_REGION": "eu"}),
			humustest.WithArgs("first"),
			humustest.WithStdin(strings.NewReader("first")),
			humustest.WithEnv(map[string]string{"EXPORT_BUCKET": "hourly"}),
			humustest.WithArgs("second"),
			humustest.WithStdin(strings.NewReader("second")),
		)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		v, err := env.Value("EXPORT_BUCKET")
		require.NoError(t, err)
		require.Equal(t, "hourly", v)
		_, err = env.Value("EXPORT_REGION")
		var notFound bedrock.ValueNotFoundError
		require.True(t, errors.As(err, &notFound), "the second WithEnv replaced the first, not merged: %v", err)
		require.Equal(t, []string{bedrocktest.ProgramName, "second"}, args)
		require.Equal(t, "second", stdin)
	})
}

func TestStart_panics(t *testing.T) {
	require.PanicsWithValue(t, "humustest: Start called with a nil init", func() {
		humustest.Start(t, humustest.NewHost(t).Host(), nil)
	})
	require.PanicsWithValue(t, "humustest: Start called with a nil Option", func() {
		humustest.Start(t, humustest.NewHost(t).Host(), noServices, humustest.WithArgs("x"), nil)
	})
	require.PanicsWithValue(t, "humustest: WithStdin called with a nil io.Reader", func() {
		humustest.WithStdin(nil)
	})
}

// helloService serves hello over HTTP on ":8080" of its host, as the end
// user's HTTP Service would. It keeps serving until Shutdown.
type helloService struct {
	host humus.Host
	srv  http.Server
}

func (h *helloService) Serve(ctx context.Context) error {
	ln, err := h.host.ListenTCP(ctx, "tcp", ":8080")
	if err != nil {
		return err
	}
	h.srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	})
	if err := h.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (h *helloService) Shutdown(ctx context.Context) error { return h.srv.Shutdown(ctx) }

// helloInit is the end user's Init of the package doc's example.
func helloInit(_ context.Context, _ *humus.Setup, h humus.Host) ([]humus.Service, error) {
	return []humus.Service{&helloService{host: h}}, nil
}

// TestInit_serves_hello is the package doc's example.
func TestInit_serves_hello(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		p, status := humustest.Start(t, h.Host(), helloInit)

		// Wait until every goroutine in the bubble is blocked, which
		// includes the App blocked in Accept on :8080.
		synctest.Wait()

		resp, err := h.HTTPClient().Get("http://localhost:8080/")
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || string(b) != "hello" {
			t.Errorf("body = %q, %v, want hello", b, err)
		}

		p.Signal(syscall.SIGTERM)
		st := <-status
		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
	})
}

// TestStart_rejects_a_Host_with_a_nil_field shows that an App whose Host
// lacks its network or its filesystem exits before telemetry or Init, with
// a message naming the field, rather than panic the first time a runtime
// reaches for it.
func TestStart_rejects_a_Host_with_a_nil_field(t *testing.T) {
	cases := []struct {
		name string
		host func(h *humustest.Host) humus.Host
		want string
	}{
		{
			name: "IPNetwork",
			host: func(h *humustest.Host) humus.Host { return humus.Host{Filesystem: h.Filesystem()} },
			want: "humus: Host.IPNetwork is nil: build the Host with OSHost, or from the test host's parts",
		},
		{
			name: "Filesystem",
			host: func(h *humustest.Host) humus.Host { return humus.Host{IPNetwork: h} },
			want: "humus: Host.Filesystem is nil: build the Host with OSHost, or from the test host's parts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var built, ran bool
				builder := func(context.Context, bedrock.Process, telemetry.Network) (telemetry.SDK, error) {
					built = true
					return telemetry.Noop(), nil
				}
				init := func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error) {
					ran = true
					return nil, nil
				}

				p, status := humustest.Start(t, tc.host(humustest.NewHost(t)), init,
					humustest.WithApp(
						humus.WithTelemetry(builder),
					),
				)
				st := <-status

				require.Equal(t, humus.ExitFailure, st.Code())
				require.EqualError(t, st.Cause(), tc.want)
				require.Equal(t, tc.want+"\n", p.Stderr())
				require.False(t, built, "telemetry was built")
				require.False(t, ran, "Init ran")
			})
		})
	}
}
