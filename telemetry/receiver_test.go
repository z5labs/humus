// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// receiver is an in-process OTLP receiver, over gRPC or http/protobuf,
// that records every request it is sent.
type receiver struct {
	mu       sync.Mutex
	spans    []*tracepb.ResourceSpans
	metrics  []*metricspb.ResourceMetrics
	logs     []*logspb.ResourceLogs
	requests []request
}

// request is what the receiver records of every request, whatever its
// signal.
type request struct {
	signal string
	// headers are the gRPC metadata or the HTTP headers, keys lowercased.
	headers map[string][]string
	// encoding is the HTTP request's Content-Encoding, or the gRPC
	// request's grpc-encoding.
	encoding string
}

// startReceiver serves an OTLP receiver over protocol on ln until the test
// ends.
func startReceiver(t *testing.T, ln net.Listener, protocol telemetry.Protocol) *receiver {
	t.Helper()
	r := &receiver{}
	switch protocol {
	case telemetry.ProtocolGRPC:
		srv := grpc.NewServer()
		coltracepb.RegisterTraceServiceServer(srv, traceService{r: r})
		colmetricspb.RegisterMetricsServiceServer(srv, metricsService{r: r})
		collogspb.RegisterLogsServiceServer(srv, logsService{r: r})
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(srv.Stop)
	case telemetry.ProtocolHTTPProtobuf:
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/traces", r.handle("traces", &coltracepb.ExportTraceServiceRequest{}, &coltracepb.ExportTraceServiceResponse{}))
		mux.HandleFunc("POST /v1/metrics", r.handle("metrics", &colmetricspb.ExportMetricsServiceRequest{}, &colmetricspb.ExportMetricsServiceResponse{}))
		mux.HandleFunc("POST /v1/logs", r.handle("logs", &collogspb.ExportLogsServiceRequest{}, &collogspb.ExportLogsServiceResponse{}))
		srv := &http.Server{Handler: mux}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
	default:
		t.Fatalf("unknown protocol %q", protocol)
	}
	return r
}

func (r *receiver) record(req request, msg proto.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	switch m := msg.(type) {
	case *coltracepb.ExportTraceServiceRequest:
		r.spans = append(r.spans, m.GetResourceSpans()...)
	case *colmetricspb.ExportMetricsServiceRequest:
		r.metrics = append(r.metrics, m.GetResourceMetrics()...)
	case *collogspb.ExportLogsServiceRequest:
		r.logs = append(r.logs, m.GetResourceLogs()...)
	}
}

func (r *receiver) handle(signal string, msg, resp proto.Message) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m := proto.Clone(msg)
		proto.Reset(m)
		if req.Header.Get("Content-Type") != "application/x-protobuf" {
			http.Error(w, "not protobuf", http.StatusUnsupportedMediaType)
			return
		}
		if err := proto.Unmarshal(body, m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		headers := map[string][]string{}
		for k, v := range req.Header {
			headers[strings.ToLower(k)] = v
		}
		r.record(request{signal: signal, headers: headers, encoding: req.Header.Get("Content-Encoding")}, m)

		b, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(b)
	}
}

func grpcRequest(ctx context.Context, signal string) request {
	md, _ := metadata.FromIncomingContext(ctx)
	req := request{signal: signal, headers: map[string][]string(md)}
	if enc := md.Get("grpc-encoding"); len(enc) > 0 {
		req.encoding = enc[0]
	}
	return req
}

type traceService struct {
	coltracepb.UnimplementedTraceServiceServer
	r *receiver
}

func (s traceService) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	s.r.record(grpcRequest(ctx, "traces"), req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

type metricsService struct {
	colmetricspb.UnimplementedMetricsServiceServer
	r *receiver
}

func (s metricsService) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	s.r.record(grpcRequest(ctx, "metrics"), req)
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

type logsService struct {
	collogspb.UnimplementedLogsServiceServer
	r *receiver
}

func (s logsService) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	s.r.record(grpcRequest(ctx, "logs"), req)
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// snapshot returns copies of what the receiver has recorded.
func (r *receiver) snapshot() ([]*tracepb.ResourceSpans, []*metricspb.ResourceMetrics, []*logspb.ResourceLogs, []request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*tracepb.ResourceSpans(nil), r.spans...),
		append([]*metricspb.ResourceMetrics(nil), r.metrics...),
		append([]*logspb.ResourceLogs(nil), r.logs...),
		append([]request(nil), r.requests...)
}

// resourceAttributes returns r's attributes as strings, keyed by name.
func resourceAttributes(r *resourcepb.Resource) map[string]string {
	attrs := map[string]string{}
	for _, kv := range r.GetAttributes() {
		attrs[kv.GetKey()] = anyString(kv.GetValue())
	}
	return attrs
}

func anyString(v *commonpb.AnyValue) string {
	if s, ok := v.GetValue().(*commonpb.AnyValue_StringValue); ok {
		return s.StringValue
	}
	return v.String()
}

// countingDialer dials through host and counts the calls.
type countingDialer struct {
	mu    sync.Mutex
	calls []string
	dial  telemetry.Dialer
}

func newCountingDialer(host *humustest.Host) *countingDialer {
	return &countingDialer{dial: host.Dial}
}

func (d *countingDialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.calls = append(d.calls, address)
	d.mu.Unlock()
	return d.dial(ctx, network, address)
}

func (d *countingDialer) Calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// failingDialer fails the test when it is called.
func failingDialer(t *testing.T) telemetry.Dialer {
	return func(_ context.Context, network, address string) (net.Conn, error) {
		t.Errorf("dialled %s %s, want nothing dialled", network, address)
		return nil, errors.New("dialled")
	}
}

func listen(t *testing.T, host *humustest.Host, address string) net.Listener {
	t.Helper()
	ln, err := host.Listen("tcp", address)
	require.NoError(t, err)
	return ln
}
