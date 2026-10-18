// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/humustest"
)

// The tests in this file run the end user's App as client_test.go's do, with
// a backend whose answer depends on how many calls it has seen.

// answering returns an answer that answers the backend's nth call, from 1,
// with the error and the pushback of the nth of answers, and the last of
// them for every call after. A pushback of "" sends none.
func answering(b *backend, answers ...answer) func(context.Context) error {
	return func(ctx context.Context) error {
		n := int(b.calls.Load())
		a := answers[min(n, len(answers))-1]
		if a.pushback != "" {
			if err := grpc.SetTrailer(ctx, metadata.Pairs("grpc-retry-pushback-ms", a.pushback)); err != nil {
				return err
			}
		}
		if a.code == codes.OK {
			return nil
		}
		return status.Error(a.code, a.code.String())
	}
}

// answer is one of the backend's answers: a code, and a pushback.
type answer struct {
	code     codes.Code
	pushback string
}

// timed returns work that makes one EmptyCall and records how long it took
// in took.
func timed(took *time.Duration) func(ctx context.Context, c testpb.TestServiceClient) error {
	return func(ctx context.Context, c testpb.TestServiceClient) error {
		start := time.Now()
		err := emptyCall(ctx, c)
		*took = time.Since(start)
		return err
	}
}

// withBackend returns the App of the tests in this file: it serves b at
// backend:9090 with a certificate from a new CA, and builds a Client of it
// with DefaultSettings, changed by change, if it is not nil.
func withBackend(t *testing.T, b *backend, change func(*grpcclient.Settings), work func(ctx context.Context, c testpb.TestServiceClient) error) app {
	t.Helper()
	ca := humustest.NewCA(t)
	settings := settingsFor(ca, "backend:9090")
	if change != nil {
		change(&settings)
	}
	return app{
		host:      humustest.NewHost(t).Host(),
		backend:   b,
		serverTLS: serving(t, ca, "backend"),
		settings:  settings,
		work:      work,
	}
}

func TestClient_retries_Unavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		b := &backend{}
		b.answer = answering(b, answer{code: codes.Unavailable}, answer{code: codes.Unavailable}, answer{code: codes.OK})

		_, err := traced(t, tel, b, emptyCall).run(t)
		require.NoError(t, err)

		require.EqualValues(t, 3, b.calls.Load())
		spans := otelgrpcSpans(tel, trace.SpanKindClient)
		require.Len(t, spans, 3, "one client span for each attempt")
		var codes []string
		for _, span := range spans {
			codes = append(codes, attrs(span)["rpc.response.status_code"].AsString())
		}
		require.ElementsMatch(t, []string{"UNAVAILABLE", "UNAVAILABLE", "OK"}, codes)
	})
}

func TestClient_pushback(t *testing.T) {
	t.Run("is the least delay", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{}
			b.answer = answering(b, answer{code: codes.ResourceExhausted, pushback: "2000"}, answer{code: codes.OK})

			var took time.Duration
			_, err := withBackend(t, b, nil, timed(&took)).run(t)
			require.NoError(t, err)

			require.EqualValues(t, 2, b.calls.Load())
			require.GreaterOrEqual(t, took, 2*time.Second)
		})
	})

	t.Run("a negative one stops the retries", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{}
			b.answer = answering(b, answer{code: codes.Unavailable, pushback: "-1"})

			_, err := withBackend(t, b, nil, emptyCall).run(t)
			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)
			require.EqualValues(t, 1, b.calls.Load())
		})
	})

	t.Run("ResourceExhausted without one is tried once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{}
			b.answer = answering(b, answer{code: codes.ResourceExhausted})

			_, err := withBackend(t, b, nil, emptyCall).run(t)
			require.Equal(t, codes.ResourceExhausted, status.Code(err), "err = %v", err)
			require.EqualValues(t, 1, b.calls.Load())
		})
	})

	t.Run("a long one isn't waited for", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{}
			b.answer = answering(b, answer{code: codes.Unavailable, pushback: "60000"})

			var took time.Duration
			_, err := withBackend(t, b, nil, timed(&took)).run(t)
			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)
			require.EqualValues(t, 1, b.calls.Load())
			require.Zero(t, took, "the call returns at once")
		})
	})
}

func TestClient_Timeout_ends_the_retries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &backend{}
		b.answer = answering(b, answer{code: codes.Unavailable, pushback: "4000"})

		var took time.Duration
		_, err := withBackend(t, b, func(s *grpcclient.Settings) { s.Timeout = 6 * time.Second }, timed(&took)).run(t)

		require.Equal(t, codes.Unavailable, status.Code(err), "the second attempt's error, err = %v", err)
		require.EqualValues(t, 2, b.calls.Load(), "a third attempt would start after the call's deadline")
		require.Equal(t, 4*time.Second, took, "the call returns once the second attempt fails")
	})
}

func TestClient_retry_budget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &backend{answer: unavailable}

		var attempts []int32
		_, err := withBackend(t, b, nil, func(ctx context.Context, c testpb.TestServiceClient) error {
			for range 6 {
				before := b.calls.Load()
				if err := emptyCall(ctx, c); status.Code(err) != codes.Unavailable {
					return err
				}
				attempts = append(attempts, b.calls.Load()-before)
			}
			return nil
		}).run(t)
		require.NoError(t, err)

		// The budget holds 10 tokens, and each attempt classified Retry
		// takes one. A retry follows only while more than 5 are left: the
		// first call's three attempts leave 7, the second's second attempt
		// leaves 5, and from then on every call makes one attempt.
		require.Equal(t, []int32{3, 2, 1, 1, 1, 1}, attempts)
	})
}

func TestClient_streams(t *testing.T) {
	t.Run("opening a stream to an address nothing listens on makes MaxAttempts attempts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var opens atomic.Int32
			counting := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
				opens.Add(1)
				return streamer(ctx, desc, cc, method, opts...)
			}
			settings := grpcclient.DefaultSettings()
			settings.Addr = "nowhere:9090"
			classify := settings.Classify
			var (
				mu       sync.Mutex
				trailers []metadata.MD
			)
			settings.Classify = func(method string, err error, trailer metadata.MD) grpcclient.Classification {
				mu.Lock()
				trailers = append(trailers, trailer)
				mu.Unlock()
				return classify(method, err, trailer)
			}

			_, err := app{
				host:     humustest.NewHost(t).Host(),
				settings: settings,
				opts:     []grpc.DialOption{grpc.WithChainStreamInterceptor(counting)},
				work: func(ctx context.Context, c testpb.TestServiceClient) error {
					_, err := c.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{})
					return err
				},
			}.run(t)

			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)
			require.EqualValues(t, 3, opens.Load())
			require.Equal(t, []metadata.MD{nil, nil, nil}, trailers, "Classify gets each failed open with no trailer")
		})
	})

	t.Run("a stream whose server fails after a message isn't retried", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{streamErr: status.Error(codes.Unavailable, "going away")}

			var got int
			_, err := withBackend(t, b, nil, streamOne(&got)).run(t)

			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)
			require.ErrorContains(t, err, "going away")
			require.Equal(t, 1, got, "the message before the error")
			require.EqualValues(t, 1, b.calls.Load())
		})
	})
}

// streamOne returns work that opens a server-streaming RPC that asks for one
// message, reads it to its end, and counts its messages in got.
func streamOne(got *int) func(ctx context.Context, c testpb.TestServiceClient) error {
	return func(ctx context.Context, c testpb.TestServiceClient) error {
		stream, err := c.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{
			ResponseParameters: []*testpb.ResponseParameters{{}},
		})
		if err != nil {
			return err
		}
		for {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			*got++
		}
	}
}

func TestNewClient_runs_a_chained_interceptor_in_opts_once_per_attempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &backend{}
		b.answer = answering(b, answer{code: codes.Unavailable}, answer{code: codes.Unavailable}, answer{code: codes.OK})

		var runs atomic.Int32
		counting := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			runs.Add(1)
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		a := withBackend(t, b, nil, emptyCall)
		a.opts = []grpc.DialOption{grpc.WithChainUnaryInterceptor(counting)}
		_, err := a.run(t)
		require.NoError(t, err)

		require.EqualValues(t, 3, runs.Load())
		require.EqualValues(t, 3, b.calls.Load())
	})
}

func TestClient_with_the_end_users_Classify(t *testing.T) {
	t.Run("one that retries Aborted and hands the rest to the default", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{}
			b.answer = answering(b, answer{code: codes.Aborted}, answer{code: codes.OK})

			_, err := withBackend(t, b, func(s *grpcclient.Settings) {
				classify := s.Classify
				s.Classify = func(method string, err error, trailer metadata.MD) grpcclient.Classification {
					if status.Code(err) == codes.Aborted {
						return grpcclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
					}
					return classify(method, err, trailer)
				}
			}, emptyCall).run(t)
			require.NoError(t, err)
			require.EqualValues(t, 2, b.calls.Load())
		})
	})

	t.Run("one that opts a method out of retries", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{answer: unavailable}

			var emptyCalls, unaryCalls int32
			_, err := withBackend(t, b, func(s *grpcclient.Settings) {
				classify := s.Classify
				s.Classify = func(method string, err error, trailer metadata.MD) grpcclient.Classification {
					if method == "/grpc.testing.TestService/EmptyCall" {
						return grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}
					}
					return classify(method, err, trailer)
				}
			}, func(ctx context.Context, c testpb.TestServiceClient) error {
				err := emptyCall(ctx, c)
				emptyCalls = b.calls.Load()
				if status.Code(err) != codes.Unavailable {
					return err
				}
				_, err = c.UnaryCall(ctx, &testpb.SimpleRequest{})
				unaryCalls = b.calls.Load() - emptyCalls
				return err
			}).run(t)
			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)

			require.EqualValues(t, 1, emptyCalls, "EmptyCall, opted out, is tried once")
			require.EqualValues(t, 3, unaryCalls, "UnaryCall is retried")
		})
	})

	t.Run("a RetryAfter beyond BackoffMax ends the call after one attempt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{answer: unavailable}

			var took time.Duration
			_, err := withBackend(t, b, func(s *grpcclient.Settings) {
				backoffMax := s.BackoffMax
				s.Classify = func(string, error, metadata.MD) grpcclient.Classification {
					return grpcclient.Classification{Retry: retry.Retry, RetryAfter: backoffMax + time.Millisecond, Breaker: circuit.Failure}
				}
			}, timed(&took)).run(t)
			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)

			require.EqualValues(t, 1, b.calls.Load())
			require.Zero(t, took, "the call returns at once")
		})
	})
}

func TestClient_decides_what_is_about_the_call(t *testing.T) {
	// retryAll is a Classify that asks for another attempt whatever the
	// result, and counts the results it is asked about.
	retryAll := func(asked *atomic.Int32) func(*grpcclient.Settings) {
		return func(s *grpcclient.Settings) {
			s.Classify = func(string, error, metadata.MD) grpcclient.Classification {
				asked.Add(1)
				return grpcclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}
			}
		}
	}

	t.Run("a call its caller cancels makes no further attempt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{answer: blocked}

			var asked atomic.Int32
			_, err := withBackend(t, b, retryAll(&asked), func(ctx context.Context, c testpb.TestServiceClient) error {
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				time.AfterFunc(time.Second, cancel)
				return emptyCall(ctx, c)
			}).run(t)

			require.Equal(t, codes.Canceled, status.Code(err), "err = %v", err)
			require.EqualValues(t, 1, b.calls.Load())
			require.Zero(t, asked.Load(), "Classify isn't asked about an error once the call's context is done")
		})
	})

	t.Run("a call made with a context already done makes one attempt, and fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{}

			var (
				asked     atomic.Int32
				streamErr error
			)
			_, err := withBackend(t, b, retryAll(&asked), func(ctx context.Context, c testpb.TestServiceClient) error {
				ctx, cancel := context.WithCancel(ctx)
				cancel()
				_, streamErr = c.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{})
				return emptyCall(ctx, c)
			}).run(t)

			require.Equal(t, codes.Canceled, status.Code(err), "err = %v", err)
			require.Equal(t, codes.Canceled, status.Code(streamErr), "err = %v", streamErr)
			require.Zero(t, b.calls.Load())
			require.Zero(t, asked.Load())
		})
	})

	t.Run("a call whose deadline passes makes no further attempt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{answer: blocked}

			var asked atomic.Int32
			_, err := withBackend(t, b, retryAll(&asked), emptyCall).run(t)

			require.Equal(t, codes.DeadlineExceeded, status.Code(err), "err = %v", err)
			require.EqualValues(t, 1, b.calls.Load())
			require.Zero(t, asked.Load(), "Classify isn't asked about an error once the call's context is done")
		})
	})

	t.Run("an open stream that fails isn't retried", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &backend{streamErr: status.Error(codes.Unavailable, "going away")}

			var (
				asked atomic.Int32
				got   int
			)
			_, err := withBackend(t, b, retryAll(&asked), streamOne(&got)).run(t)

			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)
			require.Equal(t, 1, got)
			require.EqualValues(t, 1, b.calls.Load())
			require.Zero(t, asked.Load(), "Classify isn't asked about a stream that opened")
		})
	})
}

func TestClient_panics(t *testing.T) {
	for _, tc := range []struct {
		name     string
		classify func(string, error, metadata.MD) grpcclient.Classification
		want     any
	}{
		{
			name:     "a panic in Classify reaches the caller",
			classify: func(string, error, metadata.MD) grpcclient.Classification { panic("the end user's bug") },
			want:     "the end user's bug",
		},
		{
			name: "a Classification with a zero Retry",
			classify: func(string, error, metadata.MD) grpcclient.Classification {
				return grpcclient.Classification{Breaker: circuit.Success}
			},
			want: "grpcclient: Settings.Classify returned a Classification whose Retry is Outcome(0), not a retry.Outcome",
		},
		{
			name: "a Classification with a zero Breaker",
			classify: func(string, error, metadata.MD) grpcclient.Classification {
				return grpcclient.Classification{Retry: retry.Success}
			},
			want: "grpcclient: Settings.Classify returned a Classification whose Breaker is Outcome(0), not a circuit.Outcome",
		},
		{
			name: "retry.Retry with a negative RetryAfter",
			classify: func(string, error, metadata.MD) grpcclient.Classification {
				return grpcclient.Classification{Retry: retry.Retry, RetryAfter: -time.Second, Breaker: circuit.Failure}
			},
			want: "grpcclient: Settings.Classify returned retry.Retry with a negative RetryAfter, -1s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := &backend{}
				var recovered any
				_, err := withBackend(t, b, func(s *grpcclient.Settings) { s.Classify = tc.classify }, func(ctx context.Context, c testpb.TestServiceClient) (err error) {
					defer func() { recovered = recover() }()
					return emptyCall(ctx, c)
				}).run(t)
				require.NoError(t, err)

				require.Equal(t, tc.want, recovered)
				require.EqualValues(t, 1, b.calls.Load())
			})
		})
	}
}
