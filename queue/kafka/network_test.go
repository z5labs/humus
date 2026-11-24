// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

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
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
	"github.com/z5labs/humus/telemetry"
)

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

// recordDials returns a network on h whose DialTCP records every address
// it dials, and the record.
func recordDials(h *humustest.Host) (kafka.Network, *dialRecord) {
	d := &dialRecord{}
	return hosttest.Seams{Network: h, OnDialTCP: func(ctx context.Context, network, address string) (net.Conn, error) {
		d.mu.Lock()
		d.addrs = append(d.addrs, address)
		d.mu.Unlock()
		return h.DialTCP(ctx, network, address)
	}}, d
}

func TestNewProducer_dials_only_through_its_Network(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		n, d := recordDials(h)
		// The App's Host, which is Init's argument, is another host whose
		// every dial fails, so a dial through it would fail the Produce
		// rather than reach the cluster.
		appHost := humus.Host{
			IPNetwork: hosttest.Seams{Network: humustest.NewHost(t), OnDialTCP: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("dialled through the App's Host")
			}},
			Filesystem: h.Filesystem(),
		}

		var dialsAtNew []string
		var produceErr error
		init := func(_ context.Context, s *humus.Setup, _ humus.Host) ([]humus.Service, error) {
			p, err := kafka.NewProducer(s, n, producerSettings(c.ListenAddrs()...))
			if err != nil {
				return nil, err
			}
			dialsAtNew = d.all()
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				produceErr = p.Produce(ctx, value(0))
				return nil
			})}, nil
		}
		_, status := humustest.Start(t, appHost, init, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.Empty(t, dialsAtNew, "NewProducer dials nothing")
		require.NoError(t, produceErr)
		require.NotEmpty(t, d.all(), "the Producer dials through n")
		for _, addr := range d.all() {
			require.Contains(t, c.ListenAddrs(), addr)
		}
		require.Len(t, readAll(t, h, c, topic), 1)
	})
}

func TestNewService_dials_only_through_its_Network(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 1)
		n, d := recordDials(h)

		var processed []string
		svc, err := kafka.NewService(&humus.Setup{Telemetry: telemetry.Noop()}, n, settings(c.ListenAddrs()...),
			queue.ProcessorFunc[kafka.Message](func(_ context.Context, m kafka.Message) error {
				processed = append(processed, string(m.Value))
				return nil
			}))
		require.NoError(t, err)
		require.Empty(t, d.all(), "NewService dials nothing")

		ctx, cancel := context.WithCancel(t.Context())
		served := make(chan error, 1)
		go func() { served <- svc.Serve(ctx) }()
		synctest.Wait()
		cancel()
		require.NoError(t, <-served)

		require.Equal(t, []string{"0"}, processed)
		require.NotEmpty(t, d.all(), "Serve dials through n")
		for _, addr := range d.all() {
			require.Contains(t, c.ListenAddrs(), addr)
		}
	})
}
