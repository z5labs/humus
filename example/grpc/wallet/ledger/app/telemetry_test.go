// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bytes"
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
)

func TestBuildTelemetry_names_the_service_ledger(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"by default":           {env: map[string]string{}, want: "ledger"},
		"unless it is renamed": {env: map[string]string{"OTEL_SERVICE_NAME": "ledger-canary"}, want: "ledger-canary"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_LOGS_EXPORTER": "none"}
			maps.Copy(env, tc.env)
			var stdout bytes.Buffer
			sdk, err := BuildTelemetry(t.Context(), bedrock.Process{Environment: bedrock.NewStaticEnvironment(env), Stdout: &stdout}, nil)
			require.NoError(t, err)

			_, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "probe")
			span.End()
			require.NoError(t, sdk.Shutdown(context.Background()))

			require.Contains(t, stdout.String(), `"Key":"service.name","Value":{"Type":"STRING","Value":"`+tc.want+`"}`)
		})
	}
}
