// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

// topic is the topic every test consumes, and group the consumer group.
const (
	topic = "orders"
	group = "orders-consumer"
)

// cluster returns a kfake cluster listening on h, with topic seeded with
// the given number of partitions. It is closed when the test ends, inside
// the bubble that built it.
func cluster(t *testing.T, h *humustest.Host, partitions int32, opts ...kfake.Opt) *kfake.Cluster {
	t.Helper()
	all := append([]kfake.Opt{
		kfake.ListenFn(h.Listen),
		kfake.NumBrokers(1),
		kfake.SeedTopics(partitions, topic),
	}, opts...)
	c, err := kfake.NewCluster(all...)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

// settings is DefaultSettings with brokers, the group and the topic.
func settings(brokers ...string) kafka.Settings {
	s := kafka.DefaultSettings()
	s.Brokers = brokers
	s.GroupID = group
	s.Topics = []string{topic}
	return s
}

// produce writes n records to partition 0 of topic, with values "0" to
// "n-1", through a client that dials h with opts.
func produce(t *testing.T, h *humustest.Host, c *kfake.Cluster, n int, opts ...kgo.Opt) {
	t.Helper()
	produceTo(t, h, c, map[int32]int{0: n}, opts...)
}

// produceTo writes counts[p] records to each partition p of topic.
func produceTo(t *testing.T, h *humustest.Host, c *kfake.Cluster, counts map[int32]int, opts ...kgo.Opt) {
	t.Helper()
	all := append([]kgo.Opt{
		kgo.SeedBrokers(c.ListenAddrs()...),
		kgo.Dialer(h.Dial),
		kgo.RecordPartitioner(kgo.ManualPartitioner()),
	}, opts...)
	cl, err := kgo.NewClient(all...)
	require.NoError(t, err)
	defer cl.Close()

	var records []*kgo.Record
	for p, n := range counts {
		for i := range n {
			records = append(records, &kgo.Record{
				Topic:     topic,
				Partition: p,
				Key:       []byte("key-" + strconv.Itoa(i)),
				Value:     []byte(strconv.Itoa(i)),
			})
		}
	}
	require.NoError(t, cl.ProduceSync(context.Background(), records...).FirstErr())
}

// committed returns the group's committed offset of each partition of
// topic.
func committed(c *kfake.Cluster) map[int32]int64 {
	offsets := make(map[int32]int64)
	info := c.GroupInfo(group)
	if info == nil {
		return offsets
	}
	for p, commit := range info.Commits[topic] {
		offsets[p] = commit.Offset
	}
	return offsets
}

// members returns how many members the group has.
func members(c *kfake.Cluster) int {
	info := c.GroupInfo(group)
	if info == nil {
		return 0
	}
	return len(info.Members)
}

// consumes returns an Init whose only Service consumes settings with p.
func consumes(settings kafka.Settings, p queue.ProcessorFunc[kafka.Message]) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := kafka.NewService(s, settings, p)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
}

// tlsDial returns a kgo dialer that dials h and runs a TLS handshake with
// cfg over the connection.
func tlsDial(h *humustest.Host, cfg *tls.Config) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := h.Dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		c := cfg.Clone()
		c.ServerName, _, _ = net.SplitHostPort(address)
		tc := tls.Client(conn, c)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return tc, nil
	}
}

// answerOffsetCommit makes the cluster answer the next OffsetCommit with
// code on every partition, or every one if keep is true.
func answerOffsetCommit(c *kfake.Cluster, code int16, keep bool) {
	c.ControlKey(kmsg.OffsetCommit.Int16(), func(req kmsg.Request) (kmsg.Response, error, bool) {
		if keep {
			c.KeepControl()
		}
		r := req.(*kmsg.OffsetCommitRequest)
		resp := r.ResponseKind().(*kmsg.OffsetCommitResponse)
		for _, rt := range r.Topics {
			st := kmsg.NewOffsetCommitResponseTopic()
			st.Topic = rt.Topic
			st.TopicID = rt.TopicID
			for _, rp := range rt.Partitions {
				sp := kmsg.NewOffsetCommitResponseTopicPartition()
				sp.Partition = rp.Partition
				sp.ErrorCode = code
				st.Partitions = append(st.Partitions, sp)
			}
			resp.Topics = append(resp.Topics, st)
		}
		return resp, nil, true
	})
}

// answerFetch makes the cluster answer every Fetch with code on every
// partition.
func answerFetch(c *kfake.Cluster, code int16) {
	c.ControlKey(kmsg.Fetch.Int16(), func(req kmsg.Request) (kmsg.Response, error, bool) {
		c.KeepControl()
		r := req.(*kmsg.FetchRequest)
		resp := r.ResponseKind().(*kmsg.FetchResponse)
		for _, rt := range r.Topics {
			st := kmsg.NewFetchResponseTopic()
			st.Topic = rt.Topic
			st.TopicID = rt.TopicID
			for _, rp := range rt.Partitions {
				sp := kmsg.NewFetchResponseTopicPartition()
				sp.Partition = rp.Partition
				sp.ErrorCode = code
				st.Partitions = append(st.Partitions, sp)
			}
			resp.Topics = append(resp.Topics, st)
		}
		return resp, nil, true
	})
}

// answerHeartbeat makes the cluster answer the next Heartbeat with code.
func answerHeartbeat(c *kfake.Cluster, code int16) {
	c.ControlKey(kmsg.Heartbeat.Int16(), func(req kmsg.Request) (kmsg.Response, error, bool) {
		resp := req.(*kmsg.HeartbeatRequest).ResponseKind().(*kmsg.HeartbeatResponse)
		resp.ErrorCode = code
		return resp, nil, true
	})
}

// records returns the log records emitted under the Service's scope with
// body.
func records(logs []sdklog.Record, body string) []sdklog.Record {
	var out []sdklog.Record
	for _, r := range logs {
		if r.InstrumentationScope().Name == "github.com/z5labs/humus/queue/kafka" && r.Body().AsString() == body {
			out = append(out, r)
		}
	}
	return out
}

// attrs returns the attributes of r, by key, as strings.
func attrs(r sdklog.Record) map[string]string {
	out := make(map[string]string)
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value.String()
		return true
	})
	return out
}

// refuseWrongPasswords makes the cluster answer a PLAIN authentication with
// any password but password the way Kafka does, with
// SASL_AUTHENTICATION_FAILED. kfake closes the connection instead, which a
// client can't tell from a broker going away.
func refuseWrongPasswords(c *kfake.Cluster, password string) {
	c.ControlKey(kmsg.SASLAuthenticate.Int16(), func(req kmsg.Request) (kmsg.Response, error, bool) {
		c.KeepControl()
		r := req.(*kmsg.SASLAuthenticateRequest)
		// PLAIN's message is authzid NUL authcid NUL passwd.
		fields := bytes.Split(r.SASLAuthBytes, []byte{0})
		if len(fields) == 3 && string(fields[2]) == password {
			return nil, nil, false
		}
		resp := r.ResponseKind().(*kmsg.SASLAuthenticateResponse)
		resp.ErrorCode = kerr.SaslAuthenticationFailed.Code
		msg := "Authentication failed: Invalid username or password"
		resp.ErrorMessage = &msg
		return resp, nil, true
	})
}
