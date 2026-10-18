// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/scram"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/kafka"
)

func TestInit_processes_and_commits_a_produced_record(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 1) // key-0, with the value 0

		tel := humustest.NewTelemetry() // keeps the App's logs off p.Stdout()
		p, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"KAFKA_BROKERS":  strings.Join(c.ListenAddrs(), ","),
				"KAFKA_GROUP_ID": "orders-consumer",
				"KAFKA_TOPICS":   "orders",
			}),
		)
		synctest.Wait() // the record is processed and committed
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, "stored order key-0: 0\n", p.Stdout())
		require.Equal(t, int64(1), committed(c)[0], "the record is committed")
		require.Len(t, spansNamed(tel, "process orders"), 1)
	})
}

func TestInit_produces_an_order_it_cannot_read_as_a_dead_letter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1, kfake.SeedTopics(1, dlqTopic))
		produceRecords(t, h, c, []*kgo.Record{
			{Topic: topic, Key: []byte("key-0"), Value: []byte("{")},
			{Topic: topic, Key: []byte("key-1"), Value: []byte("1")},
		})

		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"KAFKA_BROKERS":  strings.Join(c.ListenAddrs(), ","),
				"KAFKA_GROUP_ID": "orders-consumer",
				"KAFKA_TOPICS":   "orders",
			}),
		)
		time.Sleep(time.Second) // in fake time, for the dead letter's Producer to connect
		synctest.Wait()         // both records are processed and committed
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, "stored order key-1: 1\n", p.Stdout(), "the dead letter didn't stop the partition")
		require.Equal(t, int64(2), committed(c)[0], "the dead letter's record is committed too")
		dead := readAll(t, h, c, dlqTopic)
		require.Len(t, dead, 1)
		require.Equal(t, "key-0", string(dead[0].Key))
		require.Equal(t, "{", string(dead[0].Value))
		require.Len(t, spansNamed(tel, "send "+dlqTopic), 1)
	})
}

func TestInit_with_a_bad_delivery_timeout_exits_78_naming_the_variable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init, humustest.WithEnv(map[string]string{
			"KAFKA_BROKERS":          "kafka-0:9092",
			"KAFKA_GROUP_ID":         "orders-consumer",
			"KAFKA_TOPICS":           "orders",
			"KAFKA_DELIVERY_TIMEOUT": "0s",
		}))

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		cerr, ok := errors.AsType[*config.Error](st.Cause())
		require.True(t, ok, "cause = %v, want a *config.Error", st.Cause())
		require.Equal(t, "KAFKA_DELIVERY_TIMEOUT", cerr.Name)
	})
}

func TestInit_without_its_brokers_group_and_topics_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		require.EqualError(t, st.Cause(), "init: config KAFKA_BROKERS: not found\n"+
			"config KAFKA_GROUP_ID: not found\n"+
			"config KAFKA_TOPICS: not found")
	})
}

func TestInit_with_a_bad_broker_exits_78_naming_the_variable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init, humustest.WithEnv(map[string]string{
			"KAFKA_BROKERS":  "kafka-0:9092,kafka-1",
			"KAFKA_GROUP_ID": "orders-consumer",
			"KAFKA_TOPICS":   "orders",
		}))

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		cerr, ok := errors.AsType[*config.Error](st.Cause())
		require.True(t, ok, "cause = %v, want a *config.Error", st.Cause())
		require.Equal(t, "KAFKA_BROKERS", cerr.Name)
		require.EqualError(t, st.Cause(), `init: config KAFKA_BROKERS: "kafka-1" is not host:port`)
	})
}

// pemFiles returns, PEM-encoded, the CA's certificate, and cert's chain
// followed by its private key.
func pemFiles(t *testing.T, cert tls.Certificate) (caPEM, certPEM []byte) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[len(cert.Certificate)-1]})
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
	return caPEM, certPEM
}

func TestSecureInit_consumes_over_mTLS_with_SASL(t *testing.T) {
	user, password := rand.Text(), rand.Text()

	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		server := ca.Issue(t, "127.0.0.1")
		client := ca.Issue(t, "orders-consumer")
		caPEM, certPEM := pemFiles(t, client.TLS())
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"etc/kafka/ca.pem":     caPEM,
			"etc/kafka/client.pem": certPEM,
		}))
		c := cluster(t, h, 1,
			kfake.SeedTopics(1, dlqTopic),
			kfake.TLS(&tls.Config{
				Certificates: []tls.Certificate{server.TLS()},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    ca.Pool(),
				MinVersion:   tls.VersionTLS12,
			}),
			kfake.EnableSASL(),
			kfake.Superuser("SCRAM-SHA-512", user, password),
		)
		producerTLS := &tls.Config{
			Certificates: []tls.Certificate{client.TLS()},
			RootCAs:      ca.Pool(),
			MinVersion:   tls.VersionTLS12,
		}
		secure := []kgo.Opt{
			kgo.Dialer(tlsDial(h, producerTLS)),
			kgo.SASL(scram.Auth{User: user, Pass: password}.AsSha512Mechanism()),
		}
		produce(t, h, c, 1, secure...)
		// An order that isn't JSON, which secureInit's Producer dead-letters
		// over the same TLS and with the same credentials.
		produceRecords(t, h, c, []*kgo.Record{{Topic: topic, Key: []byte("key-1"), Value: []byte("{")}}, secure...)

		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, secureInit,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"KAFKA_BROKERS":       strings.Join(c.ListenAddrs(), ","),
				"KAFKA_GROUP_ID":      "orders-consumer",
				"KAFKA_TOPICS":        "orders",
				"KAFKA_TLS_CA_FILE":   "etc/kafka/ca.pem",
				"KAFKA_TLS_CERT_FILE": "etc/kafka/client.pem",
				"KAFKA_SASL_USER":     user,
				"KAFKA_SASL_PASSWORD": password,
			}),
		)
		time.Sleep(time.Second) // in fake time, for the dead letter's Producer to connect
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, "stored order key-0: 0\n", p.Stdout())
		require.Equal(t, int64(2), committed(c)[0])
		dead := readAll(t, h, c, dlqTopic, secure...)
		require.Len(t, dead, 1)
		require.Equal(t, "key-1", string(dead[0].Key))
	})
}

func TestSecureInit_without_the_client_certificate_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"etc/kafka/ca.pem": []byte("not a certificate"),
		}))
		_, status := humustest.Start(t, secureInit,
			humustest.WithApp(humus.WithHost(h.Host())),
			humustest.WithEnv(map[string]string{
				"KAFKA_BROKERS":       "127.0.0.1:9092",
				"KAFKA_GROUP_ID":      "orders-consumer",
				"KAFKA_TOPICS":        "orders",
				"KAFKA_TLS_CA_FILE":   "etc/kafka/ca.pem",
				"KAFKA_SASL_USER":     "consumer",
				"KAFKA_SASL_PASSWORD": rand.Text(),
			}),
		)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		cerr, ok := errors.AsType[*config.Error](st.Cause())
		require.True(t, ok, "cause = %v, want a *config.Error", st.Cause())
		require.Equal(t, "KAFKA_TLS_CERT_FILE", cerr.Name)
	})
}

func TestTwoDeliveries_are_two_Services_with_two_group_IDs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c, err := kfake.NewCluster(
			kfake.ListenFn(h.Listen),
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "payments", "clicks"),
		)
		require.NoError(t, err)
		t.Cleanup(c.Close)
		produceRecords(t, h, c, []*kgo.Record{
			{Topic: "payments", Value: []byte("p")},
			{Topic: "clicks", Value: []byte("c")},
		})

		var (
			mu   sync.Mutex
			seen = map[string]int{}
		)
		p, status := humustest.Start(t, twoDeliveries(c.ListenAddrs(), func(_ context.Context, m kafka.Message) error {
			mu.Lock()
			defer mu.Unlock()
			seen[m.Topic]++
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, map[string]int{"payments": 1, "clicks": 1}, seen)
		require.Equal(t, int64(1), c.GroupInfo("payments-consumer").Commits["payments"][0].Offset)
		require.Equal(t, int64(1), c.GroupInfo("clicks-consumer").Commits["clicks"][0].Offset)
	})
}
