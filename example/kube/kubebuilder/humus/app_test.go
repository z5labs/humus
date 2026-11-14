// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

func TestInit_requires_the_controller_and_probes_settings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		for _, name := range []string{
			"CONTROLLER_NAME", "POD_NAMESPACE", "POD_NAME",
			"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT", "KUBERNETES_CA_FILE",
			"PROBES_ADDR", "PROBES_TLS_CERT_FILE",
		} {
			require.ErrorContains(t, st.Cause(), "config "+name+":")
		}
	})
}

func TestInit_rejects_a_controller_name_the_Lease_cannot_take(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{"CONTROLLER_NAME": "Memcached_Controller"}),
		)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorContains(t, st.Cause(), "config CONTROLLER_NAME:")
	})
}
