// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server_test

import (
	"context"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// funcPointer returns the code pointer of f, a func value, so that two
// references to the same function compare equal.
func funcPointer(f any) uintptr {
	return reflect.ValueOf(f).Pointer()
}

// noServices is an Init that returns no Services.
func noServices(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error) { return nil, nil }

func TestDefaults_leaves_the_rest_of_the_App_unchanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := humustest.NewHost(t).Host()
		a := humus.App[humus.Host]{
			Host:      host,
			Telemetry: telemetry.NoopBuilder,
			Init:      noServices,
		}
		require.NoError(t, a.Apply(http3server.Defaults()...))

		require.Equal(t, host, a.Host, "Host")
		require.Equal(t, funcPointer(telemetry.NoopBuilder), funcPointer(a.Telemetry), "Telemetry")
		require.Equal(t, funcPointer(noServices), funcPointer(a.Init), "Init")

		require.Equal(t, 10*time.Second, a.ShutdownGracePeriod)
		require.NotNil(t, a.Drain)
	})

	t.Run("including an App with none of them set", func(t *testing.T) {
		var a humus.App[humus.Host]
		require.NoError(t, a.Apply(http3server.Defaults()...))
		require.Zero(t, a.Host)
		require.Nil(t, a.Telemetry)
		require.Nil(t, a.Init)
	})
}
