// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

// The "humus's own instruments" section of the telemetry page of the docs
// site quotes the code in this file, and TestDocs_code_samples checks that
// every quote still matches it.

// ordersTelemetry returns the end user's telemetry.Builder, whose metrics
// are read by reader, with two Views: one drops humus.circuit.calls, and
// one renames the HTTP client's humus.retry.decisions. telemetry.New takes
// no View, so the Builder builds the MeterProvider itself, and its Shutdown
// shuts that down with the rest of the SDK.
func ordersTelemetry(reader sdkmetric.Reader) telemetry.Builder {
	return func(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
		s := telemetry.DefaultSettings()
		s.ServiceName = "orders"
		// The resource telemetry.New builds for traces and logs:
		// s.ResourceOptions, then service.name.
		res, err := resource.New(ctx, append(slices.Clone(s.ResourceOptions), resource.WithAttributes(semconv.ServiceName(s.ServiceName)))...)
		if err != nil {
			return telemetry.SDK{}, err
		}
		sdk, err := telemetry.New(ctx, s, dial, p.Stdout)
		if err != nil {
			return telemetry.SDK{}, err
		}

		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(reader),
			sdkmetric.WithView(
				// Every client's humus.circuit.calls, dropped.
				sdkmetric.NewView(
					sdkmetric.Instrument{Name: "humus.circuit.calls"},
					sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
				),
				// The HTTP client's humus.retry.decisions, renamed.
				sdkmetric.NewView(
					sdkmetric.Instrument{
						Name:  "humus.retry.decisions",
						Scope: instrumentation.Scope{Name: "github.com/z5labs/humus/httpclient"},
					},
					sdkmetric.Stream{Name: "orders.payments.retries"},
				),
			),
		)
		shutdown := sdk.Shutdown
		sdk.MeterProvider = mp
		sdk.Shutdown = func(ctx context.Context) error {
			return errors.Join(mp.Shutdown(ctx), shutdown(ctx))
		}
		return sdk, nil
	}
}

func TestClient_metrics_a_View_drops_or_renames_them(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		stop := startPayments(t, h, h.Host(), ca, &failing{})
		defer stop()

		reader := sdkmetric.NewManualReader()
		st := runJobWith(t, h.Host(), ordersTelemetry(reader), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			getN(t, ctx, c, 5, "https://payments:8443/") // five 500s open the breaker

			var rm metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(ctx, &rm))
			var names []string
			for _, m := range ownMetrics(rm) {
				names = append(names, m.Name)
			}
			require.ElementsMatch(t, []string{
				"humus.circuit.state",
				"humus.circuit.transitions",
				"orders.payments.retries",
			}, names, "humus.circuit.calls is dropped, and humus.retry.decisions renamed")
			require.Equal(t, map[string]int64{"humus.retry.decision=stopped": 5}, points(t, rm, "orders.payments.retries"))
			require.Equal(t, breakerIs(0, 1, 0), state(t, rm))
			return nil
		})
		requireExitZero(t, st)
	})
}

func TestClient_metrics_read_once_the_App_has_exited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		stop := startPayments(t, h, h.Host(), ca, &failing{})
		defer stop()
		settings := paymentsSettings(t, ca)

		tel := humustest.NewTelemetry()
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			c, err := httpclient.NewClient(s, settings)
			if err != nil {
				return nil, err
			}
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				getN(t, ctx, c, 5, "https://payments:8443/") // five 500s open the breaker
				return nil
			})}, nil
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		st := <-status
		require.Zero(t, st.Code())

		// The App has exited, so Metrics returns what the SDK's Shutdown
		// collected, after the OnStop functions: the state it stopped in.
		rm, err := tel.Metrics(context.Background())
		require.NoError(t, err)
		byState := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			if sm.Scope.Name != "github.com/z5labs/humus/httpclient" {
				continue // otelhttp's instruments, and any other client's
			}
			for _, m := range sm.Metrics {
				if m.Name != "humus.circuit.state" {
					continue
				}
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					v, _ := dp.Attributes.Value("humus.circuit.state")
					byState[v.AsString()] += dp.Value
				}
			}
		}
		require.Equal(t, map[string]int64{"closed": 0, "open": 1, "half-open": 0}, byState)
	})
}
