// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/orderstatus/projector"
	"github.com/z5labs/humus/example/orderstatus/store"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

const (
	eventsTopic     = "orders.events"
	statusTopic     = "orders.status"
	retryTopic      = "orders.retry"
	deadLetterTopic = "orders.dlq"
	group           = "orderstatus"
	probesAddr      = "localhost:8443"
	brokerAddr      = "127.0.0.1:9092"
	postgresURL     = "postgres://orderstatus@postgres.internal:5432/orders"
	unset           = "\x00unset"
)

type memory struct {
	mu       sync.Mutex
	events   []store.Event
	statuses map[string]store.Status
}

func newMemory() *memory {
	return &memory{statuses: map[string]store.Status{}}
}

func (m *memory) open(context.Context, *humus.Setup, *pgxpool.Config) (projector.Store, error) {
	return m, nil
}

func (m *memory) Applied(_ context.Context, partition int32, offset int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applied(partition, offset), nil
}

func (m *memory) applied(partition int32, offset int64) bool {
	for _, e := range m.events {
		if e.Partition == partition && e.Offset == offset {
			return true
		}
	}
	return false
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
	if m.applied(e.Partition, e.Offset) {
		return store.ErrApplied
	}
	m.events = append(m.events, e)
	m.statuses[e.OrderID] = status
	return nil
}

func (m *memory) status(orderID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statuses[orderID].Status
}

type processor struct {
	h      *humustest.Host
	ca     *humustest.CA
	client *tls.Config
	env    map[string]string
	probes *http.Client
	store  *memory
}

func newProcessor(t *testing.T, files map[string][]byte) *processor {
	t.Helper()
	ca := humustest.NewCA(t)
	client := ca.Issue(t, group)
	all := map[string][]byte{
		"certs/kafka-ca.pem":        caPEM(t, ca),
		"secrets/kafka-client.pem":  pemOf(t, client),
		"secrets/postgres-password": []byte(rand.Text() + "\n"),
		"certs/postgres-ca.pem":     caPEM(t, ca),
		"secrets/probes.pem":        pemOf(t, ca.Issue(t, "localhost")),
	}
	maps.Copy(all, files)
	h := humustest.NewHost(t, humustest.WithFiles(all))

	probes := h.HTTPClient()
	probes.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	return &processor{
		h:  h,
		ca: ca,
		client: &tls.Config{
			Certificates: []tls.Certificate{client.TLS()},
			RootCAs:      ca.Pool(),
			MinVersion:   tls.VersionTLS12,
		},
		env: map[string]string{
			"KAFKA_BROKERS":           brokerAddr,
			"KAFKA_GROUP_ID":          group,
			"KAFKA_EVENTS_TOPIC":      eventsTopic,
			"KAFKA_STATUS_TOPIC":      statusTopic,
			"KAFKA_RETRY_TOPIC":       retryTopic,
			"KAFKA_DEAD_LETTER_TOPIC": deadLetterTopic,
			"KAFKA_TLS_CA_FILE":       h.Path("certs/kafka-ca.pem"),
			"KAFKA_TLS_CERT_FILE":     h.Path("secrets/kafka-client.pem"),
			"POSTGRES_URL":            postgresURL,
			"POSTGRES_PASSWORD_FILE":  h.Path("secrets/postgres-password"),
			"POSTGRES_TLS_CA_FILE":    h.Path("certs/postgres-ca.pem"),
			"PROBES_ADDR":             probesAddr,
			"PROBES_TLS_PEM_FILE":     h.Path("secrets/probes.pem"),
		},
		probes: probes,
		store:  newMemory(),
	}
}

func (p *processor) cluster(t *testing.T, opts ...kfake.Opt) *kfake.Cluster {
	t.Helper()
	c, err := kfake.NewCluster(append([]kfake.Opt{
		kfake.ListenFn(p.h.Listen),
		kfake.NumBrokers(1),
		kfake.SeedTopics(1, eventsTopic, statusTopic, retryTopic, deadLetterTopic),
		kfake.TLS(&tls.Config{
			Certificates: []tls.Certificate{p.ca.Issue(t, "127.0.0.1").TLS()},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    p.ca.Pool(),
			MinVersion:   tls.VersionTLS12,
		}),
	}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	p.env["KAFKA_BROKERS"] = strings.Join(c.ListenAddrs(), ",")
	return c
}

func (p *processor) start(t *testing.T, env map[string]string, opts ...humus.Option) (signal func(os.Signal), status <-chan bedrock.ExitStatus) {
	t.Helper()
	merged := maps.Clone(p.env)
	for name, value := range env {
		if value == unset {
			delete(merged, name)
			continue
		}
		merged[name] = value
	}
	init := func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
		return initWith(ctx, s, p.store.open)
	}
	proc, st := humustest.Start(t, init,
		humustest.WithApp(append([]humus.Option{humus.WithHost(p.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)}, opts...)...),
		humustest.WithEnv(merged),
	)
	return proc.Signal, st
}

func (p *processor) probe(t *testing.T, path string) int {
	t.Helper()
	resp, err := p.probes.Get("https://" + probesAddr + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode
}

func (p *processor) dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := p.h.Dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	cfg := p.client.Clone()
	cfg.ServerName, _, _ = net.SplitHostPort(address)
	tc := tls.Client(conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tc, nil
}

func (p *processor) kafkaClient(t *testing.T, c *kfake.Cluster, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	cl, err := kgo.NewClient(append([]kgo.Opt{
		kgo.SeedBrokers(c.ListenAddrs()...),
		kgo.Dialer(p.dial),
	}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(cl.Close)
	return cl
}

func (p *processor) produce(t *testing.T, c *kfake.Cluster, records ...*kgo.Record) {
	t.Helper()
	cl := p.kafkaClient(t, c)
	require.NoError(t, cl.ProduceSync(context.Background(), records...).FirstErr())
	cl.Close()
}

func (p *processor) readAll(t *testing.T, c *kfake.Cluster, topic string) []*kgo.Record {
	t.Helper()
	cl := p.kafkaClient(t, c, kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
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

func event(orderID, typ string, at time.Time) *kgo.Record {
	value, err := json.Marshal(map[string]any{"type": typ, "at": at})
	if err != nil {
		panic(err)
	}
	return &kgo.Record{Topic: eventsTopic, Key: []byte(orderID), Value: value}
}

func committed(c *kfake.Cluster, topic string) int64 {
	info := c.GroupInfo(group)
	if info == nil {
		return -1
	}
	commit, ok := info.Commits[topic][0]
	if !ok {
		return -1
	}
	return commit.Offset
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for start := time.Now(); !cond(); time.Sleep(10 * time.Millisecond) {
		require.Less(t, time.Since(start), 30*time.Minute, "%s never happened", what)
	}
}

func refuseProduce(c *kfake.Cluster, refusing *atomic.Bool) {
	c.ControlKey(kmsg.Produce.Int16(), func(req kmsg.Request) (kmsg.Response, error, bool) {
		c.KeepControl()
		if !refusing.Load() {
			return nil, nil, false
		}
		r := req.(*kmsg.ProduceRequest)
		resp := r.ResponseKind().(*kmsg.ProduceResponse)
		for _, rt := range r.Topics {
			st := kmsg.NewProduceResponseTopic()
			st.Topic = rt.Topic
			st.TopicID = rt.TopicID
			for _, rp := range rt.Partitions {
				sp := kmsg.NewProduceResponseTopicPartition()
				sp.Partition = rp.Partition
				sp.ErrorCode = kerr.TopicAuthorizationFailed.Code
				st.Partitions = append(st.Partitions, sp)
			}
			resp.Topics = append(resp.Topics, st)
		}
		return resp, nil, true
	})
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

func exitsConfig(t *testing.T, env map[string]string, files map[string][]byte) error {
	t.Helper()
	var cause error
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, files)
		_, status := p.start(t, env)
		st := <-status
		cause = st.Cause()
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", cause)
	})
	require.Error(t, cause)
	return cause
}

func TestInit_without_its_settings_exits_78(t *testing.T) {
	required := []string{
		"KAFKA_BROKERS",
		"KAFKA_GROUP_ID",
		"KAFKA_EVENTS_TOPIC",
		"KAFKA_STATUS_TOPIC",
		"KAFKA_RETRY_TOPIC",
		"KAFKA_DEAD_LETTER_TOPIC",
		"KAFKA_TLS_CA_FILE",
		"KAFKA_TLS_CERT_FILE",
		"POSTGRES_URL",
		"POSTGRES_PASSWORD_FILE",
		"POSTGRES_TLS_CA_FILE",
		"PROBES_ADDR",
		"PROBES_TLS_PEM_FILE",
	}
	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			cause := exitsConfig(t, map[string]string{name: unset}, nil)
			require.ErrorContains(t, cause, "config "+name+": not found")
		})
	}

	t.Run("all of them", func(t *testing.T) {
		env := map[string]string{}
		for _, name := range required {
			env[name] = unset
		}
		cause := exitsConfig(t, env, nil)
		for _, name := range required {
			require.ErrorContains(t, cause, "config "+name+": not found")
		}
	})
}

func TestInit_with_a_bad_setting_exits_78(t *testing.T) {
	password := rand.Text()
	testCases := map[string]struct {
		name   string
		value  string
		files  map[string][]byte
		names  string
		secret string
	}{
		"a broker with no port":                    {name: "KAFKA_BROKERS", value: "kafka-0:9092,kafka-1"},
		"an empty group ID":                        {name: "KAFKA_GROUP_ID", value: ""},
		"an empty events topic":                    {name: "KAFKA_EVENTS_TOPIC", value: ""},
		"an empty status topic":                    {name: "KAFKA_STATUS_TOPIC", value: ""},
		"an empty retry topic":                     {name: "KAFKA_RETRY_TOPIC", value: ""},
		"an empty dead-letter topic":               {name: "KAFKA_DEAD_LETTER_TOPIC", value: ""},
		"the events topic as the retry topic":      {name: "KAFKA_RETRY_TOPIC", value: eventsTopic},
		"the status topic as the dead-letter one":  {name: "KAFKA_DEAD_LETTER_TOPIC", value: statusTopic},
		"a Kafka CA file that doesn't exist":       {name: "KAFKA_TLS_CA_FILE", value: "certs/missing.pem"},
		"a Kafka CA file with no certificate":      {name: "KAFKA_TLS_CA_FILE", value: "certs/junk.pem", files: map[string][]byte{"certs/junk.pem": []byte("junk")}},
		"a client certificate that doesn't exist":  {name: "KAFKA_TLS_CERT_FILE", value: "secrets/missing.pem"},
		"a client certificate file with no key":    {name: "KAFKA_TLS_CERT_FILE", value: "certs/kafka-ca.pem"},
		"a session timeout of 0":                   {name: "KAFKA_SESSION_TIMEOUT", value: "0s"},
		"a session timeout with no unit":           {name: "KAFKA_SESSION_TIMEOUT", value: "45"},
		"a negative delivery timeout":              {name: "KAFKA_DELIVERY_TIMEOUT", value: "-1s"},
		"a breaker open for 0":                     {name: "KAFKA_BREAKER_OPEN_FOR", value: "0s"},
		"a PostgreSQL URL with a password":         {name: "POSTGRES_URL", value: "postgres://orderstatus:" + password + "@postgres.internal:5432/orders", secret: password},
		"a PostgreSQL URL that doesn't parse":      {name: "POSTGRES_URL", value: "postgres://orderstatus:" + password + "%zz@postgres.internal:5432/orders", secret: password},
		"a MySQL URL":                              {name: "POSTGRES_URL", value: "mysql://orderstatus@postgres.internal:5432/orders"},
		"a PostgreSQL URL with no port":            {name: "POSTGRES_URL", value: "postgres://orderstatus@postgres.internal/orders"},
		"a PostgreSQL URL with no user":            {name: "POSTGRES_URL", value: "postgres://postgres.internal:5432/orders"},
		"a PostgreSQL URL with no database":        {name: "POSTGRES_URL", value: "postgres://orderstatus@postgres.internal:5432/"},
		"a PostgreSQL URL with its own sslmode":    {name: "POSTGRES_URL", value: postgresURL + "?sslmode=disable"},
		"a PostgreSQL URL with a root certificate": {name: "POSTGRES_URL", value: postgresURL + "?sslrootcert=/etc/ssl/ca.pem"},
		"a PostgreSQL URL with a passfile":         {name: "POSTGRES_URL", value: postgresURL + "?passfile=/root/.pgpass"},
		"a password file that doesn't exist":       {name: "POSTGRES_PASSWORD_FILE", value: "secrets/missing"},
		"an empty password file":                   {name: "POSTGRES_PASSWORD_FILE", value: "secrets/empty", files: map[string][]byte{"secrets/empty": []byte("\n")}},
		"a PostgreSQL CA file that doesn't exist":  {name: "POSTGRES_TLS_CA_FILE", value: "certs/missing.pem"},
		"a PostgreSQL CA file with no certificate": {name: "POSTGRES_TLS_CA_FILE", value: "certs/junk.pem", files: map[string][]byte{"certs/junk.pem": []byte("junk")}},
		"a probes address with no port":            {name: "PROBES_ADDR", value: "localhost"},
		"a probes PEM file that doesn't exist":     {name: "PROBES_TLS_PEM_FILE", value: "secrets/missing.pem"},
		"a probes PEM file with no key":            {name: "PROBES_TLS_PEM_FILE", value: "certs/kafka-ca.pem"},
		"retry attempts of 0":                      {name: "RETRY_ATTEMPTS", value: "0"},
		"negative retry attempts":                  {name: "RETRY_ATTEMPTS", value: "-1"},
		"retry attempts that aren't a number":      {name: "RETRY_ATTEMPTS", value: "ten"},
		"a minimum delay of 0":                     {name: "RETRY_MIN_DELAY", value: "0s"},
		"a negative minimum delay":                 {name: "RETRY_MIN_DELAY", value: "-1s"},
		"a minimum delay with no unit":             {name: "RETRY_MIN_DELAY", value: "1"},
		"a maximum delay of 0":                     {name: "RETRY_MAX_DELAY", value: "0s"},
		"a minimum delay above the default max":    {name: "RETRY_MIN_DELAY", value: "10m"},
		"a maximum delay below the default min":    {name: "RETRY_MAX_DELAY", value: "500ms", names: "RETRY_MIN_DELAY"},
		"a trusted topic that is empty":            {name: "FAULT_TRUSTED_TOPICS", value: eventsTopic + ","},
	}
	for desc, tc := range testCases {
		t.Run(desc, func(t *testing.T) {
			cause := exitsConfig(t, map[string]string{tc.name: tc.value}, tc.files)
			names := tc.name
			if tc.names != "" {
				names = tc.names
			}
			require.ErrorContains(t, cause, "config "+names+":")
			if tc.secret != "" {
				require.NotContains(t, cause.Error(), tc.secret)
			}
		})
	}
}

func TestInit_reads_its_secrets_through_the_Host(t *testing.T) {
	ca := humustest.NewCA(t)
	for name, contents := range map[string][]byte{
		"KAFKA_TLS_CA_FILE":      caPEM(t, ca),
		"KAFKA_TLS_CERT_FILE":    pemOf(t, ca.Issue(t, group)),
		"POSTGRES_PASSWORD_FILE": []byte(rand.Text()),
		"POSTGRES_TLS_CA_FILE":   caPEM(t, ca),
		"PROBES_TLS_PEM_FILE":    pemOf(t, ca.Issue(t, "localhost")),
	} {
		t.Run(name, func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(outside, contents, 0o600))

			cause := exitsConfig(t, map[string]string{name: outside}, nil)
			require.ErrorContains(t, cause, "config "+name+": ")
			require.ErrorContains(t, cause, "path escapes from parent")
		})
	}
}

func TestPoolConfig_leaves_nothing_for_pgx_to_read(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("PGPASSWORD", rand.Text())
	t.Setenv("PGPASSFILE", missing)
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGSSLROOTCERT", missing)
	t.Setenv("PGSSLCERT", missing)
	t.Setenv("PGSSLKEY", missing)
	t.Setenv("PGHOST", "elsewhere.internal")
	t.Setenv("PGDATABASE", "elsewhere")

	ca := humustest.NewCA(t)
	roots := ca.Pool()
	u, err := url.Parse(postgresURL + "?application_name=orderstatus")
	require.NoError(t, err)
	password := rand.Text()

	cfg, err := poolConfig(postgres{URL: u, Password: password, CA: roots})
	require.NoError(t, err)

	conn := cfg.ConnConfig
	require.Equal(t, "postgres.internal", conn.Host)
	require.Equal(t, uint16(5432), conn.Port)
	require.Equal(t, "orderstatus", conn.User)
	require.Equal(t, password, conn.Password)
	require.Equal(t, "orders", conn.Database)
	require.Equal(t, "orderstatus", conn.RuntimeParams["application_name"])
	require.Empty(t, conn.Fallbacks)
	require.NotNil(t, conn.TLSConfig)
	require.Same(t, roots, conn.TLSConfig.RootCAs)
	require.Equal(t, "postgres.internal", conn.TLSConfig.ServerName)
	require.False(t, conn.TLSConfig.InsecureSkipVerify)
	require.Empty(t, conn.TLSConfig.Certificates)
}

func TestOpenPostgres_dials_through_the_Host_and_closes_the_pool_on_stop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ln, err := h.Listen("tcp", "postgres.internal:5432")
		require.NoError(t, err)
		defer func() { _ = ln.Close() }()
		requested := make(chan []byte, 1)
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			b := make([]byte, 8)
			_, _ = io.ReadFull(conn, b)
			requested <- b
		}()

		u, err := url.Parse(postgresURL)
		require.NoError(t, err)
		cfg, err := poolConfig(postgres{URL: u, Password: rand.Text(), CA: humustest.NewCA(t).Pool()})
		require.NoError(t, err)

		init := func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			st, err := openPostgres(ctx, s, cfg)
			if err != nil {
				return nil, err
			}
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				_, err := st.Applied(ctx, 0, 0)
				if err == nil {
					return errors.New("applied with no PostgreSQL to answer")
				}
				return nil
			})}, nil
		}
		_, status := humustest.Start(t, init, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		sslRequest := []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}
		require.Equal(t, sslRequest, <-requested)
	})
}

func TestInit_projects_an_order_and_turns_ready_once_it_has_joined_its_group(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, nil)
		signal, status := p.start(t, nil)
		<-p.h.Listening(probesAddr)
		synctest.Wait()

		require.Equal(t, http.StatusServiceUnavailable, p.probe(t, "/health/readiness"))
		require.Equal(t, http.StatusOK, p.probe(t, "/health/liveness"))

		time.Sleep(time.Minute)
		synctest.Wait()
		require.Equal(t, http.StatusServiceUnavailable, p.probe(t, "/health/readiness"))

		c := p.cluster(t, kfake.Ports(9092))
		at := time.Now().UTC().Truncate(time.Millisecond)
		p.produce(t, c, event("order-1", projector.Placed, at))
		waitFor(t, "order-1 is projected", func() bool { return p.store.status("order-1") == projector.Placed })

		require.Equal(t, http.StatusOK, p.probe(t, "/health/readiness"))

		signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		changes := p.readAll(t, c, statusTopic)
		require.Len(t, changes, 1)
		require.Equal(t, "order-1", string(changes[0].Key))
		require.JSONEq(t, `{"order_id":"order-1","status":"placed","previous":"","at":"`+at.Format(time.RFC3339Nano)+`"}`, string(changes[0].Value))
		require.Empty(t, p.readAll(t, c, retryTopic))
		require.Empty(t, p.readAll(t, c, deadLetterTopic))
		require.Equal(t, int64(1), committed(c, eventsTopic))
	})
}

var graceEnv = map[string]string{
	"HUMUS_SHUTDOWN_GRACE_PERIOD": "20s",
	"RETRY_MIN_DELAY":             "5m",
	"RETRY_MAX_DELAY":             "5m",
}

// produceRetried produces order-1's Placed event to the retry topic, as a
// first attempt at it would have, so the projector waits out
// RETRY_MIN_DELAY before it projects it.
func produceRetried(t *testing.T, p *processor, c *kfake.Cluster) {
	t.Helper()
	retried := event("order-1", projector.Placed, time.Now().UTC())
	retried.Topic = retryTopic
	retried.Headers = []kgo.RecordHeader{
		{Key: "retry.attempt", Value: []byte("1")},
		{Key: "retry.reason", Value: []byte("unavailable")},
		{Key: "retry.partition", Value: []byte("0")},
		{Key: "retry.offset", Value: []byte("0")},
	}
	p.produce(t, c, retried)
}

// consumed is how many records of topic the App's Service has handed its
// projector, by the Service's messaging.client.consumed.messages.
func consumed(t *testing.T, tel *humustest.Telemetry, topic string) int64 {
	t.Helper()
	rm, err := tel.Metrics(context.Background())
	require.NoError(t, err)
	m, ok := scopeMetric(rm, "github.com/z5labs/humus/queue/kafka", "messaging.client.consumed.messages")
	if !ok {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s is a %T", m.Name, m.Data)
	var n int64
	for _, dp := range sum.DataPoints {
		if v, _ := dp.Attributes.Value("messaging.destination.name"); v.AsString() == topic {
			n += dp.Value
		}
	}
	return n
}

// signalWhileWaiting sends the App SIGTERM once its projector has been
// handed order-1's retried record and is waiting out its delay.
//
// Readiness is no sign of that. The App turns ready once it has joined its
// group, before franz-go has loaded an offset or fetched a record, and
// synctest.Wait returns once every goroutine is durably blocked, which one
// waiting on a timer is: franz-go backing off after a failed Fetch, say.
// So signalWhileWaiting waits, as long as that takes, for the Service to
// count the record as consumed, which it does just before it hands the
// record to the projector. The projector starts its delay's timer without
// blocking on anything else first, so once synctest.Wait returns, the
// record is waiting out its delay.
func signalWhileWaiting(t *testing.T, p *processor, tel *humustest.Telemetry, signal func(os.Signal)) {
	t.Helper()
	waitFor(t, "the projector is handed order-1's retried record", func() bool { return consumed(t, tel, retryTopic) == 1 })
	synctest.Wait()
	for _, s := range tel.Spans() {
		require.NotEqual(t, "process "+retryTopic, s.Name(), "order-1's retried record was processed before the SIGTERM")
	}
	<-p.h.Listening(probesAddr)
	require.Equal(t, http.StatusOK, p.probe(t, "/health/readiness"))
	signal(syscall.SIGTERM)
}

// requireCutShort requires that the grace period ran out with order-1's
// retried record still waiting: the App exits 1 with a GracePeriodError,
// order-1 is unprojected and nothing is committed on the retry topic.
func requireCutShort(t *testing.T, p *processor, c *kfake.Cluster, st bedrock.ExitStatus) {
	t.Helper()
	require.Equal(t, 1, st.Code(), "cause: %v", st.Cause())
	gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
	require.True(t, ok, "cause = %v, want a *humus.GracePeriodError", st.Cause())
	require.Equal(t, 20*time.Second, gerr.GracePeriod)
	require.Empty(t, p.store.status("order-1"))
	require.Less(t, committed(c, retryTopic), int64(1))
}

// holdFetches holds every Fetch the cluster gets until release is closed.
// The returned channel is closed when the first Fetch arrives.
func holdFetches(c *kfake.Cluster, release <-chan struct{}) <-chan struct{} {
	arrived := make(chan struct{})
	var once sync.Once
	c.ControlKey(kmsg.Fetch.Int16(), func(kmsg.Request) (kmsg.Response, error, bool) {
		c.KeepControl()
		once.Do(func() { close(arrived) })
		c.SleepControl(func() { <-release })
		return nil, nil, false
	})
	return arrived
}

// failOnce closes the connection of the first request of key the cluster
// gets, so franz-go backs off on a timer before it sends it again.
func failOnce(c *kfake.Cluster, key kmsg.Key) {
	c.ControlKey(key.Int16(), func(kmsg.Request) (kmsg.Response, error, bool) {
		return nil, fmt.Errorf("closing the first %s's connection", key.Name()), true
	})
}

func TestInit_cuts_short_a_retried_record_still_waiting_when_the_grace_period_runs_out(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, nil)
		c := p.cluster(t)
		produceRetried(t, p, c)

		tel := humustest.NewTelemetry()
		signal, status := p.start(t, graceEnv, humus.WithTelemetry(tel.Builder()))
		signalWhileWaiting(t, p, tel, signal)
		requireCutShort(t, p, c, <-status)

		signal, status = p.start(t, graceEnv)
		waitFor(t, "order-1 is projected by the second App", func() bool { return p.store.status("order-1") == projector.Placed })
		signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Len(t, p.readAll(t, c, statusTopic), 1)
		require.Equal(t, int64(1), committed(c, retryTopic))
	})
}

func TestInit_cuts_short_a_retried_record_whose_first_fetch_is_held_until_after_readiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, nil)
		c := p.cluster(t)
		produceRetried(t, p, c)
		release := make(chan struct{})
		held := holdFetches(c, release)

		tel := humustest.NewTelemetry()
		signal, status := p.start(t, graceEnv, humus.WithTelemetry(tel.Builder()))
		<-p.h.Listening(probesAddr)
		<-held
		synctest.Wait()
		require.Equal(t, http.StatusOK, p.probe(t, "/health/readiness"))

		time.AfterFunc(time.Second, func() { close(release) })
		signalWhileWaiting(t, p, tel, signal)
		requireCutShort(t, p, c, <-status)
	})
}

func TestInit_cuts_short_a_retried_record_whose_first_fetch_fails_once(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, nil)
		c := p.cluster(t)
		produceRetried(t, p, c)
		failOnce(c, kmsg.Fetch)

		tel := humustest.NewTelemetry()
		signal, status := p.start(t, graceEnv, humus.WithTelemetry(tel.Builder()))
		signalWhileWaiting(t, p, tel, signal)
		requireCutShort(t, p, c, <-status)
	})
}

func TestInit_cuts_short_a_retried_record_whose_offset_load_fails_once(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, nil)
		c := p.cluster(t)
		produceRetried(t, p, c)
		failOnce(c, kmsg.OffsetFetch)

		tel := humustest.NewTelemetry()
		signal, status := p.start(t, graceEnv, humus.WithTelemetry(tel.Builder()))
		signalWhileWaiting(t, p, tel, signal)
		requireCutShort(t, p, c, <-status)
	})
}

func TestInit_stops_on_a_record_that_can_go_to_neither_the_retry_nor_the_dead_letter_topic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, nil)
		c := p.cluster(t)
		p.produce(t, c, event("order-1", projector.Placed, time.Now().UTC()))
		var refusing atomic.Bool
		refusing.Store(true)
		refuseProduce(c, &refusing)

		_, status := p.start(t, nil)
		st := <-status
		require.Equal(t, 1, st.Code(), "cause: %v", st.Cause())
		require.ErrorContains(t, st.Cause(), "processing "+eventsTopic+", partition 0, offset 0")
		require.ErrorContains(t, st.Cause(), "retrying "+eventsTopic+", partition 0, offset 0")
		require.Empty(t, p.store.status("order-1"))
		require.Less(t, committed(c, eventsTopic), int64(1))

		refusing.Store(false)
		signal, status := p.start(t, nil)
		waitFor(t, "order-1 is projected by the second App", func() bool { return p.store.status("order-1") == projector.Placed })
		signal(syscall.SIGTERM)
		st = <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Len(t, p.readAll(t, c, statusTopic), 1)
		require.Empty(t, p.readAll(t, c, retryTopic))
		require.Empty(t, p.readAll(t, c, deadLetterTopic))
		require.Equal(t, int64(1), committed(c, eventsTopic))
	})
}
