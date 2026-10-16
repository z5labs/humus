// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// The SDK's providers merge the attributes of OTEL_RESOURCE_ATTRIBUTES into
// any resource they are given, reading the real environment as they are
// built, and offer no option to stop them. So New puts its own resource
// back before anything is exported: traces and metrics through their
// exporters, which can be handed a different resource, and logs, whose
// records cannot be, by removing the attributes it did not build from each
// OTLP request. Console logs are written without a resource, so they need
// neither.

// resourceSpanExporter exports every span with res as its resource.
type resourceSpanExporter struct {
	sdktrace.SpanExporter
	res *resource.Resource
}

func (e resourceSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = resourceSpan{ReadOnlySpan: s, res: e.res}
	}
	return e.SpanExporter.ExportSpans(ctx, out)
}

type resourceSpan struct {
	sdktrace.ReadOnlySpan
	res *resource.Resource
}

func (s resourceSpan) Resource() *resource.Resource { return s.res }

// resourceMetricExporter exports every collection with res as its
// resource.
type resourceMetricExporter struct {
	sdkmetric.Exporter
	res *resource.Resource
}

func (e resourceMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	out := *rm
	out.Resource = e.res
	return e.Exporter.Export(ctx, &out)
}

// resourceFilter removes from an OTLP logs request every resource attribute
// whose key New's resource does not have. The SDK's merge lets New's
// attributes win over the environment's, so what is left is exactly New's
// resource.
type resourceFilter struct {
	keys map[string]bool
}

func newResourceFilter(res *resource.Resource) resourceFilter {
	keys := make(map[string]bool, res.Len())
	for _, kv := range res.Attributes() {
		keys[string(kv.Key)] = true
	}
	return resourceFilter{keys: keys}
}

func (f resourceFilter) logs(req *collogspb.ExportLogsServiceRequest) {
	for _, rl := range req.GetResourceLogs() {
		r := rl.GetResource()
		if r == nil {
			continue
		}
		r.Attributes = slices.DeleteFunc(r.Attributes, func(kv *commonpb.KeyValue) bool {
			return !f.keys[kv.GetKey()]
		})
	}
}

// unaryInterceptor filters the logs requests sent over gRPC.
func (f resourceFilter) unaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if r, ok := req.(*collogspb.ExportLogsServiceRequest); ok {
		f.logs(r)
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

// resourceFilterTransport filters the logs requests sent over HTTP. It
// relies on the exporters sending protobuf, uncompressed, which New
// configures them to do; any other request passes through as it is.
type resourceFilterTransport struct {
	base   http.RoundTripper
	filter resourceFilter
}

func (t *resourceFilterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != logsPath || req.Body == nil || req.Header.Get("Content-Encoding") != "" {
		return t.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	var msg collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	t.filter.logs(&msg)
	if body, err = proto.Marshal(&msg); err != nil {
		return nil, err
	}

	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return t.base.RoundTrip(out)
}
