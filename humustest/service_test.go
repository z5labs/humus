// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
)

// The leaf subtests of TestService, as the test binary names them.
var serviceSubtests = []string{
	"Serve_returns_nil_after_Shutdown",
	"Shutdown_with_a_done_context_stops_the_Service_at_once",
	"Shutdown_is_safe/before_Serve",
	"Shutdown_is_safe/after_Serve_has_returned_on_its_own",
	"Shutdown_is_safe/twice",
	"No_goroutine_the_Service_started_outlives_Serve",
}

// newHTTPService builds a correct HTTP Service, which listens when it is
// built.
func newHTTPService(t *testing.T, s *humus.Setup) humus.Service {
	ln, err := s.Host.Listen("tcp", ":8080")
	require.NoError(t, err)
	return &httpService{ln: ln, srv: &http.Server{Handler: http.NotFoundHandler()}}
}

// fault is the one rule a job breaks.
type fault string

const (
	correct fault = ""

	// serveFails breaks "Serve returns nil after Shutdown": Serve returns
	// an error when Shutdown stops it.
	serveFails fault = "serve fails"

	// slowShutdown breaks "Shutdown with a done context stops the Service
	// at once": Shutdown ignores its context and drains for a second.
	slowShutdown fault = "slow shutdown"

	// panicsBeforeServe breaks "Shutdown is safe before Serve".
	panicsBeforeServe fault = "panics before serve"

	// failsAfterReturn breaks "Shutdown is safe after Serve has returned on
	// its own": Shutdown fails once the job has finished.
	failsAfterReturn fault = "fails after return"

	// panicsTwice breaks "Shutdown is safe twice": it closes its stop
	// channel without a sync.Once.
	panicsTwice fault = "panics twice"

	// leaks breaks "No goroutine the Service started outlives Serve": Serve
	// starts a goroutine and never stops it.
	leaks fault = "leaks"

	// hangs is not a fault of a job: TestTestService_child tests a
	// hangingService with it.
	hangs fault = "hangs"

	// failsOnStop is not a fault of a job either: TestTestService_child
	// tests a correct job whose OnStop function fails with it.
	failsOnStop fault = "fails on stop"

	// httpServer is no fault at all: TestTestService_child tests the
	// correct HTTP Service with it.
	httpServer fault = "http"
)

// faults maps every fault to the subtest it fails.
var faults = map[fault]string{
	serveFails:        "Serve_returns_nil_after_Shutdown",
	slowShutdown:      "Shutdown_with_a_done_context_stops_the_Service_at_once",
	panicsBeforeServe: "Shutdown_is_safe/before_Serve",
	failsAfterReturn:  "Shutdown_is_safe/after_Serve_has_returned_on_its_own",
	panicsTwice:       "Shutdown_is_safe/twice",
	leaks:             "No_goroutine_the_Service_started_outlives_Serve",
}

// job is a Service that does a unit of work every second until Shutdown
// stops it or Serve's context is done, as a queue consumer does. With no
// fault it is correct; with one, it breaks that rule alone.
type job struct {
	fault fault

	mu      sync.Mutex
	started bool
	onOwn   bool // Serve returned because its context was done

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func newJob(f fault) *job {
	return &job{
		fault: f,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

func (j *job) Serve(ctx context.Context) error {
	j.mu.Lock()
	j.started = true
	j.mu.Unlock()
	defer close(j.done)

	if j.fault == leaks {
		never := make(chan struct{})
		go func() { <-never }()
	}
	for {
		select {
		case <-ctx.Done():
			j.mu.Lock()
			j.onOwn = true
			j.mu.Unlock()
			return nil
		case <-j.stop:
			if j.fault == serveFails {
				return errors.New("job: stopped")
			}
			return nil
		case <-time.After(time.Second):
		}
	}
}

func (j *job) Shutdown(ctx context.Context) error {
	j.mu.Lock()
	started, onOwn := j.started, j.onOwn
	j.mu.Unlock()
	if j.fault == panicsBeforeServe && !started {
		panic("job: Shutdown before Serve")
	}
	finished := isDone(j.done)
	if j.fault == panicsTwice {
		close(j.stop)
	} else {
		j.stopOnce.Do(func() { close(j.stop) })
	}

	switch {
	case finished && onOwn && j.fault == failsAfterReturn:
		return errors.New("job: already finished")
	case finished, !started:
		return nil
	case j.fault == slowShutdown:
		time.Sleep(time.Second)
		<-j.done
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("job: %w", ctx.Err())
	}
	select {
	case <-j.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("job: %w", ctx.Err())
	}
}

// stopped reports whether the job is not serving: it never started, or its
// Serve has returned.
func (j *job) stopped() bool {
	j.mu.Lock()
	started := j.started
	j.mu.Unlock()
	return !started || isDone(j.done)
}

func isDone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestTestService_a_correct_HTTP_Service_passes(t *testing.T) {
	humustest.TestService(t, newHTTPService)
}

// TestTicker runs TestService on the ticker of ExampleTestService.
func TestTicker(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		return newTicker(time.Second)
	})
}

func TestTestService_a_correct_job_passes(t *testing.T) {
	humustest.TestService(t, func(*testing.T, *humus.Setup) humus.Service {
		return newJob(correct)
	})
}

func TestTestService_runs_OnStop_functions_through_the_lifecycle(t *testing.T) {
	var (
		ran    atomic.Int64
		mu     sync.Mutex
		setups = make(map[*humus.Setup]bool)
	)
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		require.Equal(t, bedrocktest.ProgramName, s.Process.Args[0], "the Setup's Process comes from bedrocktest")

		// The marker can only be created in a directory no other subtest
		// has used, so each subtest has a new humustest Host.
		f, err := s.Host.FS.OpenFile("marker", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		require.NoError(t, err, "each subtest has a new Host")
		require.NoError(t, f.Close())

		mu.Lock()
		require.False(t, setups[s], "each subtest has a new Setup")
		setups[s] = true
		mu.Unlock()

		j := newJob(correct)
		s.OnStop(func(context.Context) error {
			if !j.stopped() {
				return errors.New("OnStop ran while the job was serving")
			}
			ran.Add(1)
			return nil
		})
		return j
	})
	require.Equal(t, int64(len(serviceSubtests)), ran.Load(), "an OnStop function runs in every subtest")
	require.Len(t, setups, len(serviceSubtests))
}

func TestTestService_skips_what_it_cant_check(t *testing.T) {
	results, out := runChild(t, httpServer, true)
	for _, sub := range serviceSubtests {
		want := "PASS"
		if sub == "Shutdown_is_safe/after_Serve_has_returned_on_its_own" {
			want = "SKIP"
		}
		require.Equal(t, want, results[childTest+"/"+sub], "%s\n%s", sub, out)
	}
	require.Contains(t, out, `humustest: "Shutdown is safe after Serve has returned on its own" can't be checked: Serve keeps running after its context is done`)
}

func TestTestService_reports_an_App_that_fails_OnStop(t *testing.T) {
	results, out := runChild(t, failsOnStop, false)
	for _, sub := range serviceSubtests {
		require.Equal(t, "FAIL", results[childTest+"/"+sub], sub)
	}
	require.Contains(t, out, "humustest: the App exited 1 after")
	require.Contains(t, out, "on stop: closing the pool")
}

// hangingService is a Service whose Shutdown never returns, whatever its
// context.
type hangingService struct {
	stopped chan struct{}
}

func (s *hangingService) Serve(context.Context) error {
	<-s.stopped
	return nil
}

func (s *hangingService) Shutdown(context.Context) error {
	<-s.stopped
	return nil
}

func TestTestService_reports_a_Service_that_hangs(t *testing.T) {
	results, out := runChild(t, hangs, false)
	for _, sub := range serviceSubtests {
		require.Equal(t, "FAIL", results[childTest+"/"+sub], sub)
	}
	require.Contains(t, out, "every goroutine was blocked, for good, while calling Shutdown")
}

func TestTestService_fails_only_the_rule_a_Service_breaks(t *testing.T) {
	for f, broken := range faults {
		t.Run(string(f), func(t *testing.T) {
			results, out := runChild(t, f, false)

			for _, sub := range serviceSubtests {
				want := "PASS"
				if sub == broken {
					want = "FAIL"
				}
				require.Equal(t, want, results[childTest+"/"+sub], "%s\n%s", sub, out)
			}
			rule := strings.ReplaceAll(strings.ReplaceAll(broken, "_", " "), "/", " ")
			require.Contains(t, out, fmt.Sprintf("humustest: the Service broke %q: ", rule))
		})
	}
}

// childTest is the test that runChild runs in a child test binary.
const childTest = "TestTestService_child"

// childFault is the fault of the job TestTestService_child tests. It is
// set only in a child test binary.
var childFault = flag.String("humustest.fault", "", "the fault of the job that "+childTest+" tests")

// TestTestService_child runs TestService against a job with the fault
// named by -humustest.fault. runChild runs it in a child test binary, as a
// failure inside TestService fails the test that calls it.
func TestTestService_child(t *testing.T) {
	f := fault(*childFault)
	switch f {
	case correct:
		t.Skip("run in a child test binary by runChild")
	case httpServer:
		humustest.TestService(t, newHTTPService)
	case hangs:
		humustest.TestService(t, func(*testing.T, *humus.Setup) humus.Service {
			return &hangingService{stopped: make(chan struct{})}
		})
	case failsOnStop:
		humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
			s.OnStop(func(context.Context) error { return errors.New("on stop: closing the pool") })
			return newJob(correct)
		})
	default:
		if _, ok := faults[f]; !ok {
			t.Fatalf("unknown fault %q", f)
		}
		humustest.TestService(t, func(*testing.T, *humus.Setup) humus.Service {
			return newJob(f)
		})
	}
}

// resultLine is a line of -test.v output that reports a test's result.
var resultLine = regexp.MustCompile(`(?m)^\s*--- (PASS|FAIL|SKIP): (\S+) \(`)

// runChild runs TestTestService_child in a child test binary with fault f,
// requires it to pass or to fail, and returns the result of every test it
// ran, by name, and its output.
func runChild(t *testing.T, f fault, pass bool) (map[string]string, string) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), exe,
		"-test.run=^"+childTest+"$",
		"-test.v",
		"-test.count=1",
		"-test.timeout=2m",
		"-humustest.fault="+string(f),
	)
	b, err := cmd.CombinedOutput()
	out := string(b)
	want := "PASS"
	if pass {
		require.NoError(t, err, out)
	} else {
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "the child test binary should fail\n%s", out)
		want = "FAIL"
	}

	results := make(map[string]string)
	for _, m := range resultLine.FindAllStringSubmatch(out, -1) {
		results[m[2]] = m[1]
	}
	require.Equal(t, want, results[childTest], out)
	require.NotRegexp(t, `(?m)^panic: `, out, "the child test binary should not crash")
	return results, out
}
