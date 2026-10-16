// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package health_test

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// Service satisfies humus.Service without health importing humus.
var _ humus.Service = (*health.Service)(nil)

func TestServiceIsNotReadyDuringTheDrainDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := health.NewService()
		readiness := hs.Probes().Readiness
		app := humus.App{
			Host:                humustest.NewHost(t).Host(),
			Telemetry:           telemetry.NoopBuilder,
			ShutdownGracePeriod: 10 * time.Second,
			DrainDelay:          5 * time.Second,
			Init: func(context.Context, *humus.Setup) ([]humus.Service, error) {
				return []humus.Service{hs}, nil
			},
		}

		p := bedrocktest.NewProcess(t)
		status := make(chan bedrock.ExitStatus, 1)
		go func() { status <- app.Run(t.Context(), p.Process()) }()

		<-p.Notified(syscall.SIGTERM)
		synctest.Wait() // Serve has started
		ready, err := readiness.Healthy(t.Context())
		require.NoError(t, err)
		require.True(t, ready)

		p.Signal(syscall.SIGTERM)
		synctest.Wait() // shutdown has started; the drain delay is running
		ready, err = readiness.Healthy(t.Context())
		require.NoError(t, err)
		require.False(t, ready)

		require.Zero(t, (<-status).Code())
	})
}

func TestService_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		return health.NewService()
	})
}

// ready checks m and fails the test if it returns an error.
func ready(t *testing.T, m health.Monitor) bool {
	t.Helper()
	healthy, err := m.Healthy(t.Context())
	require.NoError(t, err)
	return healthy
}

// serve calls Serve on hs in a new goroutine, waits for it to block, and
// returns the channel its result arrives on.
func serve(t *testing.T, ctx context.Context, hs *health.Service) <-chan error {
	t.Helper()
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ctx) }()
	synctest.Wait()
	select {
	case err := <-served:
		t.Fatalf("Serve returned %v before anything stopped it", err)
	default:
	}
	return served
}

// returned fails the test unless Serve has returned nil on served.
func returned(t *testing.T, served <-chan error) {
	t.Helper()
	synctest.Wait()
	select {
	case err := <-served:
		require.NoError(t, err)
	default:
		t.Fatal("Serve has not returned")
	}
}

func TestService_readiness(t *testing.T) {
	t.Run("is false before Serve", func(t *testing.T) {
		hs := health.NewService()
		require.False(t, ready(t, hs.Probes().Readiness))
	})

	t.Run("is true while Serve runs", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			hs := health.NewService()
			readiness := hs.Probes().Readiness // before Serve
			served := serve(t, t.Context(), hs)

			require.True(t, ready(t, readiness))
			require.True(t, ready(t, hs.Probes().Readiness))

			require.NoError(t, hs.Shutdown(t.Context()))
			returned(t, served)
		})
	})

	t.Run("is false for good after Shutdown, though Serve's context is live", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			hs := health.NewService()
			readiness := hs.Probes().Readiness
			served := serve(t, t.Context(), hs)
			require.True(t, ready(t, readiness))

			require.NoError(t, hs.Shutdown(t.Context()))
			require.False(t, ready(t, readiness))
			returned(t, served)
			require.False(t, ready(t, readiness))

			// A Serve after Shutdown returns at once and leaves it not
			// ready.
			require.NoError(t, hs.Serve(t.Context()))
			require.False(t, ready(t, readiness))
			require.False(t, ready(t, hs.Probes().Readiness))
		})
	})

	t.Run("is false from the moment Serve's context is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			hs := health.NewService()
			readiness := hs.Probes().Readiness
			ctx, cancel := context.WithCancel(t.Context())
			served := serve(t, ctx, hs)
			require.True(t, ready(t, readiness))

			cancel()
			require.False(t, ready(t, readiness))
			returned(t, served)
			require.False(t, ready(t, readiness))

			// A Serve after an earlier Serve has returned returns at once
			// and leaves it not ready.
			require.NoError(t, hs.Serve(t.Context()))
			require.False(t, ready(t, readiness))

			require.NoError(t, hs.Shutdown(t.Context()))
			require.False(t, ready(t, readiness))
		})
	})

	t.Run("is false for good after Shutdown before Serve", func(t *testing.T) {
		hs := health.NewService()
		require.NoError(t, hs.Shutdown(t.Context()))
		require.NoError(t, hs.Serve(t.Context()))
		require.False(t, ready(t, hs.Probes().Readiness))
	})
}

func TestService_Shutdown_with_a_done_context_while_serving(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := health.NewService()
		served := serve(t, t.Context(), hs)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := hs.Shutdown(ctx)
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, ready(t, hs.Probes().Readiness))
		returned(t, served)
	})
}

// recorded returns a monitor reporting r, which appends name to checked
// when it is checked.
func recorded(name string, r result, checked *[]string) health.Monitor {
	return health.MonitorFunc(func(context.Context) (bool, error) {
		*checked = append(*checked, name)
		return r.healthy, r.err
	})
}

func TestService_Probes_Readiness(t *testing.T) {
	t.Run("checks its own state first, then its monitors in order", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var checked []string
			hs := health.NewService(
				health.Readiness(recorded("first", healthy, &checked)),
				health.Readiness(recorded("second", healthy, &checked)),
			)
			readiness := hs.Probes().Readiness

			// Not ready on its own state, so no monitor is checked.
			require.False(t, ready(t, readiness))
			require.Empty(t, checked)

			served := serve(t, t.Context(), hs)
			require.True(t, ready(t, readiness))
			require.Equal(t, []string{"first", "second"}, checked)

			require.NoError(t, hs.Shutdown(t.Context()))
			returned(t, served)
		})
	})

	t.Run("is false with the error of a failing monitor while serving", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			errPing := errors.New("ping failed")
			var checked []string
			hs := health.NewService(
				health.Readiness(recorded("first", healthy, &checked)),
				health.Readiness(recorded("failing", result{err: errPing}, &checked)),
				health.Readiness(recorded("last", healthy, &checked)),
			)
			served := serve(t, t.Context(), hs)

			healthy, err := hs.Probes().Readiness.Healthy(t.Context())
			require.ErrorIs(t, err, errPing)
			require.False(t, healthy)
			require.Equal(t, []string{"first", "failing"}, checked)

			require.NoError(t, hs.Shutdown(t.Context()))
			returned(t, served)
		})
	})

	t.Run("is false when a monitor is unhealthy while serving", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var dependency health.Binary
			hs := health.NewService(health.Readiness(&dependency))
			served := serve(t, t.Context(), hs)

			require.False(t, ready(t, hs.Probes().Readiness))
			dependency.MarkHealthy()
			require.True(t, ready(t, hs.Probes().Readiness))

			require.NoError(t, hs.Shutdown(t.Context()))
			returned(t, served)
		})
	})
}

func TestService_Probes_Liveness(t *testing.T) {
	t.Run("is healthy with no monitors", func(t *testing.T) {
		hs := health.NewService()
		require.True(t, ready(t, hs.Probes().Liveness))
	})

	t.Run("does not depend on the Service's own state", func(t *testing.T) {
		hs := health.NewService()
		require.NoError(t, hs.Shutdown(t.Context()))
		require.True(t, ready(t, hs.Probes().Liveness))
	})

	t.Run("checks its monitors in order", func(t *testing.T) {
		var checked []string
		hs := health.NewService(
			health.Liveness(recorded("first", healthy, &checked)),
			health.Liveness(recorded("second", healthy, &checked)),
		)
		require.True(t, ready(t, hs.Probes().Liveness))
		require.Equal(t, []string{"first", "second"}, checked)
	})

	t.Run("is unhealthy when a monitor is", func(t *testing.T) {
		var deadlocked health.Binary
		deadlocked.MarkHealthy()
		hs := health.NewService(
			health.Liveness(health.And()),
			health.Liveness(&deadlocked),
		)
		require.True(t, ready(t, hs.Probes().Liveness))

		deadlocked.MarkUnhealthy()
		require.False(t, ready(t, hs.Probes().Liveness))
	})

	t.Run("returns a monitor's error", func(t *testing.T) {
		errCheck := errors.New("check failed")
		hs := health.NewService(health.Liveness(health.MonitorFunc(func(context.Context) (bool, error) {
			return false, errCheck
		})))
		healthy, err := hs.Probes().Liveness.Healthy(t.Context())
		require.ErrorIs(t, err, errCheck)
		require.False(t, healthy)
	})
}

func TestService_Probes_is_race_free(t *testing.T) {
	var dependency health.Binary
	dependency.MarkHealthy()
	hs := health.NewService(
		health.Liveness(&dependency),
		health.Readiness(&dependency),
	)
	probes := hs.Probes()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var checkers sync.WaitGroup
	for range 8 {
		checkers.Go(func() {
			for range 1000 {
				p := hs.Probes()
				_, _ = p.Readiness.Healthy(ctx)
				_, _ = p.Liveness.Healthy(ctx)
				_, _ = probes.Readiness.Healthy(ctx)
				_, _ = probes.Liveness.Healthy(ctx)
			}
		})
	}

	served := make(chan error, 1)
	go func() { served <- hs.Serve(ctx) }()
	var shutdowns sync.WaitGroup
	for range 2 {
		shutdowns.Go(func() {
			if err := hs.Shutdown(ctx); err != nil {
				t.Errorf("Shutdown returned %v, want nil", err)
			}
		})
	}

	shutdowns.Wait()
	require.NoError(t, <-served)
	checkers.Wait()
	require.False(t, ready(t, probes.Readiness))
}

func TestLiveness_panics_on_a_nil_Monitor(t *testing.T) {
	require.PanicsWithValue(t, "health: Liveness called with a nil Monitor", func() {
		health.Liveness(nil)
	})
}

func TestReadiness_panics_on_a_nil_Monitor(t *testing.T) {
	require.PanicsWithValue(t, "health: Readiness called with a nil Monitor", func() {
		health.Readiness(nil)
	})
}

func TestService_returned_from_Init(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reachable bool // a stand-in for a database
		ping := func(ctx context.Context) error {
			if !reachable {
				return errors.New("connection refused")
			}
			return nil
		}

		var working health.Binary // marked unhealthy when only a restart would fix the process
		working.MarkHealthy()

		probes := make(chan health.Probes, 1)
		app := humus.App{
			Host:                humustest.NewHost(t).Host(),
			Telemetry:           telemetry.NoopBuilder,
			ShutdownGracePeriod: 10 * time.Second,
			DrainDelay:          5 * time.Second,
			Init: func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
				hs := health.NewService(
					health.Liveness(&working),
					health.Readiness(health.MonitorFunc(func(ctx context.Context) (bool, error) {
						return ping(ctx) == nil, nil
					})),
				)
				probes <- hs.Probes() // where an HTTP or gRPC Service would expose them
				return []humus.Service{hs}, nil
			},
		}

		p := bedrocktest.NewProcess(t)
		status := make(chan bedrock.ExitStatus, 1)
		go func() { status <- app.Run(t.Context(), p.Process()) }()

		<-p.Notified(syscall.SIGTERM)
		synctest.Wait() // Serve has started
		pr := <-probes
		require.True(t, ready(t, pr.Liveness))
		require.False(t, ready(t, pr.Readiness), "ready while the database is unreachable")

		reachable = true
		require.True(t, ready(t, pr.Readiness))

		working.MarkUnhealthy()
		require.False(t, ready(t, pr.Liveness))

		p.Signal(syscall.SIGTERM)
		synctest.Wait()
		require.False(t, ready(t, pr.Readiness))

		require.Zero(t, (<-status).Code())
	})
}

func TestService_not_returned_from_Init_never_turns_ready(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		forgotten := health.NewService()
		served := health.NewService()
		app := humus.App{
			Host:                humustest.NewHost(t).Host(),
			Telemetry:           telemetry.NoopBuilder,
			ShutdownGracePeriod: 10 * time.Second,
			Init: func(context.Context, *humus.Setup) ([]humus.Service, error) {
				return []humus.Service{served}, nil // forgotten is not returned
			},
		}

		p := bedrocktest.NewProcess(t)
		status := make(chan bedrock.ExitStatus, 1)
		go func() { status <- app.Run(t.Context(), p.Process()) }()

		<-p.Notified(syscall.SIGTERM)
		synctest.Wait() // the App is serving
		require.True(t, ready(t, served.Probes().Readiness))
		require.False(t, ready(t, forgotten.Probes().Readiness))

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-status).Code())
		require.False(t, ready(t, forgotten.Probes().Readiness))
	})
}
