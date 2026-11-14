// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/kafka/watermill/cases"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const probesAddr = "localhost:8443"

func TestInit_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := start(t, nil)
				c.Run(t, s.target)
				s.stop(t)
			})
		})
	}
}

func TestInit_dead_letters_an_order_whose_invoice_every_attempt_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, map[string]string{"RETRY_ATTEMPTS": "3", "RETRY_DELAY": "5s"})
		s.target.Cluster.Fault(kfake.Fault{
			Keys:  []kmsg.Key{kmsg.Produce},
			Topic: invoicesTopic,
			Err:   kerr.TopicAuthorizationFailed,
			Count: -1,
		})
		began := time.Now()
		s.produce(t, &kgo.Record{
			Topic:   ordersTopic,
			Key:     []byte("order-1"),
			Value:   []byte(`{"id":"order-1","customer":"ada","total":4250}`),
			Headers: []kgo.RecordHeader{{Key: "correlation_id", Value: []byte("corr-1")}},
		})

		dead := s.waitFor(t, deadLetterTopic, 1)
		require.GreaterOrEqual(t, time.Since(began), 10*time.Second, "two retries, each 5s after the attempt before it")
		s.stop(t)

		require.Equal(t, "order-1", string(dead[0].Key))
		require.Equal(t, `{"id":"order-1","customer":"ada","total":4250}`, string(dead[0].Value))
		require.Equal(t, "corr-1", header(dead[0], "correlation_id"))
		require.Equal(t, "3", header(dead[0], "dlq.attempts"))
		require.Contains(t, header(dead[0], "dlq.error"), "TOPIC_AUTHORIZATION_FAILED")

		retried := s.read(t, retryTopic)
		require.Len(t, retried, 2)
		for n, rec := range retried {
			require.Equal(t, "order-1", string(rec.Key))
			require.Equal(t, strconv.Itoa(n+1), header(rec, "retry.attempt"))
			require.Contains(t, header(rec, "retry.error"), "TOPIC_AUTHORIZATION_FAILED")
			require.Equal(t, "corr-1", header(rec, "correlation_id"))
		}
		require.Empty(t, s.read(t, invoicesTopic))
	})
}

func TestInit_dead_letters_a_retried_record_with_no_attempt_at_once(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, nil)
		s.produce(t, &kgo.Record{
			Topic: retryTopic,
			Key:   []byte("order-1"),
			Value: []byte(`{"id":"order-1","customer":"ada","total":4250}`),
		})

		dead := s.waitFor(t, deadLetterTopic, 1)
		s.stop(t)

		require.Equal(t, "the record has no retry.attempt header", header(dead[0], "dlq.error"))
		require.Empty(t, header(dead[0], "dlq.attempts"))
		require.Empty(t, s.read(t, invoicesTopic))
	})
}

func TestInit_turns_ready_once_it_has_joined_its_group(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, nil)
		require.Equal(t, http.StatusOK, s.probe(t, "/health/liveness"))
		for s.probe(t, "/health/readiness") != http.StatusOK {
			time.Sleep(100 * time.Millisecond)
		}
		s.stop(t)
	})
}

func TestInit_requires_the_brokers_and_the_probes_settings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		for _, name := range []string{"KAFKA_BROKERS", "PROBES_ADDR", "PROBES_TLS_CERT_FILE"} {
			require.ErrorContains(t, st.Cause(), "config "+name+": not found")
		}
	})
}

func TestInit_rejects_a_retry_setting_that_is_not_positive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{"RETRY_ATTEMPTS": "0", "RETRY_DELAY": "-1s"}),
		)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorContains(t, st.Cause(), "config RETRY_ATTEMPTS: must be positive")
		require.ErrorContains(t, st.Cause(), "config RETRY_DELAY: must be positive")
	})
}

type service struct {
	target cases.Target
	h      *humustest.Host
	probes *http.Client
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

func start(t *testing.T, env map[string]string) service {
	t.Helper()
	ca := humustest.NewCA(t)
	certPEM, keyPEM := pemPair(t, ca.Issue(t, "localhost"))
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"tls/tls.crt": certPEM,
		"tls/tls.key": keyPEM,
	}))
	cluster, err := kfake.NewCluster(
		kfake.ListenFn(h.Listen),
		kfake.NumBrokers(1),
		kfake.SeedTopics(1, ordersTopic, invoicesTopic, retryTopic, deadLetterTopic),
	)
	require.NoError(t, err)
	t.Cleanup(cluster.Close)

	all := map[string]string{
		"KAFKA_BROKERS":        strings.Join(cluster.ListenAddrs(), ","),
		"RETRY_ATTEMPTS":       "3",
		"RETRY_DELAY":          "1s",
		"PROBES_ADDR":          probesAddr,
		"PROBES_TLS_CERT_FILE": "tls/tls.crt",
		"PROBES_TLS_KEY_FILE":  "tls/tls.key",
	}
	maps.Copy(all, env)
	p, status := humustest.Start(t, Init,
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(all),
	)
	select {
	case <-h.Listening(probesAddr):
	case st := <-status:
		t.Fatalf("the service exited %d: %v", st.Code(), st.Cause())
	}

	probes := h.HTTPClient()
	probes.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	return service{
		target: cases.Target{
			Cluster:         cluster,
			Kafka:           []kgo.Opt{kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.Dialer(h.Dial)},
			Group:           consumerGroup,
			Consumes:        []string{ordersTopic, retryTopic},
			OrdersTopic:     ordersTopic,
			InvoicesTopic:   invoicesTopic,
			DeadLetterTopic: deadLetterTopic,
			Timeout:         time.Minute,
		},
		h:      h,
		probes: probes,
		p:      p,
		status: status,
	}
}

func (s service) stop(t *testing.T) {
	t.Helper()
	<-s.p.Notified(syscall.SIGTERM)
	s.p.Signal(syscall.SIGTERM)
	st := <-s.status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

func (s service) probe(t *testing.T, path string) int {
	t.Helper()
	resp, err := s.probes.Get("https://" + probesAddr + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode
}

func (s service) client(t *testing.T, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	cl, err := kgo.NewClient(append(slices.Clone(s.target.Kafka), opts...)...)
	require.NoError(t, err)
	return cl
}

func (s service) produce(t *testing.T, records ...*kgo.Record) {
	t.Helper()
	cl := s.client(t)
	defer cl.Close()
	require.NoError(t, cl.ProduceSync(context.Background(), records...).FirstErr())
}

func (s service) waitFor(t *testing.T, topic string, n int) []*kgo.Record {
	t.Helper()
	for start := time.Now(); ; time.Sleep(100 * time.Millisecond) {
		if got := s.read(t, topic); len(got) >= n {
			return got
		}
		require.Less(t, time.Since(start), time.Minute, "%s never holds %d records", topic, n)
	}
}

func (s service) read(t *testing.T, topic string) []*kgo.Record {
	t.Helper()
	ends := map[int32]int64{}
	for _, p := range s.target.Cluster.PartitionInfos(topic) {
		if p.HighWatermark > p.LogStartOffset {
			ends[p.Partition] = p.HighWatermark
		}
	}
	if len(ends) == 0 {
		return nil
	}
	cl := s.client(t, kgo.ConsumeTopics(topic))
	defer cl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var out []*kgo.Record
	for len(ends) > 0 {
		fetches := cl.PollFetches(ctx)
		require.NoError(t, ctx.Err(), "reading %s: partitions %v are never read to their ends", topic, ends)
		fetches.EachError(func(_ string, _ int32, err error) {
			require.NoError(t, err, "reading %s", topic)
		})
		fetches.EachRecord(func(r *kgo.Record) {
			out = append(out, r)
			if r.Offset+1 >= ends[r.Partition] {
				delete(ends, r.Partition)
			}
		})
	}
	return out
}

func header(rec *kgo.Record, key string) string {
	for _, h := range rec.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func pemPair(t *testing.T, c *humustest.Certificate) (certPEM, keyPEM []byte) {
	t.Helper()
	cert := c.TLS()
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}
