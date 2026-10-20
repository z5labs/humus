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
	"encoding/binary"
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
	"runtime"
	"sort"
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
	"github.com/z5labs/humus/example/orderstatus/cases"
	"github.com/z5labs/humus/example/orderstatus/projector"
	"github.com/z5labs/humus/example/orderstatus/store"
	"github.com/z5labs/humus/fault/jsonscenario"
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

var casesEnv = map[string]string{
	"RETRY_ATTEMPTS":       "3",
	"RETRY_MIN_DELAY":      "100ms",
	"RETRY_MAX_DELAY":      "400ms",
	"FAULT_TRUSTED_TOPICS": eventsTopic + "," + retryTopic,
}

type running struct {
	signal func(os.Signal)
	status <-chan bedrock.ExitStatus
}

func (p *processor) startForCases(t *testing.T, probes string) running {
	t.Helper()
	env := maps.Clone(casesEnv)
	env["PROBES_ADDR"] = probes
	signal, status := p.start(t, env, humus.WithFaults(Faults))
	return running{signal: signal, status: status}
}

func (r running) stop(t *testing.T) {
	t.Helper()
	r.signal(syscall.SIGTERM)
	st := <-r.status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

type rollout struct {
	t       *testing.T
	p       *processor
	c       *kfake.Cluster
	current running
	probes  []string
}

func (r *rollout) run(ctx context.Context) error {
	release := pauseFetches(r.c)
	defer release()
	synctest.Wait()

	next := r.p.startForCases(r.t, r.probes[0])
	r.probes = append(r.probes[1:], r.probes[0])
	if err := r.await(ctx, next, 2); err != nil {
		return err
	}

	r.current.signal(syscall.SIGTERM)
	var st bedrock.ExitStatus
	select {
	case st = <-r.current.status:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	r.current = next
	if st.Code() != 0 {
		return fmt.Errorf("the replaced App exited %d: %w", st.Code(), st.Cause())
	}
	return r.await(ctx, next, 1)
}

func (r *rollout) await(ctx context.Context, next running, members int) error {
	for {
		info := r.c.GroupInfo(group)
		if info != nil && info.State == "Stable" && len(info.Members) == members {
			return nil
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case st := <-next.status:
			return fmt.Errorf("the new App exited %d: %w", st.Code(), st.Cause())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func pauseFetches(c *kfake.Cluster) func() {
	release := make(chan struct{})
	c.ControlKey(kmsg.Fetch.Int16(), func(kmsg.Request) (kmsg.Response, error, bool) {
		c.KeepControl()
		select {
		case <-release:
		default:
			c.SleepControl(func() { <-release })
		}
		return nil, nil, false
	})
	var once sync.Once
	return func() { once.Do(func() { close(release) }) }
}

func TestInit_passes_the_shared_cases(t *testing.T) {
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newProcessor(t, nil)
				cl := p.cluster(t, kfake.SeedTopics(3, eventsTopic, statusTopic, retryTopic, deadLetterTopic))
				r := &rollout{t: t, p: p, c: cl, current: p.startForCases(t, probesAddr), probes: []string{"localhost:8444", probesAddr}}

				c.Run(t, cases.Target{
					Kafka:           []kgo.Opt{kgo.SeedBrokers(cl.ListenAddrs()...), kgo.Dialer(p.dial)},
					EventsTopic:     eventsTopic,
					StatusTopic:     statusTopic,
					RetryTopic:      retryTopic,
					DeadLetterTopic: deadLetterTopic,
					Group:           group,
					Scenario:        codec,
					ScenarioKey:     "acme.fault",
					RetryAttempts:   3,
					RetryMinDelay:   100 * time.Millisecond,
					RetryMaxDelay:   400 * time.Millisecond,
					Timeout:         time.Minute,
					Rollout:         r.run,
				})

				r.current.stop(t)
			})
		})
	}
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

// trace logs what happens during a close, with the bubble time since it
// began. It stops logging before the test returns, as franz-go's goroutines
// may still log.
type trace struct {
	t     *testing.T
	armed atomic.Bool

	mu    sync.Mutex
	start time.Time
	done  bool
}

func (tr *trace) arm() {
	tr.mu.Lock()
	tr.start = time.Now()
	tr.mu.Unlock()
	tr.armed.Store(true)
}

func (tr *trace) log(format string, args ...any) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.done {
		return
	}
	tr.t.Logf("%-8v "+format, append([]any{time.Since(tr.start)}, args...)...)
}

func (tr *trace) stop() {
	tr.mu.Lock()
	tr.done = true
	tr.mu.Unlock()
}

// Level and Log make trace franz-go's logger, at debug, once it is armed.
func (tr *trace) Level() kgo.LogLevel { return kgo.LogLevelDebug }

func (tr *trace) Log(level kgo.LogLevel, msg string, keyvals ...any) {
	if tr.armed.Load() {
		tr.log("franz-go %s: %s %v", level, msg, keyvals)
	}
}

// closeRace orders the close of a consumer's fetch connection. Close stops
// the consumer's session, which cuts short the read of the long-poll Fetch
// in flight, and then sends a Fetch that kills the fetch session. closeRace
// holds the cut-short read until the session-kill Fetch is written on the
// same connection, so the read fails, and franz-go closes the connection,
// after that write. With holdReset, it also holds the write deadline reset
// that follows that write until crypto/tls is writing close_notify. Without
// it, the read waits for the reset instead.
type closeRace struct {
	tr        *trace
	holdReset bool

	killWritten  chan struct{} // closed once the session-kill Fetch is written
	resetDone    chan struct{} // closed once that write has reset the write deadline
	alertWriting chan struct{} // closed as crypto/tls begins writing close_notify

	mu     sync.Mutex
	writes map[string]int // the requests written since the close began
}

func newCloseRace(tr *trace, holdReset bool) *closeRace {
	return &closeRace{
		tr:           tr,
		holdReset:    holdReset,
		killWritten:  make(chan struct{}),
		resetDone:    make(chan struct{}),
		alertWriting: make(chan struct{}),
		writes:       map[string]int{},
	}
}

// wait waits for ch, for at most 10s of bubble time, so that an order that
// never comes doesn't hold a call forever.
func (r *closeRace) wait(what string, ch <-chan struct{}) {
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		r.tr.log("gave up waiting for %s", what)
	}
}

func (r *closeRace) count(key int16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes[kmsg.NameForKey(key)]++
}

func (r *closeRace) written() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for name, n := range r.writes {
		out = append(out, fmt.Sprintf("%s %d", name, n))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// dial dials as processor.dial does, and wraps the connection on both
// sides of crypto/tls.
func (r *closeRace) dial(p *processor) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := p.h.Dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		c := &raceConn{r: r}
		c.wire = &wireConn{Conn: conn, log: r.tr.log, onAlert: func() { close(r.alertWriting) }}
		cfg := p.client.Clone()
		cfg.ServerName, _, _ = net.SplitHostPort(address)
		tc := tls.Client(c.wire, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		c.Conn = tc
		return c, nil
	}
}

// raceConn is the *tls.Conn franz-go writes requests on.
type raceConn struct {
	net.Conn
	r    *closeRace
	wire *wireConn

	fetch  atomic.Bool // a Fetch has been written on it
	killer atomic.Bool // the session-kill Fetch has been written on it
	reset  atomic.Bool // that write has reset the write deadline
}

func (c *raceConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if len(b) < 6 {
		return n, err
	}
	key := int16(binary.BigEndian.Uint16(b[4:6]))
	if c.r.tr.armed.Load() {
		c.r.count(key)
		if key == kmsg.Fetch.Int16() && c.fetch.Load() && c.killer.CompareAndSwap(false, true) {
			c.r.tr.log("the session-kill Fetch is written on the long-poll Fetch's connection: %d bytes, err %v", n, err)
			close(c.r.killWritten)
		}
	}
	if key == kmsg.Fetch.Int16() {
		c.fetch.Store(true)
	}
	return n, err
}

func (c *raceConn) SetReadDeadline(t time.Time) error {
	if c.r.tr.armed.Load() && c.fetch.Load() && !c.killer.Load() && !t.IsZero() && !t.After(time.Now()) {
		if c.r.holdReset {
			c.r.tr.log("franz-go cuts short the long-poll Fetch's read; held until the session-kill Fetch is written")
			c.r.wait("the session-kill Fetch", c.r.killWritten)
		} else {
			c.r.tr.log("franz-go cuts short the long-poll Fetch's read; held until the session-kill Fetch's write resets the write deadline")
			c.r.wait("the write deadline reset", c.r.resetDone)
		}
		c.r.tr.log("the long-poll Fetch's read is cut short")
	}
	return c.Conn.SetReadDeadline(t)
}

func (c *raceConn) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() || !c.killer.Load() || !c.reset.CompareAndSwap(false, true) {
		return c.Conn.SetWriteDeadline(t)
	}
	if c.r.holdReset {
		c.r.tr.log("the session-kill Fetch's write resets the write deadline; held until crypto/tls writes close_notify")
		c.r.wait("close_notify", c.r.alertWriting)
	}
	err := c.Conn.SetWriteDeadline(t)
	c.r.tr.log("the session-kill Fetch's write has reset the connection's write deadline to none")
	close(c.r.resetDone)
	return err
}

func (c *raceConn) Close() error {
	if !c.killer.Load() {
		return c.Conn.Close()
	}
	c.wire.closing.Store(true)
	c.r.tr.log("franz-go closes the long-poll Fetch's connection")
	err := c.Conn.Close()
	c.r.tr.log("the connection's Close returns: %v", err)
	return err
}

// wireConn is the connection beneath crypto/tls. Once closing is set, it
// logs crypto/tls's write deadlines, and reports its first write, which is
// close_notify.
type wireConn struct {
	net.Conn
	log     func(format string, args ...any)
	onAlert func()

	closing atomic.Bool
	alerted atomic.Bool
}

func (w *wireConn) SetWriteDeadline(t time.Time) error {
	if w.closing.Load() {
		if t.IsZero() {
			w.log("the write deadline beneath crypto/tls is set to none")
		} else {
			w.log("the write deadline beneath crypto/tls is set %v ahead", time.Until(t))
		}
	}
	return w.Conn.SetWriteDeadline(t)
}

func (w *wireConn) Write(b []byte) (int, error) {
	if !w.closing.Load() || !w.alerted.CompareAndSwap(false, true) {
		return w.Conn.Write(b)
	}
	w.log("crypto/tls writes close_notify: %d bytes", len(b))
	w.onAlert()
	start := time.Now()
	n, err := w.Conn.Write(b)
	w.log("close_notify's write returns after %v: %v", time.Since(start), err)
	return n, err
}

// closeReadAllClient polls and closes a client as readAll does, with the
// connections ordered by closeRace, and fails the test if Close hasn't
// returned after a minute of bubble time.
func closeReadAllClient(t *testing.T, holdReset bool) {
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, nil)
		c := p.cluster(t)
		p.produce(t, c, &kgo.Record{Topic: statusTopic, Key: []byte("order-1"), Value: []byte(`{"order_id":"order-1"}`)})

		tr := &trace{t: t}
		defer tr.stop()
		r := newCloseRace(tr, holdReset)
		cl, err := kgo.NewClient(
			kgo.SeedBrokers(c.ListenAddrs()...),
			kgo.Dialer(r.dial(p)),
			kgo.ConsumeTopics(statusTopic),
			kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
			kgo.WithLogger(tr),
		)
		require.NoError(t, err)

		var out []*kgo.Record
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			fetches := cl.PollFetches(ctx)
			cancel()
			n := 0
			fetches.EachRecord(func(rec *kgo.Record) {
				out = append(out, rec)
				n++
			})
			if n == 0 {
				break
			}
		}
		require.Len(t, out, 1)

		tr.arm()
		tr.log("readAll closes its client, with its long-poll Fetch in flight")
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			cl.Close()
		}()

		select {
		case <-closed:
			tr.log("Close returns")
			tr.log("the client wrote: %s", r.written())
		case <-time.After(time.Minute):
			t.Errorf("Close hasn't returned after %v of bubble time", time.Minute)
			tr.log("Close hasn't returned; meanwhile the client wrote: %s", r.written())
			for _, g := range stuckGoroutines() {
				tr.log("stuck:\n%s", g)
			}
			c.Close()
			<-closed
			tr.log("Close returns once the cluster closes its connections")
		}
	})
}

// stuckGoroutines returns the stacks of the goroutines in a synctest bubble
// that write on a pipe, wait on the session-kill Fetch, or are kfake's
// reader of a connection: the ones the hung runs of #1098 showed.
func stuckGoroutines() []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, "synctest bubble") {
			continue
		}
		for _, frame := range []string{"net.(*pipe).write", "killSessionOnClose", "kfake.(*clientConn).read("} {
			if strings.Contains(g, frame) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

func TestReadAll_closes_its_client_when_the_session_kill_Fetch_resets_the_deadline_close_notify_waits_on(t *testing.T) {
	closeReadAllClient(t, true)
}

func TestReadAll_closes_its_client_when_the_session_kill_Fetch_resets_its_deadline_before_close_notify(t *testing.T) {
	closeReadAllClient(t, false)
}
