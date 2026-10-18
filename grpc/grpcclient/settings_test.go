// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"crypto/tls"
	"net"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/humustest"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
// Settings.TLS is checked by tlsField, since "TLS 1.2" names a version.
var fieldNames = []string{
	"Settings", "Addr", "Timeout",
	"addr", "timeout",
}

// tlsField matches "TLS" where it is not the start of a version, such as
// "TLS 1.2".
var tlsField = regexp.MustCompile(`TLS(?:[^ ]|$)|TLS [^1]`)

func TestChecks(t *testing.T) {
	t.Parallel()

	addr := func(a string) func() error {
		return func() error { return grpcclient.CheckAddr(a) }
	}
	tlsConfig := func(c *tls.Config) func() error {
		return func() error { return grpcclient.CheckTLS(c) }
	}
	timeout := func(d time.Duration) func() error {
		return func() error { return grpcclient.CheckTimeout(d) }
	}
	roots := humustest.NewCA(t).Pool()

	for _, c := range []struct {
		name   string
		accept []func() error
		reject []func() error
	}{
		{
			name: "CheckAddr",
			accept: []func() error{
				addr("carts:9090"),
				addr("carts.shop.svc.cluster.local:443"),
				addr("10.0.0.1:9090"),
				addr("[::1]:9090"),
				addr("carts:1"),
				addr("carts:65535"),
			},
			reject: []func() error{
				addr(""),
				addr(":9090"),
				addr("carts"),
				addr("carts:"),
				addr("carts:0"),
				addr("carts:65536"),
				addr("carts:-1"),
				addr("carts:grpc"),
				addr("dns:///carts:9090"),
				addr("passthrough:///carts:9090"),
				addr("unix:///run/carts.sock"),
				addr("carts:9090/v1"),
				addr("::1:9090"),
			},
		},
		{
			name: "CheckTLS",
			accept: []func() error{
				tlsConfig(nil),
				tlsConfig(&tls.Config{}),
				tlsConfig(&tls.Config{RootCAs: roots}),
				tlsConfig(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS12}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS13}),
			},
			reject: []func() error{
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS11}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS10}),
			},
		},
		{
			name: "CheckTimeout",
			accept: []func() error{
				timeout(time.Nanosecond),
				timeout(10 * time.Second),
				timeout(time.Hour),
			},
			reject: []func() error{
				timeout(0),
				timeout(-time.Nanosecond),
				timeout(-time.Second),
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for i, accept := range c.accept {
				require.NoError(t, accept(), "in-range value %d", i)
			}
			for i, reject := range c.reject {
				err := reject()
				require.Error(t, err, "out-of-range value %d", i)
				for _, name := range fieldNames {
					require.NotContains(t, err.Error(), name, "the error of out-of-range value %d names a field", i)
				}
				require.NotRegexp(t, tlsField, err.Error(), "the error of out-of-range value %d names a field", i)
			}
		})
	}
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	d := grpcclient.DefaultSettings()
	require.Equal(t, grpcclient.Settings{
		Addr:    "",
		TLS:     nil,
		Timeout: 10 * time.Second,
	}, d)

	t.Run("passes the checks of the fields that have a default", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, grpcclient.CheckTLS(d.TLS))
		require.NoError(t, grpcclient.CheckTimeout(d.Timeout))
	})

	t.Run("fails the check of Addr, which has none", func(t *testing.T) {
		t.Parallel()
		require.Error(t, grpcclient.CheckAddr(d.Addr))
	})
}

// newSetup returns a Setup on a new humustest Host, for a test that builds
// a Client and makes no RPC.
func newSetup(t *testing.T) *humus.Setup {
	t.Helper()
	return &humus.Setup{Host: humustest.NewHost(t).Host()}
}

func TestNewClient_the_Setup(t *testing.T) {
	t.Parallel()

	settings := grpcclient.DefaultSettings()
	settings.Addr = "carts:9090"

	t.Run("a nil Setup is rejected", func(t *testing.T) {
		t.Parallel()
		c, err := grpcclient.NewClient(nil, settings)
		require.Nil(t, c)
		require.EqualError(t, err, "grpcclient: NewClient: the Setup must not be nil")
	})

	t.Run("a Host with no Dial is rejected, rather than dialling the real network", func(t *testing.T) {
		t.Parallel()
		c, err := grpcclient.NewClient(&humus.Setup{}, settings)
		require.Nil(t, c)
		require.EqualError(t, err, "grpcclient: NewClient: the Setup's Host.Dial must not be nil")
	})

	t.Run("alongside the bad fields", func(t *testing.T) {
		t.Parallel()
		_, err := grpcclient.NewClient(&humus.Setup{}, grpcclient.DefaultSettings())
		require.ErrorContains(t, err, "the Setup's Host.Dial must not be nil")
		require.ErrorContains(t, err, `Settings.Addr "" is not host:port`)
	})
}

func TestNewClient_settings(t *testing.T) {
	t.Parallel()

	t.Run("DefaultSettings alone fails, naming Addr", func(t *testing.T) {
		t.Parallel()
		c, err := grpcclient.NewClient(newSetup(t), grpcclient.DefaultSettings())
		require.Nil(t, c)
		require.EqualError(t, err, `grpcclient: NewClient: Settings.Addr "" is not host:port`)
	})

	t.Run("every bad field is named in one error", func(t *testing.T) {
		t.Parallel()
		settings := grpcclient.DefaultSettings()
		settings.Addr = "carts:9090"
		settings.TLS = &tls.Config{MaxVersion: tls.VersionTLS11}
		settings.Timeout = 0

		c, err := grpcclient.NewClient(newSetup(t), settings)
		require.Nil(t, c)
		require.ErrorContains(t, err, "grpcclient: NewClient: ")
		require.ErrorContains(t, err, "Settings.TLS has a MaxVersion of TLS 1.1")
		require.ErrorContains(t, err, "Settings.Timeout must be positive")
		require.NotContains(t, err.Error(), "Settings.Addr")
	})

	t.Run("each field is named", func(t *testing.T) {
		t.Parallel()
		for field, change := range map[string]func(*grpcclient.Settings){
			"Settings.Addr":    func(s *grpcclient.Settings) { s.Addr = "dns:///carts:9090" },
			"Settings.TLS":     func(s *grpcclient.Settings) { s.TLS = &tls.Config{MaxVersion: tls.VersionTLS10} },
			"Settings.Timeout": func(s *grpcclient.Settings) { s.Timeout = -time.Second },
		} {
			settings := grpcclient.DefaultSettings()
			settings.Addr = "carts:9090"
			change(&settings)
			_, err := grpcclient.NewClient(newSetup(t), settings)
			require.ErrorContains(t, err, field)
		}
	})

	t.Run("DefaultSettings with an Addr passes, and dials nothing", func(t *testing.T) {
		t.Parallel()
		dialled := false
		s := &humus.Setup{Host: humus.Host{
			Dial: func(context.Context, string, string) (net.Conn, error) {
				dialled = true
				return nil, net.ErrClosed
			},
		}}
		settings := grpcclient.DefaultSettings()
		settings.Addr = "carts:9090"
		c, err := grpcclient.NewClient(s, settings)
		require.NoError(t, err)
		require.NotNil(t, c)
		require.False(t, dialled, "NewClient dialled")
	})

	t.Run("an option grpc-go rejects fails NewClient", func(t *testing.T) {
		t.Parallel()
		settings := grpcclient.DefaultSettings()
		settings.Addr = "carts:9090"
		c, err := grpcclient.NewClient(newSetup(t), settings, grpc.WithDefaultServiceConfig("not JSON"))
		require.Nil(t, c)
		require.ErrorContains(t, err, "grpcclient: NewClient: ")
	})
}
