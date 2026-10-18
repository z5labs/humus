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
	"net/http"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/fit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run front as a whole App inside a synctest
// bubble: a grpcserver Service whose handlers call the backend dependency
// through a Client. front and backend run on one humustest Host, over TLS
// from humustest.NewCA. The test is the edge: it makes RPCs to front whose
// baggage, in their metadata, carries tenant=acme and, for most, a scenario
// written with jsonscenario. front trusts the edge, so its scenarios reach
// the Client.

// scenarioKey is the baggage member the tests' codec reads.
const scenarioKey = "acme.fault"

// The injection points of backend's methods that front calls.
const (
	emptyCallPoint  = "grpc:grpc.testing.TestService/EmptyCall"
	streamCallPoint = "grpc:grpc.testing.TestService/StreamingOutputCall"
)

// streamCallSpan is the name of the span of an attempt at
// StreamingOutputCall.
const streamCallSpan = "grpc.testing.TestService/StreamingOutputCall"

// injectedErrorType is the error.type of an injected failure.
const injectedErrorType = "github.com/z5labs/humus/fault.InjectedError"

// The span events of failure injection.
const (
	evaluationEvent = "humus.fit.evaluation"
	stripEvent      = "humus.fit.strip"
)

// abortAt aborts every attempt at point.
func abortAt(point string) *fit.Scenario {
	return &fit.Scenario{Rules: []fit.Rule{{Point: point, Fault: fit.Fault{Abort: true}}}}
}

// delayEmptyCall delays every attempt at EmptyCall by d.
func delayEmptyCall(d time.Duration) *fit.Scenario {
	return &fit.Scenario{Rules: []fit.Rule{{Point: emptyCallPoint, Fault: fit.Fault{Delay: d}}}}
}

// sparesBackend aborts a dependency other than backend.
var sparesBackend = abortAt("grpc:ledger.v1.Ledger/Post")

// faultsCodec returns the tests' jsonscenario Codec.
func faultsCodec(t *testing.T) jsonscenario.Codec {
	t.Helper()
	c, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	return c
}

// backendRPC is what backend saw of an RPC it served, in its metadata as
// it arrived.
type backendRPC struct {
	scenario bool
	tenant   string
}

// scenarioBackend is the dependency. Its EmptyCall answers OK, and its
// StreamingOutputCall sends one message. Each records what it saw.
type scenarioBackend struct {
	testpb.UnimplementedTestServiceServer

	t    *testing.T
	mu   sync.Mutex
	rpcs []backendRPC
}

func (b *scenarioBackend) record(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	var raw string
	if vs := md.Get("baggage"); len(vs) > 0 {
		raw = vs[0]
	}
	bag, err := baggage.Parse(raw)
	require.NoError(b.t, err)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rpcs = append(b.rpcs, backendRPC{
		scenario: bag.Member(scenarioKey).Key() != "",
		tenant:   bag.Member("tenant").Value(),
	})
}

// frontCall is what one of front's handlers saw of one edge RPC.
type frontCall struct {
	// scenario is whether the handler's context carried a scenario.
	scenario bool
	// err is the error the Client's call to backend returned.
	err error
}

// front is the end user's Service. Its EmptyCall calls backend's
// EmptyCall, and its StreamingOutputCall opens backend's and reads it to
// its end, both through the Client, and each records what it saw.
type front struct {
	testpb.UnimplementedTestServiceServer

	codec  jsonscenario.Codec
	client testpb.TestServiceClient

	mu    sync.Mutex
	calls []frontCall
}

func (f *front) record(ctx context.Context, err error) {
	_, carried, _ := fit.ScenarioFromContext(ctx, f.codec)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, frontCall{scenario: carried, err: err})
}

func (f *front) EmptyCall(ctx context.Context, _ *testpb.Empty) (*testpb.Empty, error) {
	_, err := f.client.EmptyCall(ctx, &testpb.Empty{})
	f.record(ctx, err)
	if err != nil {
		return nil, err
	}
	return &testpb.Empty{}, nil
}

func (f *front) StreamingOutputCall(_ *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	ctx := stream.Context()
	err := drainStream(f.client.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{
		ResponseParameters: []*testpb.ResponseParameters{{}},
	}))
	f.record(ctx, err)
	if err != nil {
		return err
	}
	return stream.Send(&testpb.StreamingOutputCallResponse{})
}

// drainStream receives from stream until it ends, and returns nil if it
// ended cleanly.
func drainStream(stream grpc.ServerStreamingClient[testpb.StreamingOutputCallResponse], err error) error {
	if err != nil {
		return err
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (b *scenarioBackend) EmptyCall(ctx context.Context, _ *testpb.Empty) (*testpb.Empty, error) {
	b.record(ctx)
	return &testpb.Empty{}, nil
}

func (b *scenarioBackend) StreamingOutputCall(_ *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	b.record(stream.Context())
	return stream.Send(&testpb.StreamingOutputCallResponse{})
}

// frontRun configures a run of front.
type frontRun struct {
	// faults turns failure injection on, with the tests' codec.
	faults bool
	// change changes the Client's settings, when it is set.
	change func(*grpcclient.Settings)
	// stream makes the edge call front's StreamingOutputCall, rather than
	// its EmptyCall.
	stream bool
	// cancelAfter, when it is set, cancels each edge RPC after it.
	cancelAfter time.Duration
	// scenarios are the edge's RPCs, made one after another: each carries
	// its scenario, or none when it is nil.
	scenarios []*fit.Scenario
}

// frontResult is what a run recorded.
type frontResult struct {
	// edge is the error each of the edge's RPCs returned.
	edge    []error
	calls   []frontCall
	backend []backendRPC
	tel     *humustest.Telemetry
}

// runFront runs backend and front, makes the edge's RPCs to front, one
// after another, then stops both and returns what happened.
func runFront(t *testing.T, run frontRun) frontResult {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	tel := humustest.NewTelemetry()
	codec := faultsCodec(t)

	b := &scenarioBackend{t: t}
	backendCert := ca.Issue(t, "backend")
	bp, backendExit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: "backend:9090",
			TLS:  &tls.Config{Certificates: []tls.Certificate{backendCert.TLS()}},
		})
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, b)
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening("backend:9090")

	f := &front{codec: codec}
	frontCert := ca.Issue(t, "front")
	init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := settingsFor(ca, "backend:9090")
		if run.change != nil {
			run.change(&settings)
		}
		c, err := grpcclient.NewClient(s, settings)
		if err != nil {
			return nil, err
		}
		f.client = testpb.NewTestServiceClient(c)
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr:          "front:8443",
			TLS:           &tls.Config{Certificates: []tls.Certificate{frontCert.TLS()}},
			TrustScenario: func(context.Context, string) bool { return true }, // the edge
		})
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, f)
		return []humus.Service{svc}, nil
	}
	opts := []humus.Option{humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())}
	if run.faults {
		opts = append(opts, humus.WithFaults(func(context.Context, *humus.Setup) (fault.Settings, error) {
			d := fault.DefaultSettings()
			d.Decoder = codec
			return d, nil
		}))
	}
	fp, frontExit := humustest.Start(t, init, humustest.WithApp(opts...))
	<-h.Listening("front:8443")

	edge := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "front:8443", ca.Pool()))
	var edgeErrs []error
	for _, sc := range run.scenarios {
		ctx := edgeContext(t, codec, sc)
		if run.cancelAfter > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			stop := time.AfterFunc(run.cancelAfter, cancel)
			defer stop.Stop()
			defer cancel()
		}
		var err error
		if run.stream {
			err = drainStream(edge.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{}))
		} else {
			_, err = edge.EmptyCall(ctx, &testpb.Empty{})
		}
		edgeErrs = append(edgeErrs, err)
	}

	<-fp.Notified(syscall.SIGTERM)
	fp.Signal(syscall.SIGTERM)
	st := <-frontExit
	require.Zero(t, st.Code(), "front's cause: %v", st.Cause())
	<-bp.Notified(syscall.SIGTERM)
	bp.Signal(syscall.SIGTERM)
	st = <-backendExit
	require.Zero(t, st.Code(), "backend's cause: %v", st.Cause())

	f.mu.Lock()
	defer f.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	require.Len(t, f.calls, len(edgeErrs))
	return frontResult{edge: edgeErrs, calls: f.calls, backend: b.rpcs, tel: tel}
}

// edgeContext returns a context whose outgoing metadata carries baggage
// with tenant=acme and, unless sc is nil, sc written with c.
func edgeContext(t *testing.T, c jsonscenario.Codec, sc *fit.Scenario) context.Context {
	t.Helper()
	tenant, err := baggage.NewMemberRaw("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(tenant)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(t.Context(), b)
	if sc != nil {
		ctx, err = fit.ContextWithScenario(ctx, c, *sc)
		require.NoError(t, err)
	}
	header := http.Header{}
	propagation.Baggage{}.Inject(ctx, propagation.HeaderCarrier(header))
	return metadata.AppendToOutgoingContext(t.Context(), "baggage", header.Get("baggage"))
}

// repeatScenario returns n of sc.
func repeatScenario(sc *fit.Scenario, n int) []*fit.Scenario {
	out := make([]*fit.Scenario, n)
	for i := range out {
		out[i] = sc
	}
	return out
}

// ownSpans returns the client spans of the Client's own scope in tel: the
// spans of attempts that never reached grpc-go.
func ownSpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, s := range clientSpans(tel) {
		if s.InstrumentationScope().Name == clientScope {
			spans = append(spans, s)
		}
	}
	return spans
}

// frontSpan returns front's server span of the edge's one RPC named name.
func frontSpan(t *testing.T, tel *humustest.Telemetry, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range otelgrpcSpans(tel, trace.SpanKindServer) {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "front's server spans named %s", name)
	return found[0]
}

// spanEvents returns the events of span named name.
func spanEvents(span sdktrace.ReadOnlySpan, name string) []sdktrace.Event {
	var evs []sdktrace.Event
	for _, ev := range span.Events() {
		if ev.Name == name {
			evs = append(evs, ev)
		}
	}
	return evs
}

// eventAttrs returns ev's attributes by key.
func eventAttrs(ev sdktrace.Event) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value)
	for _, kv := range ev.Attributes {
		m[kv.Key] = kv.Value
	}
	return m
}

// requireAborted fails t unless err is an abort injected at point, whose
// status is Unavailable.
func requireAborted(t *testing.T, err error, point string) {
	t.Helper()
	require.Equal(t, codes.Unavailable, status.Code(err), "error: %v", err)
	require.ErrorIs(t, err, fit.ErrInjected)
	var ie *fault.InjectedError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, point, ie.Point)
	require.NoError(t, ie.Err, "an abort's Err is nil")
}

// requireOwnSpan fails t unless span is the Client's own span of an
// attempt at name that a fault ended with code, named code.
func requireOwnSpan(t *testing.T, span sdktrace.ReadOnlySpan, name, code string) {
	t.Helper()
	require.Equal(t, name, span.Name())
	require.Equal(t, trace.SpanKindClient, span.SpanKind())
	require.Equal(t, otelcodes.Error, span.Status().Code)
	a := attrs(span)
	require.Equal(t, attribute.StringValue(injectedErrorType), a["error.type"])
	require.Equal(t, attribute.StringValue(code), a["rpc.response.status_code"])
	require.Equal(t, attribute.StringValue(name), a["rpc.method"])
	require.Equal(t, attribute.StringValue("grpc"), a["rpc.system.name"])
	require.Equal(t, attribute.StringValue("backend"), a["server.address"])
	require.Equal(t, attribute.Int64Value(9090), a["server.port"])
}

func TestClient_an_aborted_attempt(t *testing.T) {
	t.Run("returns Unavailable, is retried, and opens the breaker, which rejects a call with no scenario", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			abort := abortAt(emptyCallPoint)
			res := runFront(t, frontRun{faults: true, scenarios: []*fit.Scenario{abort, abort, nil}})

			require.Empty(t, res.backend, "an aborted attempt, or a rejected one, reached backend")
			for _, err := range res.edge {
				require.Equal(t, codes.Unavailable, status.Code(err), "error: %v", err)
			}
			require.True(t, res.calls[0].scenario)
			requireAborted(t, res.calls[0].err, emptyCallPoint)

			// The second call's attempts are the fourth and the fifth in a
			// row. The fifth drains the retry budget to half, so the call
			// ends there, and opens the breaker, which rejects the third
			// call at once.
			requireAborted(t, res.calls[1].err, emptyCallPoint)
			require.ErrorIs(t, res.calls[2].err, circuit.ErrRejected)
			var ie *fault.InjectedError
			require.False(t, errors.As(res.calls[2].err, &ie), "a call with no scenario failed with an injected error")

			require.Len(t, ownSpans(res.tel), 6, "three aborted attempts, two, and one rejected")
			require.Empty(t, otelgrpcSpans(res.tel, trace.SpanKindClient), "an attempt reached grpc-go")
			rm := collect(t, t.Context(), res.tel)
			require.Equal(t, map[string]int64{
				"humus.circuit.outcome=failure":  5,
				"humus.circuit.outcome=rejected": 1,
			}, points(t, rm, "humus.circuit.calls"))
			require.Equal(t, breakerIs(0, 1, 0), state(t, rm))
		})
	})

	t.Run("is a client span with its error.type, and an evaluation on front's server span", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFront(t, frontRun{faults: true, scenarios: []*fit.Scenario{abortAt(emptyCallPoint)}})
			require.Empty(t, res.backend)
			requireAborted(t, res.calls[0].err, emptyCallPoint)

			server := frontSpan(t, res.tel, emptyCallSpan)
			spans := ownSpans(res.tel)
			require.Len(t, spans, 3, "an attempt per MaxAttempts")
			for _, span := range spans {
				requireOwnSpan(t, span, emptyCallSpan, "UNAVAILABLE")
				require.Equal(t, server.SpanContext().SpanID(), span.Parent().SpanID(), "the attempt is a child of the caller's span")
				require.Empty(t, span.Events())
			}

			evs := spanEvents(server, evaluationEvent)
			require.Len(t, evs, 3, "an evaluation per attempt")
			for _, ev := range evs {
				a := eventAttrs(ev)
				require.Equal(t, attribute.StringValue(emptyCallPoint), a["humus.fit.point"])
				require.Equal(t, attribute.StringValue(emptyCallPoint), a["humus.fit.rule"])
				require.Equal(t, attribute.BoolValue(true), a["humus.fit.abort"])
			}
			require.Empty(t, spanEvents(server, stripEvent), "an aborted attempt sends nothing to strip")
		})
	})
}

func TestClient_an_aborted_stream_open(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		abort := abortAt(streamCallPoint)
		res := runFront(t, frontRun{faults: true, stream: true, scenarios: []*fit.Scenario{abort, abort, nil}})

		require.Empty(t, res.backend, "an aborted open, or a rejected one, reached backend")
		for _, err := range res.edge {
			require.Equal(t, codes.Unavailable, status.Code(err), "error: %v", err)
		}
		requireAborted(t, res.calls[0].err, streamCallPoint)
		requireAborted(t, res.calls[1].err, streamCallPoint)
		require.ErrorIs(t, res.calls[2].err, circuit.ErrRejected)

		spans := ownSpans(res.tel)
		require.Len(t, spans, 6, "three aborted opens, two, and one rejected")
		for _, span := range spans[:5] {
			requireOwnSpan(t, span, streamCallSpan, "UNAVAILABLE")
		}
		rm := collect(t, t.Context(), res.tel)
		require.Equal(t, map[string]int64{
			"humus.circuit.outcome=failure":  5,
			"humus.circuit.outcome=rejected": 1,
		}, points(t, rm, "humus.circuit.calls"))
		require.Equal(t, breakerIs(0, 1, 0), state(t, rm))
	})
}

func TestClient_a_delayed_attempt(t *testing.T) {
	t.Run("waits the delay before grpc-go, and succeeds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFront(t, frontRun{faults: true, scenarios: []*fit.Scenario{delayEmptyCall(time.Second)}})

			require.NoError(t, res.edge[0])
			require.NoError(t, res.calls[0].err)
			require.Len(t, res.backend, 1)
			require.Empty(t, ownSpans(res.tel))

			server := frontSpan(t, res.tel, emptyCallSpan)
			spans := otelgrpcSpans(res.tel, trace.SpanKindClient)
			require.Len(t, spans, 1)
			require.Equal(t, time.Second, spans[0].StartTime().Sub(server.StartTime()), "the attempt didn't wait the delay")
			evs := spanEvents(server, evaluationEvent)
			require.Len(t, evs, 1)
			require.Equal(t, attribute.Float64Value(1), eventAttrs(evs[0])["humus.fit.delay"])
		})
	})

	t.Run("cut short by Timeout ends as DeadlineExceeded", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFront(t, frontRun{
				faults:    true,
				change:    func(s *grpcclient.Settings) { s.Timeout = 2 * time.Second },
				scenarios: []*fit.Scenario{delayEmptyCall(5 * time.Second)},
			})

			require.Empty(t, res.backend)
			require.Equal(t, codes.DeadlineExceeded, status.Code(res.edge[0]), "error: %v", res.edge[0])
			err := res.calls[0].err
			require.Equal(t, codes.DeadlineExceeded, status.Code(err), "error: %v", err)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotErrorIs(t, err, fit.ErrInjected)
			var ie *fault.InjectedError
			require.ErrorAs(t, err, &ie)
			require.Equal(t, emptyCallPoint, ie.Point)

			spans := ownSpans(res.tel)
			require.Len(t, spans, 1, "a deadline isn't retried")
			requireOwnSpan(t, spans[0], emptyCallSpan, "DEADLINE_EXCEEDED")
			require.Equal(t, 2*time.Second, spans[0].EndTime().Sub(spans[0].StartTime()), "the span isn't the attempt's wait")

			rm := collect(t, t.Context(), res.tel)
			require.Equal(t, map[string]int64{"humus.circuit.outcome=failure": 1}, points(t, rm, "humus.circuit.calls"))
		})
	})

	t.Run("cut short by the caller ends as Canceled, which the breaker ignores", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFront(t, frontRun{
				faults:      true,
				cancelAfter: time.Second,
				scenarios:   []*fit.Scenario{delayEmptyCall(5 * time.Second)},
			})

			require.Empty(t, res.backend)
			require.Equal(t, codes.Canceled, status.Code(res.edge[0]), "error: %v", res.edge[0])
			err := res.calls[0].err
			require.Equal(t, codes.Canceled, status.Code(err), "error: %v", err)
			require.ErrorIs(t, err, context.Canceled)
			var ie *fault.InjectedError
			require.ErrorAs(t, err, &ie)

			spans := ownSpans(res.tel)
			require.Len(t, spans, 1)
			requireOwnSpan(t, spans[0], emptyCallSpan, "CANCELLED")

			rm := collect(t, t.Context(), res.tel)
			require.Equal(t, map[string]int64{"humus.circuit.outcome=ignored": 1}, points(t, rm, "humus.circuit.calls"))
		})
	})
}

func TestClient_the_end_users_Classify_decides_an_injected_failure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		classify := grpcclient.DefaultSettings().Classify
		spare := func(method string, err error, trailer metadata.MD) grpcclient.Classification {
			var ie *fault.InjectedError
			if errors.As(err, &ie) {
				return grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Ignored}
			}
			return classify(method, err, trailer)
		}
		res := runFront(t, frontRun{
			faults:    true,
			change:    func(s *grpcclient.Settings) { s.Classify = spare },
			scenarios: append(repeatScenario(abortAt(emptyCallPoint), 6), nil),
		})

		for _, c := range res.calls[:6] {
			requireAborted(t, c.err, emptyCallPoint)
		}
		require.NoError(t, res.edge[6], "the breaker opened")
		require.Len(t, res.backend, 1)
		require.Len(t, ownSpans(res.tel), 6, "one attempt per call")

		rm := collect(t, t.Context(), res.tel)
		require.Equal(t, map[string]int64{
			"humus.circuit.outcome=ignored": 6,
			"humus.circuit.outcome=success": 1,
		}, points(t, rm, "humus.circuit.calls"))
		require.Equal(t, breakerIs(1, 0, 0), state(t, rm))
	})
}

func TestClient_ForwardScenario(t *testing.T) {
	// requireSent fails t unless backend was called once, with tenant=acme,
	// and returns what it saw and front's server span.
	requireSent := func(t *testing.T, res frontResult) (backendRPC, sdktrace.ReadOnlySpan) {
		t.Helper()
		require.NoError(t, res.edge[0])
		require.True(t, res.calls[0].scenario, "front's handler didn't see the trusted scenario")
		require.Len(t, res.backend, 1)
		got := res.backend[0]
		require.Equal(t, "acme", got.tenant, "a baggage member other than the scenario was lost")
		server := frontSpan(t, res.tel, emptyCallSpan)
		require.Len(t, spanEvents(server, evaluationEvent), 1)
		return got, server
	}

	t.Run("false strips the scenario", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFront(t, frontRun{faults: true, scenarios: []*fit.Scenario{sparesBackend}})
			got, server := requireSent(t, res)

			require.False(t, got.scenario, "backend saw the scenario")
			strips := spanEvents(server, stripEvent)
			require.Len(t, strips, 1)
			require.Empty(t, strips[0].Attributes)
		})
	})

	t.Run("true sends the scenario on", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFront(t, frontRun{
				faults:    true,
				change:    func(s *grpcclient.Settings) { s.ForwardScenario = true },
				scenarios: []*fit.Scenario{sparesBackend},
			})
			got, server := requireSent(t, res)

			require.True(t, got.scenario, "backend didn't see the scenario")
			require.Empty(t, spanEvents(server, stripEvent))
		})
	})

	t.Run("false strips a stream's scenario too", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFront(t, frontRun{faults: true, stream: true, scenarios: []*fit.Scenario{sparesBackend}})

			require.NoError(t, res.edge[0])
			require.Len(t, res.backend, 1)
			require.False(t, res.backend[0].scenario, "backend saw the scenario")
			require.Equal(t, "acme", res.backend[0].tenant)
			require.Len(t, spanEvents(frontSpan(t, res.tel, streamCallSpan), stripEvent), 1)
		})
	})
}

func TestClient_without_failure_injection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		res := runFront(t, frontRun{scenarios: []*fit.Scenario{abortAt(emptyCallPoint)}})

		require.NoError(t, res.edge[0])
		require.NoError(t, res.calls[0].err)
		require.Len(t, res.backend, 1)
		require.True(t, res.backend[0].scenario, "the scenario didn't reach backend as baggage")
		require.Equal(t, "acme", res.backend[0].tenant)
		require.Empty(t, ownSpans(res.tel))
		server := frontSpan(t, res.tel, emptyCallSpan)
		require.Empty(t, spanEvents(server, evaluationEvent))
		require.Empty(t, spanEvents(server, stripEvent))
	})
}
