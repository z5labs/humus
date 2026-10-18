// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/humustest"
)

// The tests in this file run the end user's App as client_test.go's do,
// and drive the Client's circuit breaker through its states in fake time.

// clientScope is the instrumentation scope of the Client's own telemetry.
const clientScope = "github.com/z5labs/humus/grpc/grpcclient"

// The bodies of the breaker's log records.
const (
	breakerOpen     = "circuit breaker open"
	breakerHalfOpen = "circuit breaker half-open"
	breakerClosed   = "circuit breaker closed"
)

// StreamingInputCall reads the client's messages to their end, and answers
// with one response.
func (b *backend) StreamingInputCall(stream grpc.ClientStreamingServer[testpb.StreamingInputCallRequest, testpb.StreamingInputCallResponse]) error {
	b.record(stream.Context())
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return stream.SendAndClose(&testpb.StreamingInputCallResponse{})
			}
			return err
		}
	}
}

// reasonKey is the trailer FullDuplexCall sends.
const reasonKey = "x-reason"

// FullDuplexCall sends the trailer x-reason: draining, and ends with
// streamErr.
func (b *backend) FullDuplexCall(stream grpc.BidiStreamingServer[testpb.StreamingOutputCallRequest, testpb.StreamingOutputCallResponse]) error {
	b.record(stream.Context())
	stream.SetTrailer(metadata.Pairs(reasonKey, "draining"))
	return b.streamErr
}

// HalfDuplexCall sends one message, and ends only once its context is
// done, as when the client cancels it.
func (b *backend) HalfDuplexCall(stream grpc.BidiStreamingServer[testpb.StreamingOutputCallRequest, testpb.StreamingOutputCallResponse]) error {
	b.record(stream.Context())
	if err := stream.Send(&testpb.StreamingOutputCallResponse{}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

// failingUntil returns an answer that is Unavailable while failing is set,
// and OK once it is not.
func failingUntil(failing *atomic.Bool) func(context.Context) error {
	return func(context.Context) error {
		if failing.Load() {
			return status.Error(codes.Unavailable, "not now")
		}
		return nil
	}
}

// breakerRecords returns the Client's log records in tel, in the order
// they were emitted.
func breakerRecords(tel *humustest.Telemetry) []sdklog.Record {
	var out []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == clientScope {
			out = append(out, r)
		}
	}
	return out
}

// bodies returns the body of each of rs.
func bodies(rs []sdklog.Record) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Body().AsString())
	}
	return out
}

// recordAttrs returns r's attributes by key, as strings and ints.
func recordAttrs(r sdklog.Record) map[string]any {
	m := make(map[string]any)
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		m[string(kv.Key)] = kv.Value.AsInterface()
		return true
	})
	return m
}

// clientSpans returns every span of kind client in tel, whatever its scope.
func clientSpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, s := range tel.Spans() {
		if s.SpanKind() == trace.SpanKindClient {
			spans = append(spans, s)
		}
	}
	return spans
}

// countingTrips returns a change of Settings whose BreakerPolicy never
// trips, and records the counts its Trip is asked about, one for each
// success or failure the breaker counts.
func countingTrips(mu *sync.Mutex, counted *[]circuit.Counts) func(*grpcclient.Settings) {
	return func(s *grpcclient.Settings) {
		s.BreakerPolicy.Trip = func(c circuit.Counts) bool {
			mu.Lock()
			defer mu.Unlock()
			*counted = append(*counted, c)
			return false
		}
	}
}

func TestClient_breaker_opens_rejects_and_closes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		var failing atomic.Bool
		failing.Store(true)
		b := &backend{answer: failingUntil(&failing)}

		var (
			failures  []error
			rejected  error
			reached   int32
			successes []error
		)
		a := traced(t, tel, b, func(ctx context.Context, c testpb.TestServiceClient) error {
			for range 5 {
				failures = append(failures, emptyCall(ctx, c))
			}
			rejected = emptyCall(ctx, c)
			reached = b.calls.Load()

			time.Sleep(10 * time.Second)
			failing.Store(false)
			for range 3 {
				successes = append(successes, emptyCall(ctx, c))
			}
			return nil
		})
		a.settings.MaxAttempts = 1
		_, err := a.run(t)
		require.NoError(t, err)

		for i, err := range failures {
			require.Equal(t, codes.Unavailable, status.Code(err), "call %d, err = %v", i+1, err)
			require.NotErrorIs(t, err, circuit.ErrRejected)
		}
		require.ErrorIs(t, rejected, circuit.ErrRejected)
		require.Equal(t, codes.Unavailable, status.Code(rejected))
		require.Equal(t, "circuit: call rejected: open for another 10s", status.Convert(rejected).Message())
		var re *circuit.RejectedError
		require.ErrorAs(t, rejected, &re)
		require.Equal(t, circuit.Open, re.State)
		require.EqualValues(t, 5, reached, "the sixth call never reached the backend")
		for i, err := range successes {
			require.NoError(t, err, "success %d", i+1)
		}
		require.EqualValues(t, 8, b.calls.Load())

		records := breakerRecords(tel)
		require.Equal(t, []string{breakerOpen, breakerHalfOpen, breakerClosed}, bodies(records))
		require.Equal(t, log.SeverityWarn, records[0].Severity())
		require.Equal(t, log.SeverityInfo, records[1].Severity())
		require.Equal(t, log.SeverityInfo, records[2].Severity())

		// The open record
		{
			a := recordAttrs(records[0])
			require.Equal(t, "closed", a["humus.circuit.previous_state"])
			require.Equal(t, "backend", a["server.address"])
			require.EqualValues(t, 9090, a["server.port"])
			require.EqualValues(t, 5, a["humus.circuit.consecutive_failures"])
			require.EqualValues(t, 5, a["humus.circuit.failures"])
			require.EqualValues(t, 0, a["humus.circuit.successes"])
			require.EqualValues(t, 0, a["humus.circuit.consecutive_successes"])
			require.EqualValues(t, 0, a["humus.circuit.timed_out"])
		}

		// The closed record
		{
			a := recordAttrs(records[2])
			require.Equal(t, "half-open", a["humus.circuit.previous_state"])
			require.EqualValues(t, 3, a["humus.circuit.consecutive_successes"])
		}

		// The trace
		{
			spans := clientSpans(tel)
			require.Len(t, spans, 9, "five failures, the rejection's own span and three successes")
			require.Len(t, otelgrpcSpans(tel, trace.SpanKindClient), 8)

			var own []sdktrace.ReadOnlySpan
			for _, s := range spans {
				if s.InstrumentationScope().Name == clientScope {
					own = append(own, s)
				}
			}
			require.Len(t, own, 1)
			span := own[0]
			require.Equal(t, emptyCallSpan, span.Name())
			require.Equal(t, otelcodes.Error, span.Status().Code)
			require.Equal(t, "circuit: call rejected: open for another 10s", span.Status().Description)
			require.Equal(t, span.StartTime(), span.EndTime(), "it ends at once")
			a := attrs(span)
			require.Equal(t, "grpc", a["rpc.system.name"].AsString())
			require.Equal(t, emptyCallSpan, a["rpc.method"].AsString())
			require.Equal(t, "UNAVAILABLE", a["rpc.response.status_code"].AsString())
			require.Equal(t, "backend", a["server.address"].AsString())
			require.EqualValues(t, 9090, a["server.port"].AsInt64())

			job := jobSpan(t, tel).SpanContext()
			require.Equal(t, job.SpanID(), span.Parent().SpanID(), "the rejection's span is a child of the job's span")
		}
	})
}

func TestClient_breaker_rejection_is_not_retried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &backend{answer: unavailable}

		var (
			first, second, opening error
			afterFirst             int32
			took                   time.Duration
		)
		_, err := withBackend(t, b, func(s *grpcclient.Settings) {
			s.MaxAttempts = 3
			s.BreakerPolicy.Trip = func(c circuit.Counts) bool { return c.ConsecutiveFailures >= 1 }
		}, func(ctx context.Context, c testpb.TestServiceClient) error {
			first = emptyCall(ctx, c)
			afterFirst = b.calls.Load()

			start := time.Now()
			second = emptyCall(ctx, c)
			took = time.Since(start)

			_, opening = c.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{})
			return nil
		}).run(t)
		require.NoError(t, err)

		require.ErrorIs(t, first, circuit.ErrRejected, "the first call's second attempt was rejected, and ended it")
		require.EqualValues(t, 1, afterFirst)

		require.ErrorIs(t, second, circuit.ErrRejected)
		require.Equal(t, codes.Unavailable, status.Code(second))
		require.Zero(t, took, "the call returns at once")

		require.ErrorIs(t, opening, circuit.ErrRejected, "a stream's open goes through the breaker")
		require.Equal(t, codes.Unavailable, status.Code(opening))
		require.EqualValues(t, 1, b.calls.Load(), "no rejected call reached the backend")
	})
}

func TestClient_breaker_does_not_count_the_caller_giving_up(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		b := &backend{answer: blocked}

		var (
			asked atomic.Int32
			errs  []error
		)
		a := traced(t, tel, b, func(ctx context.Context, c testpb.TestServiceClient) error {
			for range 6 {
				ctx, cancel := context.WithCancel(ctx)
				time.AfterFunc(time.Second, cancel)
				errs = append(errs, emptyCall(ctx, c))
				cancel()
			}
			// A seventh call still reaches the backend: the breaker is
			// closed.
			ctx, cancel := context.WithCancel(ctx)
			time.AfterFunc(time.Second, cancel)
			defer cancel()
			errs = append(errs, emptyCall(ctx, c))
			return nil
		})
		a.settings.MaxAttempts = 1
		a.settings.Classify = func(string, error, metadata.MD) grpcclient.Classification {
			asked.Add(1)
			return grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}
		}
		_, err := a.run(t)
		require.NoError(t, err)

		for i, err := range errs {
			require.Equal(t, codes.Canceled, status.Code(err), "call %d, err = %v", i+1, err)
		}
		require.EqualValues(t, 7, b.calls.Load())
		require.Zero(t, asked.Load(), "Classify isn't asked about a call its caller cancelled")
		require.Empty(t, breakerRecords(tel), "the breaker never left closed")
	})
}

func TestClient_breaker_counts_deadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		b := &backend{answer: blocked}

		var (
			errs     []error
			took     []time.Duration
			rejected error
		)
		a := traced(t, tel, b, func(ctx context.Context, c testpb.TestServiceClient) error {
			for range 5 {
				start := time.Now()
				errs = append(errs, emptyCall(ctx, c))
				took = append(took, time.Since(start))
			}
			rejected = emptyCall(ctx, c)
			return nil
		})
		a.settings.MaxAttempts = 1
		_, err := a.run(t)
		require.NoError(t, err)

		for i := range errs {
			require.Equal(t, codes.DeadlineExceeded, status.Code(errs[i]), "call %d, err = %v", i+1, errs[i])
			require.Equal(t, 10*time.Second, took[i], "call %d ends at Timeout", i+1)
		}
		require.ErrorIs(t, rejected, circuit.ErrRejected)
		require.EqualValues(t, 5, b.calls.Load())

		records := breakerRecords(tel)
		require.Equal(t, []string{breakerOpen}, bodies(records))
		require.EqualValues(t, 5, recordAttrs(records[0])["humus.circuit.consecutive_failures"])
	})
}

func TestClient_breaker_counts_a_streams_end(t *testing.T) {
	t.Run("a server-streaming RPC that ends with Unavailable after a message is one failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var (
				mu      sync.Mutex
				counted []circuit.Counts
				got     int
			)
			b := &backend{streamErr: status.Error(codes.Unavailable, "going away")}
			_, err := withBackend(t, b, countingTrips(&mu, &counted), streamOne(&got)).run(t)
			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)
			require.Equal(t, 1, got)

			require.Equal(t, []circuit.Counts{{Failures: 1, ConsecutiveFailures: 1}}, counted)
		})
	})

	t.Run("a server-streaming RPC read to io.EOF is one success", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var (
				mu      sync.Mutex
				counted []circuit.Counts
				got     int
			)
			_, err := withBackend(t, &backend{}, countingTrips(&mu, &counted), streamOne(&got)).run(t)
			require.ErrorIs(t, err, io.EOF)
			require.Equal(t, 1, got)

			require.Equal(t, []circuit.Counts{{Successes: 1, ConsecutiveSuccesses: 1}}, counted)
		})
	})

	t.Run("a client-streaming RPC's response settles its ticket", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var (
				mu      sync.Mutex
				counted []circuit.Counts
			)
			_, err := withBackend(t, &backend{}, countingTrips(&mu, &counted), func(ctx context.Context, c testpb.TestServiceClient) error {
				stream, err := c.StreamingInputCall(ctx)
				if err != nil {
					return err
				}
				if err := stream.Send(&testpb.StreamingInputCallRequest{}); err != nil {
					return err
				}
				_, err = stream.CloseAndRecv()
				return err
			}).run(t)
			require.NoError(t, err)

			require.Equal(t, []circuit.Counts{{Successes: 1, ConsecutiveSuccesses: 1}}, counted)
		})
	})

	t.Run("a stream whose caller cancels it is ignored", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var (
				mu      sync.Mutex
				counted []circuit.Counts
				asked   atomic.Int32
				got     int
			)
			_, err := withBackend(t, &backend{}, func(s *grpcclient.Settings) {
				countingTrips(&mu, &counted)(s)
				s.Classify = func(string, error, metadata.MD) grpcclient.Classification {
					asked.Add(1)
					return grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}
				}
			}, func(ctx context.Context, c testpb.TestServiceClient) error {
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				// The backend's HalfDuplexCall stays open after its one
				// message, until its caller cancels it.
				stream, err := c.HalfDuplexCall(ctx)
				if err != nil {
					return err
				}
				if _, err := stream.Recv(); err != nil {
					return err
				}
				got++
				cancel()
				_, err = stream.Recv()
				return err
			}).run(t)
			require.Equal(t, codes.Canceled, status.Code(err), "err = %v", err)
			require.Equal(t, 1, got)

			require.Empty(t, counted, "the breaker counted nothing")
			require.Zero(t, asked.Load(), "Classify isn't asked about a stream its caller cancelled")
		})
	})
}

func TestClient_breaker_Classify_sees_a_streams_trailer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type asked struct {
			method  string
			err     error
			trailer metadata.MD
		}
		var (
			mu  sync.Mutex
			all []asked
		)
		b := &backend{streamErr: status.Error(codes.Unavailable, "going away")}
		_, err := withBackend(t, b, func(s *grpcclient.Settings) {
			classify := s.Classify
			s.Classify = func(method string, err error, trailer metadata.MD) grpcclient.Classification {
				mu.Lock()
				all = append(all, asked{method: method, err: err, trailer: trailer})
				mu.Unlock()
				return classify(method, err, trailer)
			}
		}, func(ctx context.Context, c testpb.TestServiceClient) error {
			stream, err := c.FullDuplexCall(ctx)
			if err != nil {
				return err
			}
			if err := stream.CloseSend(); err != nil {
				return err
			}
			for {
				if _, err := stream.Recv(); err != nil {
					return err
				}
			}
		}).run(t)
		require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)

		require.Len(t, all, 1, "Classify is asked once, about the stream's end")
		require.Equal(t, "/grpc.testing.TestService/FullDuplexCall", all[0].method)
		require.Equal(t, codes.Unavailable, status.Code(all[0].err))
		require.Equal(t, "going away", status.Convert(all[0].err).Message())
		require.Equal(t, []string{"draining"}, all[0].trailer.Get(reasonKey))
	})
}

func TestClient_breaker_turned_off(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		b := &backend{answer: unavailable}

		var errs []error
		a := traced(t, tel, b, func(ctx context.Context, c testpb.TestServiceClient) error {
			for range 30 {
				errs = append(errs, emptyCall(ctx, c))
			}
			return nil
		})
		a.settings.MaxAttempts = 1
		neverTrip(&a.settings)
		_, err := a.run(t)
		require.NoError(t, err)

		for i, err := range errs {
			require.Equal(t, codes.Unavailable, status.Code(err), "call %d", i+1)
			require.NotErrorIs(t, err, circuit.ErrRejected, "call %d", i+1)
		}
		require.EqualValues(t, 30, b.calls.Load(), "every call reached the backend")
		require.Empty(t, breakerRecords(tel), "the breaker never opened")
	})
}

func TestClient_breaker_policy_panics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &backend{}

		var (
			panicked atomic.Bool
			recov    any
			after    error
		)
		_, err := withBackend(t, b, func(s *grpcclient.Settings) {
			trip := s.BreakerPolicy.Trip
			s.BreakerPolicy.Trip = func(c circuit.Counts) bool {
				if panicked.CompareAndSwap(false, true) {
					panic("the end user's bug")
				}
				return trip(c)
			}
		}, func(ctx context.Context, c testpb.TestServiceClient) error {
			func() {
				defer func() { recov = recover() }()
				_ = emptyCall(ctx, c)
			}()
			after = emptyCall(ctx, c)
			return nil
		}).run(t)
		require.NoError(t, err)

		require.Equal(t, "the end user's bug", recov, "the panic passes to the goroutine that made the call")
		require.NoError(t, after, "the breaker stays consistent, and admits the next call")
		require.EqualValues(t, 2, b.calls.Load())
	})
}
