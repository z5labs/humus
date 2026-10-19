// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/orderstatus/projector"
	"github.com/z5labs/humus/example/orderstatus/store"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/kafka"
	"github.com/z5labs/humus/telemetry"
)

const (
	eventsTopic     = "orders.events"
	statusTopic     = "orders.status"
	retryTopic      = "orders.retry"
	deadLetterTopic = "orders.dlq"
	unset           = "\x00unset"
)

type run struct {
	code   int
	cause  error
	stdout string
	stderr string
}

type cli struct {
	h      *humustest.Host
	ca     *humustest.CA
	client *tls.Config
	env    map[string]string
}

func newCLI(t *testing.T, files map[string][]byte) *cli {
	t.Helper()
	ca := humustest.NewCA(t)
	client := ca.Issue(t, "orderevents")
	all := map[string][]byte{
		"certs/kafka-ca.pem":       caPEM(t, ca),
		"secrets/kafka-client.pem": pemOf(t, client),
	}
	maps.Copy(all, files)
	h := humustest.NewHost(t, humustest.WithFiles(all))
	return &cli{
		h:  h,
		ca: ca,
		client: &tls.Config{
			Certificates: []tls.Certificate{client.TLS()},
			RootCAs:      ca.Pool(),
			MinVersion:   tls.VersionTLS12,
		},
		env: map[string]string{
			"KAFKA_BROKERS":       "127.0.0.1:9092",
			"KAFKA_EVENTS_TOPIC":  eventsTopic,
			"KAFKA_TLS_CA_FILE":   h.Path("certs/kafka-ca.pem"),
			"KAFKA_TLS_CERT_FILE": h.Path("secrets/kafka-client.pem"),
		},
	}
}

func (c *cli) cluster(t *testing.T) *kfake.Cluster {
	t.Helper()
	k, err := kfake.NewCluster(
		kfake.ListenFn(c.h.Listen),
		kfake.NumBrokers(1),
		kfake.SeedTopics(12, eventsTopic),
		kfake.TLS(&tls.Config{
			Certificates: []tls.Certificate{c.ca.Issue(t, "127.0.0.1").TLS()},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    c.ca.Pool(),
			MinVersion:   tls.VersionTLS12,
		}),
	)
	require.NoError(t, err)
	t.Cleanup(k.Close)
	c.env["KAFKA_BROKERS"] = strings.Join(k.ListenAddrs(), ",")
	return k
}

func (c *cli) start(t *testing.T, env map[string]string, build telemetry.Builder, args ...string) run {
	t.Helper()
	merged := maps.Clone(c.env)
	for name, value := range env {
		if value == unset {
			delete(merged, name)
			continue
		}
		merged[name] = value
	}
	p, status := humustest.Start(t, Init,
		humustest.WithApp(humus.WithHost(c.h.Host()), humus.WithTelemetry(build)),
		humustest.WithEnv(merged),
		humustest.WithArgs(args...),
	)
	st := <-status
	return run{code: st.Code(), cause: st.Cause(), stdout: p.Stdout(), stderr: p.Stderr()}
}

func (c *cli) dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := c.h.Dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	cfg := c.client.Clone()
	cfg.ServerName, _, _ = net.SplitHostPort(address)
	tc := tls.Client(conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tc, nil
}

func (c *cli) readAll(t *testing.T, k *kfake.Cluster, topic string) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(k.ListenAddrs()...),
		kgo.Dialer(c.dial),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	require.NoError(t, err)
	defer cl.Close()
	var out []*kgo.Record
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		fetches := cl.PollFetches(ctx)
		cancel()
		fetches.EachError(func(_ string, _ int32, err error) {
			if !errors.Is(err, context.DeadlineExceeded) {
				require.NoError(t, err, "reading %s", topic)
			}
		})
		n := 0
		fetches.EachRecord(func(r *kgo.Record) {
			out = append(out, r)
			n++
		})
		if n == 0 {
			return out
		}
	}
}

func pemOf(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var out []byte
	for _, der := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}

func caPEM(t *testing.T, ca *humustest.CA) []byte {
	t.Helper()
	c := ca.Issue(t, "unused").TLS()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[len(c.Certificate)-1]})
}

type memory struct {
	mu       sync.Mutex
	events   []store.Event
	statuses map[string]store.Status
}

func (m *memory) Applied(_ context.Context, partition int32, offset int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.Partition == partition && e.Offset == offset {
			return true, nil
		}
	}
	return false, nil
}

func (m *memory) Events(_ context.Context, orderID string) ([]store.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var events []store.Event
	for _, e := range m.events {
		if e.OrderID == orderID {
			events = append(events, e)
		}
	}
	return events, nil
}

func (m *memory) Apply(_ context.Context, e store.Event, status store.Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	m.statuses[e.OrderID] = status
	return nil
}

type produced struct {
	mu      sync.Mutex
	records []kafka.Record
}

func (p *produced) Produce(_ context.Context, records ...kafka.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = append(p.records, records...)
	return nil
}

func (p *produced) topics() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	topics := make([]string, len(p.records))
	for i, r := range p.records {
		topics[i] = r.Topic
	}
	return topics
}

func project(t *testing.T, records []*kgo.Record) (map[string]store.Status, []string) {
	t.Helper()
	m := &memory{statuses: map[string]store.Status{}}
	out := &produced{}
	p, err := projector.New(m, out, out, out, projector.Settings{
		EventsTopic:     eventsTopic,
		StatusTopic:     statusTopic,
		RetryTopic:      retryTopic,
		DeadLetterTopic: deadLetterTopic,
		Attempts:        1,
		MinDelay:        time.Second,
		MaxDelay:        time.Second,
	}, noop.NewMeterProvider())
	require.NoError(t, err)
	for _, r := range records {
		require.NoError(t, p.Process(context.Background(), kafka.Message{
			Topic:     r.Topic,
			Partition: r.Partition,
			Offset:    r.Offset,
			Key:       r.Key,
			Value:     r.Value,
			Timestamp: r.Timestamp,
		}))
	}
	return m.statuses, out.topics()
}

func TestInit_produces_the_order_s_events_keyed_by_its_ID_in_order(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCLI(t, nil)
		k := c.cluster(t)

		types := []string{projector.Placed, projector.Paid, projector.Shipped, projector.Delivered}
		got := c.start(t, nil, telemetry.NoopBuilder, append([]string{"-order-id", "order-1"}, types...)...)
		require.Zero(t, got.code, "cause: %v", got.cause)
		require.Equal(t, "order-1\n", got.stdout)

		records := c.readAll(t, k, eventsTopic)
		require.Len(t, records, len(types))
		var last time.Time
		for i, r := range records {
			require.Equal(t, "order-1", string(r.Key))
			require.Equal(t, records[0].Partition, r.Partition)
			require.Equal(t, records[0].Offset+int64(i), r.Offset)

			var e map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(r.Value, &e))
			require.ElementsMatch(t, []string{"type", "at"}, slices.Collect(maps.Keys(e)))
			var typ string
			require.NoError(t, json.Unmarshal(e["type"], &typ))
			require.Equal(t, types[i], typ)
			var at time.Time
			require.NoError(t, json.Unmarshal(e["at"], &at))
			require.False(t, at.IsZero())
			require.False(t, at.Before(last), "event %d's at %s is before %s", i, at, last)
			last = at
		}

		statuses, topics := project(t, records)
		require.Equal(t, projector.Delivered, statuses["order-1"].Status)
		require.Equal(t, []string{statusTopic, statusTopic, statusTopic, statusTopic}, topics)
	})
}

func TestInit_without_an_order_ID_makes_a_random_one_and_prints_it(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCLI(t, nil)
		k := c.cluster(t)

		first := c.start(t, nil, telemetry.NoopBuilder, projector.Placed)
		require.Zero(t, first.code, "cause: %v", first.cause)
		second := c.start(t, nil, telemetry.NoopBuilder, projector.Placed)
		require.Zero(t, second.code, "cause: %v", second.cause)

		firstID := strings.TrimSuffix(first.stdout, "\n")
		secondID := strings.TrimSuffix(second.stdout, "\n")
		require.NotEmpty(t, firstID)
		require.NotEqual(t, firstID, secondID)

		records := c.readAll(t, k, eventsTopic)
		keys := make([]string, len(records))
		for i, r := range records {
			keys[i] = string(r.Key)
		}
		require.ElementsMatch(t, []string{firstID, secondID}, keys)

		statuses, topics := project(t, records)
		require.Equal(t, projector.Placed, statuses[firstID].Status)
		require.Equal(t, projector.Placed, statuses[secondID].Status)
		require.Equal(t, []string{statusTopic, statusTopic}, topics)
	})
}

func TestInit_exits_ExitConfig_on_a_bad_setting(t *testing.T) {
	testCases := map[string]struct {
		files map[string][]byte
		env   map[string]string
		args  []string
		want  []string
	}{
		"no settings at all": {
			env: map[string]string{"KAFKA_BROKERS": unset, "KAFKA_EVENTS_TOPIC": unset, "KAFKA_TLS_CA_FILE": unset, "KAFKA_TLS_CERT_FILE": unset},
			want: []string{
				"config types: not found",
				"config KAFKA_BROKERS: not found",
				"config KAFKA_EVENTS_TOPIC: not found",
				"config KAFKA_TLS_CA_FILE: not found",
				"config KAFKA_TLS_CERT_FILE: not found",
			},
		},
		"an empty order ID and an empty type": {
			args: []string{"-order-id", "", projector.Placed, ""},
			want: []string{"config -order-id: must not be empty", "config types: must not name an empty type"},
		},
		"an unknown flag": {
			args: []string{"-order", "order-1", projector.Placed},
			want: []string{"config arguments: flag provided but not defined: -order"},
		},
		"a bad broker address": {
			env:  map[string]string{"KAFKA_BROKERS": "kafka.internal"},
			args: []string{projector.Placed},
			want: []string{"config KAFKA_BROKERS:"},
		},
		"an empty events topic": {
			env:  map[string]string{"KAFKA_EVENTS_TOPIC": ""},
			args: []string{projector.Placed},
			want: []string{"config KAFKA_EVENTS_TOPIC: must not be empty"},
		},
		"a CA file with no certificate": {
			files: map[string][]byte{"certs/kafka-ca.pem": []byte("not a certificate")},
			args:  []string{projector.Placed},
			want:  []string{"config KAFKA_TLS_CA_FILE: no PEM certificate in the file"},
		},
		"a client certificate file with no key": {
			files: map[string][]byte{"secrets/kafka-client.pem": []byte("not a certificate")},
			args:  []string{projector.Placed},
			want:  []string{"config KAFKA_TLS_CERT_FILE:"},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var got run
			synctest.Test(t, func(t *testing.T) {
				got = newCLI(t, tc.files).start(t, tc.env, telemetry.NoopBuilder, tc.args...)
			})
			require.Equal(t, humus.ExitConfig, got.code, "cause: %v", got.cause)
			require.Empty(t, got.stdout)
			for _, want := range tc.want {
				require.ErrorContains(t, got.cause, want)
			}
		})
	}
}

func TestBuildTelemetry_writes_the_logs_to_stderr_and_leaves_stdout_to_the_order_ID(t *testing.T) {
	var got run
	synctest.Test(t, func(t *testing.T) {
		got = newCLI(t, nil).start(t, map[string]string{"KAFKA_BROKERS": unset}, buildTelemetry, projector.Placed)
	})
	require.Equal(t, humus.ExitConfig, got.code, "cause: %v", got.cause)
	require.Empty(t, got.stdout)
	require.Contains(t, got.stderr, `"severity_text":"ERROR"`)
	require.Contains(t, got.stderr, "config KAFKA_BROKERS: not found")
}
