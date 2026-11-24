// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/kafka"
	"github.com/z5labs/humus/telemetry"
)

// The Kafka error codes the tests expect, as the package reports them.
var (
	invalidRecord            = &kafka.Error{Code: 87, Name: "INVALID_RECORD"}
	messageTooLarge          = &kafka.Error{Code: 10, Name: "MESSAGE_TOO_LARGE"}
	notEnoughReplicas        = &kafka.Error{Code: 19, Name: "NOT_ENOUGH_REPLICAS", Retriable: true}
	topicAuthorizationFailed = &kafka.Error{Code: 29, Name: "TOPIC_AUTHORIZATION_FAILED"}
	groupAuthorizationFailed = &kafka.Error{Code: 30, Name: "GROUP_AUTHORIZATION_FAILED"}
	saslAuthenticationFailed = &kafka.Error{Code: 58, Name: "SASL_AUTHENTICATION_FAILED"}
	unknownTopicOrPartition  = &kafka.Error{Code: 3, Name: "UNKNOWN_TOPIC_OR_PARTITION", Retriable: true}
)

// requireCode requires that errors.AsType finds an *kafka.Error in err with
// want's number, name and retriability, and that errors.Is matches err with
// want.
func requireCode(t *testing.T, err error, want *kafka.Error, msgAndArgs ...any) {
	t.Helper()
	got, ok := errors.AsType[*kafka.Error](err)
	require.True(t, ok, "errors.As finds no *kafka.Error in %v", err)
	require.Equal(t, *want, *got, msgAndArgs...)
	require.ErrorIs(t, err, want, msgAndArgs...)
}

// requireNoFranzGo walks err's Unwrap, both kinds, and requires that no
// error in it has a type from a franz-go package or is one of the error
// values franz-go exports.
func requireNoFranzGo(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var walk func(err error, at string)
	walk = func(err error, at string) {
		typ := reflect.TypeOf(err)
		elem := typ
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if path := elem.PkgPath(); path == "github.com/twmb/franz-go" || strings.HasPrefix(path, "github.com/twmb/franz-go/") {
			t.Errorf("%s: %v is a %s, a type of franz-go's", at, err, typ)
		}
		for name, v := range kafka.FranzGoValues {
			if typ == reflect.TypeOf(v) && err == v {
				t.Errorf("%s: %v is %s", at, err, name)
			}
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			if w := u.Unwrap(); w != nil {
				walk(w, at+".Unwrap()")
			}
		case interface{ Unwrap() []error }:
			for i, w := range u.Unwrap() {
				if w != nil {
					walk(w, fmt.Sprintf("%s.Unwrap()[%d]", at, i))
				}
			}
		}
	}
	walk(err, "err")
}

// franzGoName matches a type's name as reflect or semconv.ErrorType writes
// it, when the type is one of franz-go's packages'.
var franzGoName = regexp.MustCompile(`franz-go|^\*?(kerr|kgo|kbin|kmsg|kversion|sasl|plain|scram|oauth|aws|kotel)\.`)

// requireErrorTypeNotFranzGo requires that errType, an error.type, names no
// type of franz-go's.
func requireErrorTypeNotFranzGo(t *testing.T, errType string) {
	t.Helper()
	require.NotEmpty(t, errType)
	require.NotRegexp(t, franzGoName, errType, "error.type names a franz-go type")
}

func TestError(t *testing.T) {
	t.Parallel()

	require.Equal(t, "kafka: TOPIC_AUTHORIZATION_FAILED (error code 29)", topicAuthorizationFailed.Error())
	require.Equal(t, "kafka: error code 29", (&kafka.Error{Code: 29}).Error())

	wrapped := fmt.Errorf("kafka: fetching orders, partition 0: %w", topicAuthorizationFailed)
	require.ErrorIs(t, wrapped, &kafka.Error{Code: 29}, "errors.Is compares codes alone")
	require.NotErrorIs(t, wrapped, &kafka.Error{Code: 30, Name: "TOPIC_AUTHORIZATION_FAILED"})
	require.NotErrorIs(t, wrapped, (*kafka.Error)(nil))
	require.NotErrorIs(t, wrapped, errors.New("kafka: TOPIC_AUTHORIZATION_FAILED (error code 29)"))
}

func TestFromClient(t *testing.T) {
	t.Parallel()

	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tlsErr := &tls.CertificateVerificationError{Err: errors.New("bad certificate")}
	testCases := []struct {
		name    string
		err     error
		code    *kafka.Error // found with errors.As, when not nil
		matches []error      // each found with errors.Is
	}{
		{name: "a Kafka error code", err: kerr.TopicAuthorizationFailed, code: topicAuthorizationFailed},
		{name: "a retriable Kafka error code", err: kerr.UnknownTopicOrPartition, code: unknownTopicOrPartition},
		{
			name: "a Kafka error code franz-go wraps",
			err:  fmt.Errorf("%w (uncompressed_bytes=2000000)", kerr.MessageTooLarge),
			code: messageTooLarge,
		},
		{
			name: "a refusal with the broker's message",
			err:  fmt.Errorf("%s: %w", "Authentication failed", kerr.SaslAuthenticationFailed),
			code: saslAuthenticationFailed,
		},
		{
			name:    "franz-go's timeout, with its last error",
			err:     fmt.Errorf("%w, last err: %w", kgo.ErrRecordTimeout, kerr.NotEnoughReplicas),
			code:    notEnoughReplicas,
			matches: []error{},
		},
		{name: "franz-go's closed client", err: kgo.ErrClientClosed},
		{name: "a connection franz-go failed", err: fmt.Errorf("unable to dial: %w", opErr), matches: []error{opErr}},
		{name: "a TLS failure", err: fmt.Errorf("unable to dial: %w", tlsErr), matches: []error{tlsErr}},
		{name: "a franz-go type wrapping io.EOF", err: &kgo.ErrFirstReadEOF{}, matches: []error{}},
		{name: "context", err: fmt.Errorf("%w: %w", kgo.ErrClientClosed, context.Canceled), matches: []error{context.Canceled}},
		{name: "joined", err: errors.Join(kerr.InvalidRecord, io.ErrUnexpectedEOF), code: invalidRecord, matches: []error{io.ErrUnexpectedEOF}},
	}
	for name, v := range kafka.FranzGoValues {
		testCases = append(testCases, struct {
			name    string
			err     error
			code    *kafka.Error
			matches []error
		}{name: name, err: v})
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := kafka.FromClient(tc.err)
			requireNoFranzGo(t, got)
			if _, code := tc.err.(*kerr.Error); code {
				require.Equal(t, tc.code.Error(), got.Error(), "a code is humus's *kafka.Error")
			} else {
				require.Equal(t, tc.err.Error(), got.Error(), "the client's message is kept")
			}
			if tc.code != nil {
				requireCode(t, got, tc.code)
			}
			for _, m := range tc.matches {
				require.ErrorIs(t, got, m)
			}
		})
	}

	t.Run("an error with nothing of franz-go's is kept as it is", func(t *testing.T) {
		t.Parallel()

		err := fmt.Errorf("kafka: committing: %w", context.DeadlineExceeded)
		require.Same(t, err, kafka.FromClient(err))
		require.NoError(t, kafka.FromClient(nil))
	})
}

func TestRecordError(t *testing.T) {
	t.Parallel()

	timeouts := []struct {
		name string
		err  error
	}{
		{name: "the call's DeliveryTimeout deadline", err: context.DeadlineExceeded},
		{name: "the client's own timeout", err: kgo.ErrRecordTimeout},
		{name: "the client's own timeout, with its last error", err: fmt.Errorf("%w, last err: %w", kgo.ErrRecordTimeout, kerr.NotEnoughReplicas)},
	}
	for _, tc := range timeouts {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := kafka.RecordError(tc.err)
			require.ErrorIs(t, got, kafka.ErrDeliveryTimeout)
			require.ErrorIs(t, got, context.DeadlineExceeded)
			require.ErrorContains(t, got, kafka.ErrDeliveryTimeout.Error())
			requireNoFranzGo(t, got)
		})
	}

	t.Run("a closed client", func(t *testing.T) {
		t.Parallel()

		got := kafka.RecordError(kgo.ErrClientClosed)
		require.ErrorIs(t, got, kafka.ErrProducerClosed)
		requireNoFranzGo(t, got)
	})

	t.Run("a Kafka error code", func(t *testing.T) {
		t.Parallel()

		got := kafka.RecordError(kerr.MessageTooLarge)
		requireCode(t, got, messageTooLarge)
		require.NotErrorIs(t, got, kafka.ErrDeliveryTimeout)
		require.NotErrorIs(t, got, context.DeadlineExceeded)
	})

	t.Run("an acknowledged record", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, kafka.RecordError(nil))
	})
}

func TestProduce_a_record_refused_with_MESSAGE_TOO_LARGE_returns_an_Error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		c.Fault(kfake.Fault{Keys: []kmsg.Key{kmsg.Produce}, Topic: topic, Err: kerr.MessageTooLarge, Count: -1})

		var err error
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx, value(0))
			return nil
		})

		requireCode(t, err, messageTooLarge)
		require.ErrorContains(t, err, "kafka: producing to orders, partition 0: ")
		requireNoFranzGo(t, err)

		errType := spanAttrs(spanNamed(t, tel, sendSpan))["error.type"].AsString()
		requireErrorTypeNotFranzGo(t, errType)
		sent, ok := metricNamed(t, tel, serviceScope, "messaging.client.sent.messages").Data.(metricdata.Sum[int64])
		require.True(t, ok)
		require.Len(t, sent.DataPoints, 1)
		require.Equal(t, errType, setAttrs(sent.DataPoints[0].Attributes)["error.type"])
	})
}

func TestServe_ended_by_TOPIC_AUTHORIZATION_FAILED_returns_an_Error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 3)
		c.Fault(kfake.Fault{Keys: []kmsg.Key{kmsg.Fetch}, Topic: topic, Err: kerr.TopicAuthorizationFailed, Count: -1})

		_, status := humustest.Start(t, h.Host(), consumes(settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error {
			return nil
		}))

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		requireCode(t, st.Cause(), topicAuthorizationFailed)
		require.ErrorContains(t, st.Cause(), "kafka: fetching orders, partition 0: ")
		requireNoFranzGo(t, st.Cause())
		requireErrorTypeNotFranzGo(t, semconv.ErrorType(st.Cause()).Value.AsString())
	})
}

func TestService_refused_SASL_credentials_give_SASL_AUTHENTICATION_FAILED(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		user, password := rand.Text(), rand.Text()
		c, _ := saslCluster(t, h, "PLAIN", user, password)

		s := settings(c.ListenAddrs()...)
		s.SASL = kafka.Plain(user, rand.Text())
		_, status := humustest.Start(t, h.Host(), consumes(s, func(context.Context, kafka.Message) error {
			return nil
		}))

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		requireCode(t, st.Cause(), saslAuthenticationFailed)
		requireNoFranzGo(t, st.Cause())
		requireErrorTypeNotFranzGo(t, semconv.ErrorType(st.Cause()).Value.AsString())
	})
}

func TestProducer_refused_SASL_credentials_give_SASL_AUTHENTICATION_FAILED(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		user, password := rand.Text(), rand.Text()
		c, _ := saslCluster(t, h, "PLAIN", user, password)

		ps := producerSettings(c.ListenAddrs()...)
		ps.SASL = kafka.Plain(user, rand.Text())
		var err error
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx, value(0))
			return nil
		})

		requireCode(t, err, saslAuthenticationFailed)
		requireNoFranzGo(t, err)
		refusals := records(tel.Logs(), refusedBody)
		require.Len(t, refusals, 1)
		requireErrorTypeNotFranzGo(t, attrs(refusals[0])["error.type"])
	})
}

func TestProduce_a_record_past_DeliveryTimeout_matches_ErrDeliveryTimeout(t *testing.T) {
	t.Run("the call's deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			holdProduce(t, c) // released when the test ends

			ps := producerSettings(c.ListenAddrs()...)
			ps.DeliveryTimeout = 2 * time.Second
			var err error
			tel := humustest.NewTelemetry()
			producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
				err = p.Produce(ctx, value(0))
				return nil
			})

			require.ErrorIs(t, err, kafka.ErrDeliveryTimeout)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			requireNoFranzGo(t, err)
			requireErrorTypeNotFranzGo(t, spanAttrs(spanNamed(t, tel, sendSpan))["error.type"].AsString())
		})
	})

	t.Run("while the cluster answers with a retriable error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			answerProduce(c, kerr.NotEnoughReplicas.Code, always)

			ps := producerSettings(c.ListenAddrs()...)
			ps.DeliveryTimeout = 2 * time.Second
			var err error
			tel := humustest.NewTelemetry()
			producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
				err = p.Produce(ctx, value(0))
				return nil
			})

			require.ErrorIs(t, err, kafka.ErrDeliveryTimeout)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			requireNoFranzGo(t, err)
			requireErrorTypeNotFranzGo(t, spanAttrs(spanNamed(t, tel, sendSpan))["error.type"].AsString())
		})
	})
}

func TestProducer_a_closed_Producer_fails_records_with_ErrProducerClosed(t *testing.T) {
	t.Run("a Produce after the OnStop function has run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)

			after := make(chan struct{})
			produced := make(chan error, 1)
			tel := humustest.NewTelemetry()
			_, status := humustest.Start(t, h.Host(), produces(producerSettings(c.ListenAddrs()...), func(_ context.Context, p *kafka.Producer) error {
				go func() {
					<-after
					produced <- p.Produce(context.Background(), value(0))
				}()
				return nil
			}), humustest.WithApp(humus.WithTelemetry(tel.Builder())))

			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
			close(after)
			err := <-produced
			require.ErrorIs(t, err, kafka.ErrProducerClosed)
			requireNoFranzGo(t, err)
			requireErrorTypeNotFranzGo(t, semconv.ErrorType(err).Value.AsString())
		})
	})

	t.Run("a record the grace period cut short", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			held, _ := holdProduce(t, c) // released when the test ends

			produced := make(chan error, 1)
			tel := humustest.NewTelemetry()
			_, status := humustest.Start(t, h.Host(), produces(producerSettings(c.ListenAddrs()...), func(_ context.Context, p *kafka.Producer) error {
				go func() { produced <- p.Produce(context.Background(), value(0)) }()
				<-held
				return nil
			}),
				humustest.WithApp(humus.WithTelemetry(tel.Builder())),
				humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s"}),
			)

			st := <-status
			require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
			err := <-produced
			require.ErrorIs(t, err, kafka.ErrProducerClosed)
			requireNoFranzGo(t, err)
			requireErrorTypeNotFranzGo(t, spanAttrs(spanNamed(t, tel, sendSpan))["error.type"].AsString())
		})
	})
}

// cutShortRuns is how many times each test of a record the grace period cut
// short shuts the App down.
const cutShortRuns = 10

// cutShortRun is what one shutdown of cutShort's App showed.
type cutShortRun struct {
	code int
	err  error
	// spans is the number of send spans recorded, and errType and status
	// the error.type and status code of the first.
	spans   int
	errType string
	status  codes.Code
	// sent is the sum of messaging.client.sent.messages, and durations the
	// count of messaging.client.operation.duration, as the SDK's Shutdown
	// collected them. sentErrTypes and durationErrTypes are the error.type
	// of each of their data points.
	sent             int64
	durations        uint64
	sentErrTypes     []string
	durationErrTypes []string
	// events are the steps of the shutdown, in the order they happened.
	events []string
}

// shutdownSteps is the order in which the steps of one shutdown happened.
type shutdownSteps struct {
	mu   sync.Mutex
	list []string
}

func (e *shutdownSteps) add(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, fmt.Sprintf(format, args...))
}

func (e *shutdownSteps) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

// watchedProvider is a TracerProvider that adds a step when a send span
// ends, before the span is handed to the SDK.
type watchedProvider struct {
	trace.TracerProvider
	ev *shutdownSteps
}

func (p watchedProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return watchedTracer{Tracer: p.TracerProvider.Tracer(name, opts...), ev: p.ev}
}

type watchedTracer struct {
	trace.Tracer
	ev *shutdownSteps
}

func (t watchedTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	ctx, span := t.Tracer.Start(ctx, name, opts...)
	if name != sendSpan {
		return ctx, span
	}
	w := watchedSpan{Span: span, ev: t.ev}
	return trace.ContextWithSpan(ctx, w), w
}

type watchedSpan struct {
	trace.Span
	ev *shutdownSteps
}

func (s watchedSpan) End(opts ...trace.SpanEndOption) {
	s.ev.add("the send span ends")
	s.Span.End(opts...)
}

// watched returns tel's Builder, whose SDK adds a step when a send span
// ends and when its Shutdown starts and returns.
func watched(tel *humustest.Telemetry, ev *shutdownSteps) telemetry.Builder {
	build := tel.Builder()
	return func(ctx context.Context, p bedrock.Process, n telemetry.Network) (telemetry.SDK, error) {
		sdk, err := build(ctx, p, n)
		if err != nil {
			return sdk, err
		}
		sdk.TracerProvider = watchedProvider{TracerProvider: sdk.TracerProvider, ev: ev}
		shutdown := sdk.Shutdown
		sdk.Shutdown = func(ctx context.Context) error {
			ev.add("the telemetry SDK's Shutdown starts")
			defer ev.add("the telemetry SDK's Shutdown returns")
			return shutdown(ctx)
		}
		return sdk, nil
	}
}

// cutShort runs, in a bubble of its own, the App of the subtest "a record
// the grace period cut short": a job hands one record to a Producer whose
// cluster holds every Produce request, and returns once the cluster holds
// it, so the Producer's OnStop function's flush runs out the 2s grace
// period and closing the client fails the record.
func cutShort(t *testing.T) cutShortRun {
	t.Helper()
	var run cutShortRun
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		held, _ := holdProduce(t, c) // released when the test ends

		ev := &shutdownSteps{}
		tel := humustest.NewTelemetry()
		produced := make(chan error, 1)
		job := produces(producerSettings(c.ListenAddrs()...), func(_ context.Context, p *kafka.Producer) error {
			go func() {
				err := p.Produce(context.Background(), value(0))
				ev.add("Produce returns: %v", err)
				produced <- err
			}()
			<-held
			ev.add("the cluster holds the Produce request")
			return nil
		})
		init := func(ctx context.Context, s *humus.Setup, n humus.Host) ([]humus.Service, error) {
			// OnStop functions run in reverse order of registration, so this
			// one runs right after the Producer's.
			s.OnStop(func(context.Context) error {
				ev.add("the Producer's OnStop function has returned")
				return nil
			})
			return job(ctx, s, n)
		}
		_, status := humustest.Start(t, h.Host(), init,
			humustest.WithApp(humus.WithTelemetry(watched(tel, ev))),
			humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s"}),
		)

		run.code = (<-status).Code()
		ev.add("the App has exited")
		run.err = <-produced

		spans := spansNamed(tel, sendSpan)
		run.spans = len(spans)
		if len(spans) > 0 {
			run.errType = spanAttrs(spans[0])["error.type"].AsString()
			run.status = spans[0].Status().Code
		}
		rm, err := tel.Metrics(t.Context())
		require.NoError(t, err)
		for _, sm := range rm.ScopeMetrics {
			if sm.Scope.Name != serviceScope {
				continue
			}
			for _, m := range sm.Metrics {
				switch data := m.Data.(type) {
				case metricdata.Sum[int64]:
					if m.Name == "messaging.client.sent.messages" {
						for _, dp := range data.DataPoints {
							run.sent += dp.Value
							run.sentErrTypes = append(run.sentErrTypes, setAttrs(dp.Attributes)["error.type"])
						}
					}
				case metricdata.Histogram[float64]:
					if m.Name == "messaging.client.operation.duration" {
						for _, dp := range data.DataPoints {
							run.durations += dp.Count
							run.durationErrTypes = append(run.durationErrTypes, setAttrs(dp.Attributes)["error.type"])
						}
					}
				}
			}
		}
		run.events = ev.all()
	})
	return run
}

// cutShortOnOneCore shuts cutShort's App down cutShortRuns times with
// GOMAXPROCS at 1, so no goroutine runs beside the lifecycle's while it
// goes from the Producer's OnStop function to the telemetry SDK's
// Shutdown, and franz-go's goroutine that hands the closed client's
// records their results runs only once Close has returned. It requires
// every shutdown to exit 1 with the record failed with ErrProducerClosed,
// and logs what each showed. Each shutdown is a bubble of its own, and
// the assertions are outside them, so every shutdown is counted.
func cutShortOnOneCore(t *testing.T) []cutShortRun {
	t.Helper()
	prev := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	runs := make([]cutShortRun, cutShortRuns)
	for i := range runs {
		runs[i] = cutShort(t)
	}
	for i, run := range runs {
		t.Logf("shutdown %d: exit %d, send spans %d (error.type %q, status %v), sent.messages %d %q, operation.duration count %d %q",
			i, run.code, run.spans, run.errType, run.status, run.sent, run.sentErrTypes, run.durations, run.durationErrTypes)
		t.Logf("shutdown %d: %s", i, strings.Join(run.events, " -> "))
	}
	for i, run := range runs {
		require.Equal(t, humus.ExitFailure, run.code, "shutdown %d", i)
		require.ErrorIs(t, run.err, kafka.ErrProducerClosed, "shutdown %d", i)
	}
	return runs
}

// producerClosedType is ErrProducerClosed's error.type.
var producerClosedType = semconv.ErrorType(kafka.ErrProducerClosed).Value.AsString()

func TestProducer_records_the_send_span_of_a_record_the_grace_period_cut_short(t *testing.T) {
	runs := cutShortOnOneCore(t)
	var lost []int
	for i, run := range runs {
		if run.spans != 1 || run.errType != producerClosedType || run.status != codes.Error {
			lost = append(lost, i)
		}
	}
	require.Empty(t, lost, "shutdowns that recorded no send span with ErrProducerClosed's error.type and an error status, of %d", len(runs))
}

func TestProducer_counts_a_record_the_grace_period_cut_short_in_its_messaging_instruments(t *testing.T) {
	runs := cutShortOnOneCore(t)
	var lost []int
	for i, run := range runs {
		if run.sent != 1 || run.durations != 1 ||
			!slices.Equal(run.sentErrTypes, []string{producerClosedType}) ||
			!slices.Equal(run.durationErrTypes, []string{producerClosedType}) {
			lost = append(lost, i)
		}
	}
	require.Empty(t, lost, "shutdowns whose last collection missed the record, with ErrProducerClosed's error.type, in messaging.client.sent.messages or messaging.client.operation.duration, of %d", len(runs))
}
