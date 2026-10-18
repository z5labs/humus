//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

// The tests in this file run Kafka itself, in a container started with
// testcontainers-go, and their Apps run on humus.OSHost(). They are the only
// tests in the package that open a real socket, so they sit behind the
// testcontainers build tag and run in a CI job of their own, outside the
// coverage gate:
//
//	go -C queue/kafka test -race -tags testcontainers ./...
//
// Every other test runs against kfake, inside a testing/synctest bubble.
// kfake is a reimplementation of Kafka's protocol, coordinator included, and
// these tests are where kfake and Kafka would be seen to differ: each one says
// what it shows that kfake can't. They run in real time, so their waits are
// bounded polls rather than synctest.Wait.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

// kafkaImage is the Kafka the tests run: Apache Kafka's GraalVM native
// image, in KRaft mode, which starts in seconds.
const kafkaImage = "docker.io/apache/kafka-native:4.3.1"

// broker is the one Kafka every test in the package shares, started by the
// first test that needs it and stopped by TestMain. Each test creates its own
// topic and consumer group, so the tests don't see each other's records.
var broker struct {
	once      sync.Once
	addr      string
	container testcontainers.Container
	err       error
}

func TestMain(m *testing.M) {
	m.Run()
	if broker.container != nil {
		// The tests have finished; a container left behind is reaped by
		// testcontainers' own reaper.
		_ = broker.container.Terminate(context.Background())
	}
}

// brokers returns the address of the shared Kafka, starting it on first use.
func brokers(t *testing.T) []string {
	t.Helper()
	broker.once.Do(func() {
		broker.addr, broker.container, broker.err = startKafka()
	})
	require.NoError(t, broker.err, "starting Kafka in a container")
	return []string{broker.addr}
}

// startKafka starts a single-node Kafka, broker and controller, and returns
// the address its clients reach it on.
//
// Kafka hands its clients the address it advertises, so the advertised
// listener must name the port on the host, which therefore has to be known
// before the container starts: a free port is picked, and the container's
// 9092 is bound to it.
func startKafka() (string, testcontainers.Container, error) {
	port, err := freePort()
	if err != nil {
		return "", nil, err
	}
	addr := net.JoinHostPort("127.0.0.1", port)
	req := testcontainers.ContainerRequest{
		Image:        kafkaImage,
		ExposedPorts: []string{"9092/tcp"},
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{
				network.MustParsePort("9092/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: port}},
			}
		},
		Env: map[string]string{
			"KAFKA_NODE_ID":                        "1",
			"KAFKA_PROCESS_ROLES":                  "broker,controller",
			"KAFKA_CONTROLLER_QUORUM_VOTERS":       "1@localhost:9093",
			"KAFKA_CONTROLLER_LISTENER_NAMES":      "CONTROLLER",
			"KAFKA_LISTENERS":                      "PLAINTEXT://0.0.0.0:9092,CONTROLLER://0.0.0.0:9093",
			"KAFKA_ADVERTISED_LISTENERS":           "PLAINTEXT://" + addr,
			"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP": "PLAINTEXT:PLAINTEXT,CONTROLLER:PLAINTEXT",
			"KAFKA_INTER_BROKER_LISTENER_NAME":     "PLAINTEXT",

			"KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR":         "1",
			"KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR": "1",
			"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR":            "1",
			// A group's first rebalance starts at once, rather than
			// waiting three seconds for more members.
			"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS": "0",
			"KAFKA_AUTO_CREATE_TOPICS_ENABLE":        "false",
		},
		WaitingFor: wait.ForLog("Kafka Server started").WithStartupTimeout(2 * time.Minute),
	}
	c, err := testcontainers.GenericContainer(context.Background(), testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return "", c, err
	}
	return addr, c, nil
}

// freePort returns a TCP port that is free on the host now.
func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("picking a port for Kafka: %w", err)
	}
	_, port, err := net.SplitHostPort(l.Addr().String())
	if cerr := l.Close(); err == nil {
		err = cerr
	}
	return port, err
}

// admin returns an admin client of the shared Kafka, closed when the test
// ends.
func admin(t *testing.T) *kadm.Client {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers(t)...))
	require.NoError(t, err)
	t.Cleanup(cl.Close)
	return kadm.NewClient(cl)
}

// unique returns prefix followed by a random suffix, so that each test has a
// topic and a group of its own.
func unique(prefix string) string {
	return prefix + "-" + strings.ToLower(rand.Text())
}

// newTopic creates a topic with the given number of partitions and returns
// its name.
func newTopic(t *testing.T, adm *kadm.Client, partitions int32) string {
	t.Helper()
	name := unique("orders")
	resp, err := adm.CreateTopic(t.Context(), partitions, 1, nil, name)
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	return name
}

// produceAt writes counts[p] records to each partition p of topic. Their
// values are their sequence numbers in the call, from "0".
func produceAt(t *testing.T, topic string, counts map[int32]int) {
	t.Helper()
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers(t)...),
		kgo.RecordPartitioner(kgo.ManualPartitioner()),
	)
	require.NoError(t, err)
	defer cl.Close()
	var records []*kgo.Record
	for p, n := range counts {
		for i := range n {
			records = append(records, &kgo.Record{Topic: topic, Partition: p, Value: []byte(strconv.Itoa(i))})
		}
	}
	require.NoError(t, cl.ProduceSync(t.Context(), records...).FirstErr())
}

// committedTo returns group's committed offset of each partition of topic,
// as Kafka answers an OffsetFetch for it.
func committedTo(t *testing.T, adm *kadm.Client, group, topic string) map[int32]int64 {
	t.Helper()
	offsets, err := fetchCommitted(adm, group, topic)
	require.NoError(t, err)
	return offsets
}

// fetchCommitted is committedTo for a Processor, which runs outside the
// test's goroutine and so can't fail the test itself.
func fetchCommitted(adm *kadm.Client, group, topic string) (map[int32]int64, error) {
	resps, err := adm.FetchOffsets(context.Background(), group)
	if err != nil {
		return nil, err
	}
	offsets := make(map[int32]int64)
	for p, r := range resps[topic] {
		if r.Err != nil {
			return nil, r.Err
		}
		offsets[p] = r.At
	}
	return offsets, nil
}

// realSettings is DefaultSettings with the shared Kafka, group and topic.
func realSettings(t *testing.T, group, topic string) kafka.Settings {
	s := kafka.DefaultSettings()
	s.Brokers = brokers(t)
	s.GroupID = group
	s.Topics = []string{topic}
	return s
}

// startOnOS runs an App whose only Service consumes s with p, on
// humus.OSHost(), with opts after it.
func startOnOS(t *testing.T, s kafka.Settings, p queue.ProcessorFunc[kafka.Message], opts ...humus.Option) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	return humustest.Start(t, consumes(s, p), humustest.WithApp(append([]humus.Option{humus.WithHost(humus.OSHost())}, opts...)...))
}

// terminate sends p SIGTERM, once the App listens for it, and returns the
// App's status.
func terminate(t *testing.T, p *bedrocktest.Process, status <-chan bedrock.ExitStatus) bedrock.ExitStatus {
	t.Helper()
	<-p.Notified(syscall.SIGTERM)
	p.Signal(syscall.SIGTERM)
	return exited(t, status)
}

// exited returns the App's status, failing the test if the App hasn't
// exited within a minute.
func exited(t *testing.T, status <-chan bedrock.ExitStatus) bedrock.ExitStatus {
	t.Helper()
	var st bedrock.ExitStatus
	select {
	case st = <-status:
	case <-time.After(time.Minute):
		t.Fatal("the App did not exit within a minute")
	}
	return st
}

// processing is one record an App processed: when Process was called and
// when it returned.
type processing struct {
	partition int32
	offset    int64
	start     time.Time
	end       time.Time
}

// ledger records, for each App of a test, every record it processed. It is
// safe for concurrent use.
type ledger struct {
	mu   sync.Mutex
	apps map[string][]processing
}

func newLedger() *ledger {
	return &ledger{apps: make(map[string][]processing)}
}

// processor returns a Processor that runs work, if not nil, and records
// each record app processed, whatever work returned.
func (l *ledger) processor(app string, work func(context.Context, kafka.Message) error) queue.ProcessorFunc[kafka.Message] {
	return func(ctx context.Context, m kafka.Message) error {
		start := time.Now()
		var err error
		if work != nil {
			err = work(ctx, m)
		}
		l.mu.Lock()
		l.apps[app] = append(l.apps[app], processing{partition: m.Partition, offset: m.Offset, start: start, end: time.Now()})
		l.mu.Unlock()
		return err
	}
}

// of returns the records app processed, in the order it finished them.
func (l *ledger) of(app string) []processing {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.apps[app])
}

// offsets returns, for each partition, the offsets app processed, in order.
func (l *ledger) offsets(app string) map[int32][]int64 {
	out := make(map[int32][]int64)
	for _, r := range l.of(app) {
		out[r.partition] = append(out[r.partition], r.offset)
	}
	return out
}

// covers reports whether the Apps, together, processed every offset from 0
// to n-1 of each of partitions.
func (l *ledger) covers(partitions int32, n int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := make(map[int32]map[int64]bool)
	for _, rs := range l.apps {
		for _, r := range rs {
			if seen[r.partition] == nil {
				seen[r.partition] = make(map[int64]bool)
			}
			seen[r.partition][r.offset] = true
		}
	}
	for p := range partitions {
		if len(seen[p]) < n {
			return false
		}
	}
	return true
}

// eventually fails the test unless cond holds within d.
func eventually(t *testing.T, d time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	require.Eventually(t, cond, d, 100*time.Millisecond, append([]any{msg}, args...)...)
}

func TestKafka_at_least_once_redelivers_from_the_committed_offset(t *testing.T) {
	// kfake keeps a group's commits in memory, in the same struct that
	// answers OffsetFetch. Here the commit goes through Kafka's group
	// coordinator to __consumer_offsets, and a second App, a new member
	// of the group, resumes from the offset Kafka read back from there.
	t.Parallel()

	adm := admin(t)
	topic, group := newTopic(t, adm, 1), unique("group")
	produceAt(t, topic, map[int32]int{0: 5}) // offsets 0 to 4

	bad := errors.New("cannot process")
	l := newLedger()
	_, status := startOnOS(t, realSettings(t, group, topic), l.processor("first", func(_ context.Context, m kafka.Message) error {
		if m.Offset == 3 {
			return bad
		}
		return nil
	}))
	st := exited(t, status)
	require.Equal(t, humus.ExitFailure, st.Code())
	require.ErrorIs(t, st.Cause(), bad)
	require.Equal(t, map[int32][]int64{0: {0, 1, 2, 3}}, l.offsets("first"))
	require.Equal(t, map[int32]int64{0: 3}, committedTo(t, adm, group, topic), "0 to 2 are committed, 3 is not")

	p, status := startOnOS(t, realSettings(t, group, topic), l.processor("second", nil))
	eventually(t, 30*time.Second, func() bool { return len(l.of("second")) == 2 }, "the second App processes 3 and 4")
	st = terminate(t, p, status)
	require.Zero(t, st.Code(), st.Cause())
	require.Equal(t, map[int32][]int64{0: {3, 4}}, l.offsets("second"), "3 is processed again, from the committed offset")
	require.Equal(t, map[int32]int64{0: 5}, committedTo(t, adm, group, topic))
}

func TestKafka_at_most_once_commits_a_batch_before_processing_it(t *testing.T) {
	// kfake applies a commit to its in-memory group as it answers the
	// request. Here each record's Process asks Kafka, over a connection of
	// its own, for the group's committed offset, and Kafka already has the
	// offset past that record: the commit was durable before processing
	// began.
	t.Parallel()

	adm := admin(t)
	topic, group := newTopic(t, adm, 1), unique("group")
	produceAt(t, topic, map[int32]int{0: 5})

	s := realSettings(t, group, topic)
	s.Delivery = queue.AtMostOnce
	var (
		mu sync.Mutex
		// atStart is the committed offset when each record's processing
		// began, by the record's offset.
		atStart   = map[int64]int64{}
		fetchErrs []error
	)
	l := newLedger()
	p, status := startOnOS(t, s, l.processor("app", func(_ context.Context, m kafka.Message) error {
		c, err := fetchCommitted(adm, group, topic)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			fetchErrs = append(fetchErrs, err)
			return nil
		}
		atStart[m.Offset] = c[0]
		return nil
	}))
	eventually(t, 30*time.Second, func() bool { return len(l.of("app")) == 5 }, "the App processes 0 to 4")
	st := terminate(t, p, status)
	require.Zero(t, st.Code(), st.Cause())

	require.Equal(t, map[int32][]int64{0: {0, 1, 2, 3, 4}}, l.offsets("app"))
	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, fetchErrs)
	require.Len(t, atStart, 5)
	for offset, c := range atStart {
		require.Greater(t, c, offset, "offset %d was committed before it was processed", offset)
	}
	require.Equal(t, int64(5), atStart[0], "the whole batch, 0 to 4, was committed before 0 was processed")
}

func TestKafka_cooperative_sticky_rebalance_moves_one_partition_after_its_record_in_flight(t *testing.T) {
	// kfake runs a cooperative rebalance with its own coordinator, on
	// synctest's clock. Here Kafka's coordinator runs both phases, with
	// real heartbeats and real time between them: the joining App gets one
	// partition, and only once the first App's revoke callback has let
	// the record in flight finish and committed it.
	t.Parallel()

	const (
		partitions = 2
		n          = 80
	)
	adm := admin(t)
	topic, group := newTopic(t, adm, partitions), unique("group")
	produceAt(t, topic, map[int32]int{0: n, 1: n})

	l := newLedger()
	var (
		mu sync.Mutex
		// startedAt is the committed offset of each partition when the
		// second App began its first record of it.
		startedAt = map[int32]int64{}
		fetchErrs []error
	)
	slow := func(context.Context, kafka.Message) error {
		time.Sleep(250 * time.Millisecond)
		return nil
	}
	tel := humustest.NewTelemetry()
	first, firstStatus := startOnOS(t, realSettings(t, group, topic), l.processor("first", slow), humus.WithTelemetry(tel.Builder()))
	eventually(t, 30*time.Second, func() bool {
		o := l.offsets("first")
		return len(o[0]) >= 3 && len(o[1]) >= 3
	}, "the first App processes both partitions")

	second, secondStatus := startOnOS(t, realSettings(t, group, topic), l.processor("second", func(ctx context.Context, m kafka.Message) error {
		mu.Lock()
		if _, ok := startedAt[m.Partition]; !ok {
			if c, err := fetchCommitted(adm, group, topic); err != nil {
				fetchErrs = append(fetchErrs, err)
			} else {
				startedAt[m.Partition] = c[m.Partition]
			}
		}
		mu.Unlock()
		return slow(ctx, m)
	}))
	eventually(t, 2*time.Minute, func() bool { return l.covers(partitions, n) }, "the two Apps process every record")

	st := terminate(t, first, firstStatus)
	require.Zero(t, st.Code(), st.Cause())
	st = terminate(t, second, secondStatus)
	require.Zero(t, st.Code(), st.Cause())

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, fetchErrs)
	require.Len(t, startedAt, 1, "the second App took one of the two partitions")
	var moved int32
	for p := range startedAt {
		moved = p
	}
	before := l.offsets("first")[moved]
	after := l.offsets("second")[moved]
	require.NotEmpty(t, before)
	require.Equal(t, before[len(before)-1]+1, startedAt[moved],
		"the first App's last record of the moved partition was committed before the second App began")
	require.Equal(t, startedAt[moved], after[0], "the second App began at the committed offset")
	require.Equal(t, offsets(n), append(slices.Clone(before), after...), "no record of the moved partition was processed twice or skipped")
	kept := 1 - moved
	require.Equal(t, offsets(n), l.offsets("first")[kept], "the first App processed the partition it kept once, in order")
	require.Empty(t, l.offsets("second")[kept], "the second App processed nothing of the partition the first App kept")
	require.Equal(t, map[int32]int64{0: n, 1: n}, committedTo(t, adm, group, topic))

	// The revoke of the moved partition alone began while the first App
	// was processing its last record of it, and that record finished.
	var revokedAt time.Time
	for _, r := range records(tel.Logs(), "partitions revoked") {
		if attrs(r)["messaging.destination.partition.ids"] == "["+strconv.Itoa(int(moved))+"]" {
			revokedAt = r.Timestamp()
			break
		}
	}
	require.False(t, revokedAt.IsZero(), "the first App logged the revoke of the moved partition alone")
	var last processing
	for _, r := range l.of("first") {
		if r.partition == moved {
			last = r
		}
	}
	require.True(t, last.start.Before(revokedAt) && last.end.After(revokedAt),
		"offset %d was in flight when the revoke began (%v), and finished after it (%v to %v)", last.offset, revokedAt, last.start, last.end)
}

func TestKafka_a_member_whose_revoke_outlasts_RebalanceTimeout_is_fenced(t *testing.T) {
	// kfake is told when to fence a member: a test answers its heartbeat
	// with UNKNOWN_MEMBER_ID, and franz-go reports the partitions lost.
	// Here Kafka's coordinator decides. The first App is still
	// heartbeating, but stuck in a revoke, when another rebalance starts;
	// Kafka waits RebalanceTimeout for it to rejoin, then drops it and
	// gives all its partitions away while its records are still in
	// flight. franz-go learns it was fenced from the refused commit, not
	// a heartbeat, and rejoins as a new member: the partition it still
	// held comes back from that rejoin as revoked rather than lost.
	t.Parallel()

	const partitions = 2
	adm := admin(t)
	topic, group := newTopic(t, adm, partitions), unique("group")
	produceAt(t, topic, map[int32]int{0: 1, 1: 1})

	s := realSettings(t, group, topic)
	s.RebalanceTimeout = 5 * time.Second
	// The fenced member keeps heartbeating, and its session outlasts the
	// test's rebalances, so only RebalanceTimeout can remove it.
	s.SessionTimeout = 45 * time.Second

	// The first App's records stay in flight until the test releases
	// them, after the other Apps have processed both: far past
	// RebalanceTimeout.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	l := newLedger()
	entered := make(chan int32, partitions)
	tel := humustest.NewTelemetry()
	first, firstStatus := startOnOS(t, s, l.processor("first", func(ctx context.Context, m kafka.Message) error {
		entered <- m.Partition
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}), humus.WithTelemetry(tel.Builder()))
	for range partitions {
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("the first App did not begin a record of each partition")
		}
	}

	// The second App's join moves one partition, whose revoke waits for
	// the record in flight.
	second, secondStatus := startOnOS(t, s, l.processor("second", nil))
	eventually(t, 30*time.Second, func() bool { return len(records(tel.Logs(), "partitions revoked")) > 0 },
		"the first App begins revoking a partition")
	moved := attrs(records(tel.Logs(), "partitions revoked")[0])["messaging.destination.partition.ids"]
	require.Contains(t, []string{"[0]", "[1]"}, moved, "the first App revoked one partition")
	kept := "[0]" // the partition the first App still held when it was fenced
	if moved == "[0]" {
		kept = "[1]"
	}

	// The third App's join starts the rebalance the first App can't
	// rejoin while its revoke waits.
	third, thirdStatus := startOnOS(t, s, l.processor("third", nil))
	eventually(t, time.Minute, func() bool { return len(l.of("second"))+len(l.of("third")) == partitions },
		"both partitions are processed again by the other Apps while the first App's records are in flight")
	require.Empty(t, l.of("first"), "the first App's records are still in flight")
	releaseAll()

	eventually(t, time.Minute, func() bool { return len(l.of("first")) == partitions }, "the first App's records in flight finish")
	eventually(t, time.Minute, func() bool { return gaveUp(tel, kept) }, "the first App gives up the partition it kept, %s", kept)
	require.True(t, commitRefused(tel), "Kafka refused the fenced member's commit")

	for _, app := range []struct {
		p      *bedrocktest.Process
		status <-chan bedrock.ExitStatus
	}{{first, firstStatus}, {second, secondStatus}, {third, thirdStatus}} {
		st := terminate(t, app.p, app.status)
		require.Zero(t, st.Code(), "the refused commit is not an error: %v", st.Cause())
	}

	require.Equal(t, map[int32][]int64{0: {0}, 1: {0}}, l.offsets("first"))
	again := map[int32][]int64{}
	for _, app := range []string{"second", "third"} {
		for p, o := range l.offsets(app) {
			again[p] = append(again[p], o...)
		}
	}
	require.Equal(t, map[int32][]int64{0: {0}, 1: {0}}, again, "each record in flight was processed again, once, by its partition's new owner")
	require.Equal(t, map[int32]int64{0: 1, 1: 1}, committedTo(t, adm, group, topic))
}

// gates holds a fenced App's records, by partition and offset, until a test
// releases them, and reports each record held as it begins.
type gates struct {
	held    map[at]chan struct{}
	once    map[at]*sync.Once
	entered chan at
}

// at names one record of a partition.
type at struct {
	partition int32
	offset    int64
}

func newGates(records ...at) *gates {
	g := &gates{held: map[at]chan struct{}{}, once: map[at]*sync.Once{}, entered: make(chan at, len(records))}
	for _, r := range records {
		g.held[r] = make(chan struct{})
		g.once[r] = new(sync.Once)
	}
	return g
}

// work holds m until it is released, if it is one of g's records.
func (g *gates) work(ctx context.Context, m kafka.Message) error {
	gate, ok := g.held[at{m.Partition, m.Offset}]
	if !ok {
		return nil
	}
	g.entered <- at{m.Partition, m.Offset}
	select {
	case <-gate:
	case <-ctx.Done():
	}
	return nil
}

// release lets r finish.
func (g *gates) release(r at) {
	g.once[r].Do(func() { close(g.held[r]) })
}

// releaseAll lets every record finish.
func (g *gates) releaseAll() {
	for r := range g.held {
		g.release(r)
	}
}

// await fails the test unless the App begins each of rs, in any order,
// within d.
func (g *gates) await(t *testing.T, d time.Duration, rs ...at) {
	t.Helper()
	want := map[at]bool{}
	for _, r := range rs {
		want[r] = true
	}
	deadline := time.After(d)
	for len(want) > 0 {
		select {
		case r := <-g.entered:
			delete(want, r)
		case <-deadline:
			t.Fatalf("the App did not begin %v within %v", want, d)
		}
	}
}

// fence runs the fence of TestKafka_a_member_whose_revoke_outlasts_RebalanceTimeout_is_fenced
// on a topic of two partitions with one record each: the first App, which
// holds g's records, is stuck revoking one partition when the third App's
// join starts another rebalance, and Kafka drops it and gives both
// partitions to the second App and the third. It returns the partition the
// first App revoked, the one it still held, and a function per App that
// terminates it. Under AtLeastOnce it returns once the other Apps have
// processed both records again; under AtMostOnce the records were committed
// before the first App processed them, so the other Apps have nothing to
// process.
func fence(t *testing.T, s kafka.Settings, l *ledger, g *gates, tel *humustest.Telemetry) (moved, kept int32, apps []func() bedrock.ExitStatus) {
	t.Helper()
	topic := s.Topics[0]
	produceAt(t, topic, map[int32]int{0: 1, 1: 1})

	run := func(name string, work func(context.Context, kafka.Message) error, opts ...humus.Option) {
		p, status := startOnOS(t, s, l.processor(name, work), opts...)
		apps = append(apps, func() bedrock.ExitStatus { return terminate(t, p, status) })
	}
	run("first", g.work, humus.WithTelemetry(tel.Builder()))
	g.await(t, 30*time.Second, at{0, 0}, at{1, 0})

	// The second App's join moves one partition, whose revoke waits for
	// the record in flight.
	run("second", nil)
	eventually(t, 30*time.Second, func() bool { return len(records(tel.Logs(), "partitions revoked")) > 0 },
		"the first App begins revoking a partition")
	ids := attrs(records(tel.Logs(), "partitions revoked")[0])["messaging.destination.partition.ids"]
	require.Contains(t, []string{"[0]", "[1]"}, ids, "the first App revoked one partition")
	moved, kept = 0, 1
	if ids == "[1]" {
		moved, kept = 1, 0
	}

	// The third App's join starts the rebalance the first App can't rejoin
	// while its revoke waits.
	run("third", nil)
	if s.Delivery == queue.AtLeastOnce {
		eventually(t, time.Minute, func() bool { return len(l.of("second"))+len(l.of("third")) == 2 },
			"both partitions are processed again by the other Apps while the first App's records are in flight")
	}
	require.Empty(t, l.of("first"), "the first App's records are still in flight")
	return moved, kept, apps
}

// owner returns the App, other than the first, that processed partition p.
func owner(l *ledger, p int32) string {
	for _, app := range []string{"second", "third"} {
		if len(l.offsets(app)[p]) > 0 {
			return app
		}
	}
	return ""
}

// logged returns how many messages containing body franz-go logged, in the
// App that recorded tel.
func logged(tel *humustest.Telemetry, body string) int {
	n := 0
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == "github.com/twmb/franz-go/pkg/kgo" && strings.Contains(r.Body().AsString(), body) {
			n++
		}
	}
	return n
}

func TestKafka_at_least_once_a_fenced_member_does_not_rewind_the_new_owners_commit(t *testing.T) {
	// kfake can fence a member the same way, and its unit test shows the
	// commit isn't sent. Here Kafka's own coordinator fences the member,
	// answers its rejoin with UNKNOWN_MEMBER_ID, so that franz-go rejoins
	// as a new member, and would accept a commit from that member for a
	// partition another member owns: the check is all that stands between
	// the revoke's commit and __consumer_offsets. A fresh member then
	// resumes from the offset Kafka reads back from there.
	t.Parallel()

	adm := admin(t)
	topic, group := newTopic(t, adm, 2), unique("group")
	s := realSettings(t, group, topic)
	s.RebalanceTimeout = 5 * time.Second
	// The fenced member keeps heartbeating, and its session outlasts the
	// test's rebalances, so only RebalanceTimeout can remove it.
	s.SessionTimeout = 45 * time.Second

	// The first App holds each partition's offset 0. It holds offset 1 of
	// each too, in case the partition it keeps polls the records after the
	// fence before it gives the partition up.
	g := newGates(at{0, 0}, at{1, 0}, at{0, 1}, at{1, 1})
	t.Cleanup(g.releaseAll)
	l := newLedger()
	tel := humustest.NewTelemetry()
	moved, kept, apps := fence(t, s, l, g, tel)

	// The kept partition's new owner processes ten more records and
	// commits past them, while the first App has offset 0 in flight.
	produceAt(t, topic, map[int32]int{kept: 10}) // offsets 1 to 10
	newOwner := owner(l, kept)
	eventually(t, 30*time.Second, func() bool { return len(l.offsets(newOwner)[kept]) == 11 },
		"the new owner processes offsets 0 to 10 of partition %d", kept)
	eventually(t, 30*time.Second, func() bool { return committedTo(t, adm, group, topic)[kept] == 11 },
		"the new owner commits offset 11 of partition %d", kept)

	// The first App's record of the kept partition finishes, and Kafka
	// refuses its commit: the member was fenced out. Offset 0 stays
	// pending.
	g.release(at{kept, 0})
	eventually(t, 30*time.Second, func() bool { return commitRefused(tel) }, "Kafka refuses the fenced member's commit")

	// The moved partition's record finishes and its revoke ends. franz-go
	// rejoins as a new member, which the new assignment gives nothing, and
	// revokes the kept partition, whose revoke commits what it has pending:
	// an offset below the 11 the new owner committed.
	g.release(at{moved, 0})
	eventually(t, time.Minute, func() bool { return gaveUp(tel, "["+strconv.Itoa(int(kept))+"]") },
		"the first App gives up the partition it kept, %d", kept)
	require.Positive(t, logged(tel, "rejoining without a member id"), "franz-go rejoined as a new member")
	g.releaseAll()

	st := apps[0]()
	require.Zero(t, st.Code(), "the refused commit is not an error: %v", st.Cause())
	first := l.offsets("first")
	require.Equal(t, []int64{0}, first[moved])
	require.Contains(t, [][]int64{{0}, {0, 1}}, first[kept], "the first App stopped the kept partition after its record in flight")
	require.Equal(t, int64(11), committedTo(t, adm, group, topic)[kept],
		"after the first App gave the partition up and exited, the committed offset is still the new owner's")
	for _, stop := range apps[1:] {
		st := stop()
		require.Zero(t, st.Code(), st.Cause())
	}
	require.Equal(t, map[int32]int64{moved: 1, kept: 11}, committedTo(t, adm, group, topic))

	// A fresh member processes only the records after the new owner's
	// commit.
	produceAt(t, topic, map[int32]int{kept: 1}) // offset 11
	p, status := startOnOS(t, s, l.processor("fresh", nil))
	eventually(t, 30*time.Second, func() bool { return len(l.of("fresh")) > 0 }, "the fresh member processes a record")
	st = terminate(t, p, status)
	require.Zero(t, st.Code(), st.Cause())
	require.Equal(t, map[int32][]int64{kept: {11}}, l.offsets("fresh"))
}

func TestKafka_at_most_once_a_fenced_member_does_not_commit_or_process_a_batch_polled_while_it_rejoins(t *testing.T) {
	// kfake can't show this. The commit is called while franz-go joins the
	// group, and waits for the join on a mutex, which synctest doesn't
	// count as durably blocked, so kfake's clock would stop. Here the
	// commit waits for Kafka's coordinator to answer the join, and then
	// goes out under the member ID the join returned, which Kafka would
	// accept for a partition another member owns.
	t.Parallel()

	adm := admin(t)
	topic, group := newTopic(t, adm, 2), unique("group")
	s := realSettings(t, group, topic)
	s.Delivery = queue.AtMostOnce
	s.RebalanceTimeout = 5 * time.Second
	s.SessionTimeout = 45 * time.Second

	g := newGates(at{0, 0}, at{1, 0})
	t.Cleanup(g.releaseAll)
	l := newLedger()
	tel := humustest.NewTelemetry()
	moved, kept, apps := fence(t, s, l, g, tel)

	// The kept partition's new owner commits and processes ten more
	// records. The first App polls them too, while it has offset 0 in
	// flight.
	produceAt(t, topic, map[int32]int{kept: 10}) // offsets 1 to 10
	var newOwner string
	eventually(t, time.Minute, func() bool {
		newOwner = owner(l, kept)
		return len(l.offsets(newOwner)[kept]) == 10
	}, "the new owner processes offsets 1 to 10 of partition %d", kept)
	require.Equal(t, int64(11), committedTo(t, adm, group, topic)[kept])
	time.Sleep(time.Second) // the first App's fetch returns the ten records

	// The moved partition's record finishes and its revoke ends, so
	// franz-go rejoins. Once it is joining, the kept partition's record
	// finishes, and the partition calls the commit of the batch the first
	// App polled after the fence: offsets 1 to 10.
	joins := logged(tel, "blocking commits from join&sync")
	g.release(at{moved, 0})
	eventually(t, 30*time.Second, func() bool { return logged(tel, "blocking commits from join&sync") > joins },
		"franz-go begins to rejoin")
	g.release(at{kept, 0})

	eventually(t, time.Minute, func() bool { return gaveUp(tel, "["+strconv.Itoa(int(kept))+"]") },
		"the first App gives up the partition it kept, %d", kept)
	require.Positive(t, logged(tel, "rejoining without a member id"), "franz-go rejoined as a new member")
	skipped := records(tel.Logs(), "batch skipped: its commit was refused")
	require.Len(t, skipped, 1, "the batch polled while franz-go rejoined is skipped")
	a := attrs(skipped[0])
	require.Equal(t, strconv.Itoa(int(kept)), a["messaging.destination.partition.id"])
	require.Equal(t, "1", a["messaging.kafka.offset.first"])
	require.Equal(t, "10", a["messaging.kafka.offset.last"])
	require.Equal(t, "WARN", skipped[0].Severity().String())

	for _, stop := range apps {
		st := stop()
		require.Zero(t, st.Code(), st.Cause())
	}
	require.Equal(t, map[int32][]int64{moved: {0}, kept: {0}}, l.offsets("first"),
		"the first App processed only the records it had in flight when it was fenced")
	require.Equal(t, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, l.offsets(newOwner)[kept])
	for _, o := range l.offsets(newOwner)[kept] {
		require.NotContains(t, l.offsets("first")[kept], o, "offset %d of partition %d was processed by both members", o, kept)
	}
	require.Equal(t, map[int32]int64{moved: 1, kept: 11}, committedTo(t, adm, group, topic))
}

// gaveUp reports whether the App that recorded tel logged giving up
// exactly the partitions ids names, as a revoke or as a loss.
//
// Against Kafka, a fenced member gives up the partition it still held as a
// revoke: franz-go rejoins as a new member, and the new assignment no longer
// has it. So the Service's OnPartitionsLost path is not exercised against
// Kafka here; kfake's tests cover it.
func gaveUp(tel *humustest.Telemetry, ids string) bool {
	for _, body := range []string{"partitions revoked", "partitions lost"} {
		for _, r := range records(tel.Logs(), body) {
			if attrs(r)["messaging.destination.partition.ids"] == ids {
				return true
			}
		}
	}
	return false
}

// commitRefused reports whether franz-go, in the App that recorded tel,
// logged a commit refused because the coordinator no longer knew the
// member, or no longer had its generation. It matches franz-go's own log
// lines, such as "offset commit returned a fatal group member error,
// triggering rejoin" with err UNKNOWN_MEMBER_ID, so a franz-go upgrade that
// rewords them fails this check without the Service having changed.
func commitRefused(tel *humustest.Telemetry) bool {
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name != "github.com/twmb/franz-go/pkg/kgo" || !strings.Contains(r.Body().AsString(), "commit") {
			continue
		}
		for _, v := range attrs(r) {
			if strings.Contains(v, "UNKNOWN_MEMBER_ID") || strings.Contains(v, "ILLEGAL_GENERATION") {
				return true
			}
		}
	}
	return false
}

func TestKafka_after_SIGTERM_the_other_member_takes_the_partitions_at_once(t *testing.T) {
	// kfake's coordinator handles LeaveGroup itself, and its session
	// timeout runs on synctest's clock. Here the Service's LeaveGroup
	// reaches Kafka's coordinator, which rebalances at once, with a real
	// session timeout far longer than the handover takes.
	t.Parallel()

	const partitions = 2
	adm := admin(t)
	topic, group := newTopic(t, adm, partitions), unique("group")
	produceAt(t, topic, map[int32]int{0: 1, 1: 1})

	s := realSettings(t, group, topic)
	s.SessionTimeout = 90 * time.Second

	l := newLedger()
	tel1, tel2 := humustest.NewTelemetry(), humustest.NewTelemetry()
	first, firstStatus := startOnOS(t, s, l.processor("first", nil), humus.WithTelemetry(tel1.Builder()))
	eventually(t, 30*time.Second, func() bool { return len(l.of("first")) == partitions }, "the first App processes both partitions")

	second, secondStatus := startOnOS(t, s, l.processor("second", nil), humus.WithTelemetry(tel2.Builder()))
	eventually(t, 30*time.Second, func() bool { return len(records(tel2.Logs(), "partitions assigned")) > 0 },
		"the second App is assigned a partition")
	assigned := attrs(records(tel2.Logs(), "partitions assigned")[0])["messaging.destination.partition.ids"]
	require.Contains(t, []string{"[0]", "[1]"}, assigned, "the second App took one partition")
	var kept int32 // the partition the first App kept
	if assigned == "[0]" {
		kept = 1
	}

	st := terminate(t, first, firstStatus)
	require.Zero(t, st.Code(), st.Cause())
	left := time.Now()

	produceAt(t, topic, map[int32]int{0: 1, 1: 1}) // offset 1 of each partition
	eventually(t, s.SessionTimeout/3, func() bool {
		for _, r := range l.of("second") {
			if r.partition == kept && r.offset == 1 {
				return true
			}
		}
		return false
	}, "the second App takes partition %d well within the %v session timeout", kept, s.SessionTimeout)
	require.Less(t, time.Since(left), s.SessionTimeout/3)

	st = terminate(t, second, secondStatus)
	require.Zero(t, st.Code(), st.Cause())
	require.Equal(t, map[int32]int64{0: 2, 1: 2}, committedTo(t, adm, group, topic))
}

func TestKafka_an_idempotent_Producer_with_acks_all_produces_what_the_Service_consumes(t *testing.T) {
	// kfake accepts an idempotent produce request, but the producer ID, the
	// epoch and the sequence numbers it checks are its own. Here the
	// Producer, with DefaultProducerSettings and so with franz-go's
	// idempotence and acks=all, gets its producer ID and epoch from Kafka's
	// InitProducerId, Kafka checks each batch's sequence numbers, and the
	// Service consumes the records back, each under a process span that is
	// the child of the record's send span.
	t.Parallel()

	const n = 10
	adm := admin(t)
	// More partitions than one, so that one key keeping the records on one
	// partition, in order, is the key's doing.
	topic, group := newTopic(t, adm, 3), unique("group")

	ps := kafka.DefaultProducerSettings()
	ps.Brokers = brokers(t)
	records := make([]kafka.Record, n)
	for i := range records {
		records[i] = kafka.Record{Topic: topic, Key: []byte("one key"), Value: []byte(strconv.Itoa(i))}
	}
	produced := humustest.NewTelemetry()
	_, status := humustest.Start(t, produces(ps, func(ctx context.Context, p *kafka.Producer) error {
		return p.Produce(ctx, records...)
	}), humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(produced.Builder())))
	st := exited(t, status)
	require.Zero(t, st.Code(), "the Producer's App: %v", st.Cause())

	var (
		mu       sync.Mutex
		messages []kafka.Message
	)
	consumed := humustest.NewTelemetry()
	p, status := startOnOS(t, realSettings(t, group, topic), func(_ context.Context, m kafka.Message) error {
		mu.Lock()
		defer mu.Unlock()
		messages = append(messages, m)
		return nil
	}, humus.WithTelemetry(consumed.Builder()))
	eventually(t, 30*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(messages) == n
	}, "the Service consumes the %d records", n)
	st = terminate(t, p, status)
	require.Zero(t, st.Code(), st.Cause())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, messages, n)
	for i, m := range messages {
		require.Equal(t, messages[0].Partition, m.Partition, "every record is on one partition")
		require.Equal(t, int64(i), m.Offset)
		require.Equal(t, "one key", string(m.Key))
		require.Equal(t, strconv.Itoa(i), string(m.Value), "the records are consumed in the order they were produced")
	}
	require.Equal(t, map[int32]int64{messages[0].Partition: n}, committedTo(t, adm, group, topic))

	// Kafka stored the records as an idempotent producer's: under the
	// producer ID and epoch it handed out, where a producer without
	// idempotence writes -1.
	stored := readTopic(t, topic, n)
	require.Len(t, stored, n)
	for _, r := range stored {
		require.GreaterOrEqual(t, r.ProducerID, int64(0), "offset %d was produced with a producer ID", r.Offset)
		require.Equal(t, stored[0].ProducerID, r.ProducerID, "one producer ID for every record of the Producer")
		require.GreaterOrEqual(t, r.ProducerEpoch, int16(0), "offset %d was produced with an epoch", r.Offset)
	}

	sends := spansNamed(produced, "send "+topic)
	processes := spansNamed(consumed, "process "+topic)
	require.Len(t, sends, n)
	require.Len(t, processes, n)
	for i := range int64(n) {
		send, process := spanAt(t, sends, i), spanAt(t, processes, i)
		require.True(t, process.Parent().IsRemote(), "offset %d's process span has its parent from the record's headers", i)
		require.Equal(t, send.SpanContext().TraceID(), process.Parent().TraceID(), "offset %d", i)
		require.Equal(t, send.SpanContext().SpanID(), process.Parent().SpanID(), "offset %d's process span is the child of its send span", i)
	}
}

// readTopic returns the first n records of topic, polling Kafka for them
// for at most 30 seconds.
func readTopic(t *testing.T, topic string, n int) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers(t)...),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	require.NoError(t, err)
	defer cl.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var out []*kgo.Record
	for len(out) < n {
		fs := cl.PollFetches(ctx)
		require.NoError(t, fs.Err(), "reading %d records of %s back", n, topic)
		out = append(out, fs.Records()...)
	}
	return out
}
