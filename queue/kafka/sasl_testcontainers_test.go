//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/oauth"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/kafka"
)

// kafkaJVMImage is kafkaImage's version of Apache Kafka on the JVM. The
// GraalVM native image can't build the unsecured OAUTHBEARER callback
// handlers, which it has no reflection metadata for, so a broker with an
// OAUTHBEARER listener runs on the JVM.
const kafkaJVMImage = "docker.io/apache/kafka:4.3.1"

// startOAuthKafka starts a single-node Kafka of its own, whose clients'
// listener requires OAUTHBEARER, validated by Kafka's built-in unsecured
// validator, which accepts an unsigned JWT and is meant for tests. It
// returns the listener's address. The container is stopped when the test
// ends.
func startOAuthKafka(t *testing.T) string {
	t.Helper()
	port, err := freePort()
	require.NoError(t, err)
	addr := net.JoinHostPort("127.0.0.1", port)
	c, err := testcontainers.GenericContainer(t.Context(), testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        kafkaJVMImage,
			ExposedPorts: []string{"9094/tcp"},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.PortBindings = network.PortMap{
					network.MustParsePort("9094/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: port}},
				}
			},
			Env: map[string]string{
				"KAFKA_NODE_ID":                        "1",
				"KAFKA_PROCESS_ROLES":                  "broker,controller",
				"KAFKA_CONTROLLER_QUORUM_VOTERS":       "1@localhost:9093",
				"KAFKA_CONTROLLER_LISTENER_NAMES":      "CONTROLLER",
				"KAFKA_LISTENERS":                      "PLAINTEXT://0.0.0.0:9092,CONTROLLER://0.0.0.0:9093,OAUTH://0.0.0.0:9094",
				"KAFKA_ADVERTISED_LISTENERS":           "PLAINTEXT://localhost:9092,OAUTH://" + addr,
				"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP": "PLAINTEXT:PLAINTEXT,CONTROLLER:PLAINTEXT,OAUTH:SASL_PLAINTEXT",
				"KAFKA_INTER_BROKER_LISTENER_NAME":     "PLAINTEXT",

				// With no sasl.server.callback.handler.class, the broker
				// validates OAUTHBEARER tokens with the unsecured validator.
				"KAFKA_LISTENER_NAME_OAUTH_SASL_ENABLED_MECHANISMS":      "OAUTHBEARER",
				"KAFKA_LISTENER_NAME_OAUTH_OAUTHBEARER_SASL_JAAS_CONFIG": "org.apache.kafka.common.security.oauthbearer.OAuthBearerLoginModule required;",

				"KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR":         "1",
				"KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR": "1",
				"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR":            "1",
				"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS":         "0",
				"KAFKA_AUTO_CREATE_TOPICS_ENABLE":                "false",
			},
			WaitingFor: wait.ForLog("Kafka Server started").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, c)
	require.NoError(t, err, "starting Kafka in a container")
	return addr
}

// unsecuredJWT returns an unsigned JWT for subject that expires in an hour,
// which Kafka's unsecured OAUTHBEARER validator accepts.
func unsecuredJWT(subject string) string {
	part := func(v map[string]any) string {
		b, _ := json.Marshal(v) // a map of strings and numbers always marshals
		return base64.RawURLEncoding.EncodeToString(b)
	}
	now := time.Now()
	return part(map[string]any{"alg": "none"}) + "." +
		part(map[string]any{"sub": subject, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}) + "."
}

// countingSASL is a SASLMechanism that counts the exchanges it starts, one
// for each connection.
type countingSASL struct {
	kafka.SASLMechanism
	exchanges *atomic.Int64
}

func (c countingSASL) Authenticate(ctx context.Context, addr string) (kafka.SASLSession, []byte, error) {
	c.exchanges.Add(1)
	return c.SASLMechanism.Authenticate(ctx, addr)
}

func TestKafka_OAuthBearer_authenticates_a_Service_and_a_Producer_with_a_token_for_each_connection(t *testing.T) {
	// kfake has no OAUTHBEARER, so Kafka itself validates the token here.
	addr := startOAuthKafka(t)

	var (
		tokens    atomic.Int64
		exchanges atomic.Int64
	)
	mechanism := countingSASL{
		SASLMechanism: kafka.OAuthBearer(func(context.Context) (kafka.OAuthToken, error) {
			tokens.Add(1)
			return kafka.OAuthToken{Token: unsecuredJWT("orders")}, nil
		}),
		exchanges: &exchanges,
	}

	// The test's own admin client, which creates the topic, authenticates
	// with a token of its own.
	cl, err := kgo.NewClient(kgo.SeedBrokers(addr), kgo.SASL(oauth.Oauth(func(context.Context) (oauth.Auth, error) {
		return oauth.Auth{Token: unsecuredJWT("admin")}, nil
	})))
	require.NoError(t, err)
	t.Cleanup(cl.Close)
	name := newTopic(t, kadm.NewClient(cl), 1)

	ps := kafka.DefaultProducerSettings()
	ps.Brokers = []string{addr}
	ps.SASL = mechanism
	var produced error
	_, status := humustest.Start(t, produces(ps, func(ctx context.Context, p *kafka.Producer) error {
		produced = p.Produce(ctx,
			kafka.Record{Topic: name, Value: []byte("0")},
			kafka.Record{Topic: name, Value: []byte("1")},
			kafka.Record{Topic: name, Value: []byte("2")},
		)
		return nil
	}), humustest.WithApp(humus.WithHost(humus.OSHost())))
	st := exited(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.NoError(t, produced, "the Producer authenticated")

	s := kafka.DefaultSettings()
	s.Brokers = []string{addr}
	s.GroupID = unique("orders")
	s.Topics = []string{name}
	s.SASL = mechanism
	var (
		mu   sync.Mutex
		seen []string
	)
	p, status := startOnOS(t, s, func(_ context.Context, m kafka.Message) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, string(m.Value))
		return nil
	})
	eventually(t, time.Minute, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 3
	}, "the Service authenticated and processed the three records")
	st = terminate(t, p, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.Equal(t, []string{"0", "1", "2"}, seen)

	require.GreaterOrEqual(t, exchanges.Load(), int64(3), "the Producer and the Service opened more than one connection")
	require.Equal(t, exchanges.Load(), tokens.Load(), "token is called once for each connection")

	// The validator does check the token: one that isn't a JWT is refused.
	ps.SASL = kafka.OAuthBearer(func(context.Context) (kafka.OAuthToken, error) {
		return kafka.OAuthToken{Token: "not-a-jwt"}, nil
	})
	_, status = humustest.Start(t, produces(ps, func(ctx context.Context, p *kafka.Producer) error {
		produced = p.Produce(ctx, kafka.Record{Topic: name, Value: []byte("3")})
		return nil
	}), humustest.WithApp(humus.WithHost(humus.OSHost())))
	st = exited(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	requireCode(t, produced, saslAuthenticationFailed)
	require.ErrorContains(t, produced, `{"status":"invalid_token"}`, "Kafka's unsecured validator refused the token")
}
