// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

// errNoBroker is the error of every dial through the network refusing
// returns.
var errNoBroker = errors.New("no broker behind n")

// dialRecord records the address of every dial through a network.
type dialRecord struct {
	mu    sync.Mutex
	addrs []string
}

func (d *dialRecord) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// refusing returns a network on h whose DialTCP records every address it
// dials and fails with errNoBroker, and the record.
func refusing(h *humustest.Host) (rabbitmq.Network, *dialRecord) {
	d := &dialRecord{}
	return hosttest.Seams{Network: h, OnDialTCP: func(_ context.Context, _, address string) (net.Conn, error) {
		d.mu.Lock()
		d.addrs = append(d.addrs, address)
		d.mu.Unlock()
		return nil, errNoBroker
	}}, d
}

// appHostFailing returns a Host for the App, other than n, whose every
// dial fails with an error of its own, so a dial through it would fail
// with that error rather than errNoBroker.
func appHostFailing(t *testing.T) humus.Host {
	h := humustest.NewHost(t)
	return humus.Host{
		IPNetwork: hosttest.Seams{Network: h, OnDialTCP: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("dialled through the App's Host")
		}},
		Filesystem: h.Filesystem(),
	}
}

func TestNewPublisher_dials_only_through_its_Network(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n, d := refusing(humustest.NewHost(t))

		var dialsAtNew []string
		var publishErr error
		init := func(_ context.Context, s *humus.Setup, _ humus.Host) ([]humus.Service, error) {
			settings := publisherSettings()
			settings.MaxAttempts = 1
			pub, err := rabbitmq.NewPublisher(s, n, settings)
			if err != nil {
				return nil, err
			}
			dialsAtNew = d.all()
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				publishErr = pub.Publish(ctx, rabbitmq.Publishing{RoutingKey: "orders", Body: []byte("order")})
				return nil
			})}, nil
		}
		_, status := humustest.Start(t, appHostFailing(t), init, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.Empty(t, dialsAtNew, "NewPublisher dials nothing")
		require.ErrorIs(t, publishErr, errNoBroker, "the Publisher dials through n")
		require.Equal(t, []string{"rabbit:5672"}, d.all())
	})
}

func TestNewService_dials_only_through_its_Network(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n, d := refusing(humustest.NewHost(t))
		svc, err := rabbitmq.NewService(&humus.Setup{Telemetry: telemetry.Noop()}, n, settings(), noop)
		require.NoError(t, err)
		require.Empty(t, d.all(), "NewService dials nothing")

		served := make(chan error, 1)
		go func() { served <- svc.Serve(t.Context()) }()
		synctest.Wait()

		require.Equal(t, []string{"rabbit:5672"}, d.all(), "Serve dials through n")
		require.NoError(t, svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}
