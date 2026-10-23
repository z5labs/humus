// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
)

type resourceAttr struct {
	Key   string
	Value struct{ Value any }
}

func TestBuildTelemetry_names_the_service_catalog_and_nothing_else(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"by default":           {env: map[string]string{}, want: "catalog"},
		"unless it is renamed": {env: map[string]string{"OTEL_SERVICE_NAME": "catalog-canary"}, want: "catalog-canary"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_LOGS_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none"}
			maps.Copy(env, tc.env)
			var stdout bytes.Buffer
			sdk, err := BuildTelemetry(t.Context(), bedrock.Process{Environment: bedrock.NewStaticEnvironment(env), Stdout: &stdout}, nil)
			require.NoError(t, err)

			_, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "probe")
			span.End()
			require.NoError(t, sdk.Shutdown(context.Background()))

			var exported struct{ Resource []resourceAttr }
			require.NoError(t, json.NewDecoder(&stdout).Decode(&exported))
			require.Len(t, exported.Resource, 1, "%+v", exported.Resource)
			require.Equal(t, "service.name", exported.Resource[0].Key)
			require.Equal(t, tc.want, exported.Resource[0].Value.Value)
		})
	}
}

func TestInit_with_a_bad_telemetry_setting_exits_78(t *testing.T) {
	for name, value := range map[string]string{
		"OTEL_SDK_DISABLED":           "maybe",
		"OTEL_SERVICE_NAME":           "",
		"OTEL_TRACES_EXPORTER":        "zipkin",
		"OTEL_METRICS_EXPORTER":       "prometheus",
		"OTEL_LOGS_EXPORTER":          "file",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "collector",
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				f.env[name] = value
				_, status := httpservertest.Start(t, Init,
					humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(BuildTelemetry), humus.WithFaults(Faults)),
					humustest.WithEnv(f.env),
				)
				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
				require.ErrorContains(t, st.Cause(), "config "+name+":")
			})
		})
	}
}
