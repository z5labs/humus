// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/queue/kafka"
	"github.com/z5labs/humus/telemetry"
)

// producerSettings is DefaultProducerSettings with brokers.
func producerSettings(brokers ...string) kafka.ProducerSettings {
	ps := kafka.DefaultProducerSettings()
	ps.Brokers = brokers
	return ps
}

// produces returns an Init that builds a Producer with settings, and whose
// only Service is a job that calls run with it.
func produces(settings kafka.ProducerSettings, run func(ctx context.Context, p *kafka.Producer) error) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		p, err := kafka.NewProducer(s, settings)
		if err != nil {
			return nil, err
		}
		return []humus.Service{job.NewService(s, func(ctx context.Context) error {
			return run(ctx, p)
		})}, nil
	}
}

// readAll returns every record of topic, in each partition's order,
// through a client that dials h with opts. It polls until a poll finds
// nothing new for a second, which inside a bubble takes no real time.
func readAll(t *testing.T, h *humustest.Host, c *kfake.Cluster, topic string, opts ...kgo.Opt) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(append([]kgo.Opt{
		kgo.SeedBrokers(c.ListenAddrs()...),
		kgo.Dialer(h.Dial),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	}, opts...)...)
	require.NoError(t, err)
	defer cl.Close()

	var out []*kgo.Record
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		fetches := cl.PollFetches(ctx)
		cancel()
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

// holdProduce makes the cluster hold every Produce request until release is
// closed, after which it handles them as it would have. Each held request
// is announced on the returned channel, which is never blocked on. release
// is closed when the test ends, if the test hasn't, before the cluster
// closes.
func holdProduce(t *testing.T, c *kfake.Cluster) (held <-chan struct{}, release func()) {
	t.Helper()
	ch := make(chan struct{}, 64)
	released := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	c.ControlKey(kmsg.Produce.Int16(), func(kmsg.Request) (kmsg.Response, error, bool) {
		c.KeepControl()
		select {
		case <-released:
			return nil, nil, false
		default:
		}
		select {
		case ch <- struct{}{}:
		default:
		}
		c.SleepControl(func() { <-released })
		return nil, nil, false
	})
	return ch, release
}

// value returns the record with Topic topic, and i as its value.
func value(i int) kafka.Record {
	return kafka.Record{Topic: topic, Value: []byte(strconv.Itoa(i))}
}

func TestProduce_stores_the_records_of_a_call(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		tel := humustest.NewTelemetry()
		var err error
		_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx,
				kafka.Record{Topic: topic, Key: []byte("key-0"), Value: []byte("0"), Headers: []kafka.Header{{Key: "h", Value: []byte("v")}}},
				value(1),
			)
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.NoError(t, err)

		got := readAll(t, h, c, topic)
		require.Len(t, got, 2)
		require.Equal(t, "key-0", string(got[0].Key))
		require.Equal(t, "0", string(got[0].Value))
		require.Len(t, got[0].Headers, 2, "the record's header, then the span's context")
		require.Equal(t, kgo.RecordHeader{Key: "h", Value: []byte("v")}, got[0].Headers[0])
		require.Equal(t, "traceparent", got[0].Headers[1].Key)
		require.Nil(t, got[1].Key)
		require.Equal(t, "1", string(got[1].Value))

		var kgoLogs int
		for _, r := range tel.Logs() {
			if r.InstrumentationScope().Name == "github.com/twmb/franz-go/pkg/kgo" {
				kgoLogs++
			}
		}
		require.NotZero(t, kgoLogs, "franz-go logs through the Setup's LoggerProvider")
	})
}

func TestProduce_with_no_records_returns_nil(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		dialled := false
		c.ControlKey(-1, func(kmsg.Request) (kmsg.Response, error, bool) {
			c.KeepControl()
			dialled = true
			return nil, nil, false
		})

		var err error
		_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx)
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.NoError(t, err)
		require.False(t, dialled, "a call with no records sent something")
	})
}

func TestProduce_keeps_the_order_of_records_with_one_key(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 3)

		records := make([]kafka.Record, 10)
		for i := range records {
			records[i] = kafka.Record{Topic: topic, Key: []byte("one key"), Value: []byte(strconv.Itoa(i))}
		}
		var err error
		_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx, records...)
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.NoError(t, err)

		got := readAll(t, h, c, topic)
		require.Len(t, got, 10)
		for i, r := range got {
			require.Equal(t, got[0].Partition, r.Partition, "every record is on one partition")
			require.Equal(t, strconv.Itoa(i), string(r.Value))
			require.Equal(t, int64(i), r.Offset)
		}
	})
}

func TestProduce_with_a_done_ctx_sends_nothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produced := false
		c.ControlKey(kmsg.Produce.Int16(), func(kmsg.Request) (kmsg.Response, error, bool) {
			c.KeepControl()
			produced = true
			return nil, nil, false
		})

		var err error
		var took time.Duration
		_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			start := time.Now()
			err = p.Produce(ctx, value(0))
			took = time.Since(start)
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, took, "Produce returned at once")
		require.False(t, produced, "a call whose ctx was done sent a produce request")
		require.Empty(t, readAll(t, h, c, topic))
	})
}

func TestProduce_rejects_a_record_with_no_topic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		var err error
		_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx, value(0), kafka.Record{Value: []byte("no topic")})
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.EqualError(t, err, "kafka: producing: record 1 has no Topic")
		require.Empty(t, readAll(t, h, c, topic), "nothing of the call is stored")
	})
}

func TestProduce_returns_when_ctx_is_done_and_OnStop_flushes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		_, release := holdProduce(t, c)

		var err error
		var took time.Duration
		_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			start := time.Now()
			err = p.Produce(ctx, kafka.Record{Topic: topic, Value: []byte("held")})
			took = time.Since(start)
			release() // the cluster answers once the call has given up
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Zero(t, st.Code(), "OnStop's flush waited for the record; cause: %v", st.Cause())
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, 2*time.Second, took)

		got := readAll(t, h, c, topic)
		require.Len(t, got, 1, "it was stored all the same")
		require.Equal(t, "held", string(got[0].Value))
	})
}

func TestProduce_DeliveryTimeout_bounds_a_call_against_a_hung_broker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		holdProduce(t, c) // released when the test ends

		var err error
		var took time.Duration
		_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(_ context.Context, p *kafka.Producer) error {
			start := time.Now()
			err = p.Produce(context.Background(), value(0))
			took = time.Since(start)
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "kafka: producing to orders, partition 0: ")
		require.Equal(t, kafka.DefaultProducerSettings().DeliveryTimeout, took)
	})
}

func TestProduce_a_caller_giving_up_does_not_fail_another_callers_records(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		held, release := holdProduce(t, c)

		var (
			first, second           error
			firstTook, secondReturn time.Duration
		)
		_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			start := time.Now()
			var wg sync.WaitGroup
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				first = p.Produce(ctx, value(0))
				firstTook = time.Since(start)
			})
			<-held // the first call's request is held
			wg.Go(func() {
				second = p.Produce(ctx, value(1))
				secondReturn = time.Since(start)
			})
			time.Sleep(3 * time.Second)
			release()
			wg.Wait()
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.ErrorIs(t, first, context.DeadlineExceeded)
		require.Equal(t, time.Second, firstTook)
		require.NoError(t, second)
		require.GreaterOrEqual(t, secondReturn, 3*time.Second, "the second call succeeded once the broker answered")
		require.Less(t, secondReturn, kafka.DefaultProducerSettings().DeliveryTimeout)
		require.Len(t, readAll(t, h, c, topic), 2, "both calls' records were stored")
	})
}

func TestProducer_grace_period_cuts_the_OnStop_flush_short(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		held, _ := holdProduce(t, c) // released when the test ends

		produced := make(chan error, 1)
		_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(_ context.Context, p *kafka.Producer) error {
			// A goroutine the job leaves running, whose record the broker
			// holds.
			go func() { produced <- p.Produce(context.Background(), value(0)) }()
			<-held
			return nil
		}),
			humustest.WithApp(humus.WithHost(h.Host())),
			humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s"}),
		)

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		require.True(t, ok, "the cause is a *humus.GracePeriodError: %v", st.Cause())
		require.Equal(t, 2*time.Second, gerr.GracePeriod)
		require.ErrorIs(t, st.Cause(), context.DeadlineExceeded)
		require.ErrorContains(t, st.Cause(), "kafka: flushing the Producer")

		err := <-produced
		require.ErrorIs(t, err, kgo.ErrClientClosed, "Close failed the held record")
	})
}

func TestProducer_after_Close_Produce_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		after := make(chan struct{})
		produced := make(chan error, 1)
		var took time.Duration
		_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(_ context.Context, p *kafka.Producer) error {
			// A goroutine the job leaves running, which produces after
			// the App has stopped.
			go func() {
				<-after
				start := time.Now()
				err := p.Produce(context.Background(), value(0))
				took = time.Since(start)
				produced <- err
			}()
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		close(after)
		err := <-produced
		require.ErrorIs(t, err, kgo.ErrClientClosed)
		require.Zero(t, took, "Produce returned at once")
		require.Empty(t, readAll(t, h, c, topic))
	})
}

func TestNewProducer_Init_failing_after_it_closes_the_client(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		bad := errors.New("init failed after NewProducer")
		var pr *kafka.Producer
		_, status := humustest.Start(t, func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			var err error
			pr, err = kafka.NewProducer(s, producerSettings(c.ListenAddrs()...))
			if err != nil {
				return nil, err
			}
			// The client connects and starts its goroutines.
			if err := pr.Produce(ctx, value(0)); err != nil {
				return nil, err
			}
			return nil, bad
		}, humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.ErrorIs(t, st.Cause(), bad)
		require.NotContains(t, st.Cause().Error(), "on stop", "the OnStop function flushed and closed the client")
		require.ErrorIs(t, pr.Produce(context.Background(), value(1)), kgo.ErrClientClosed)
		require.Len(t, readAll(t, h, c, topic), 1)
		// synctest.Test fails if any of franz-go's goroutines is left
		// when the bubble ends.
	})
}

func TestNewProducer(t *testing.T) {
	t.Parallel()

	setup := func() *humus.Setup { return &humus.Setup{Host: humus.OSHost(), Telemetry: telemetry.Noop()} }

	t.Run("reports every bad field, naming each", func(t *testing.T) {
		t.Parallel()

		ps := kafka.DefaultProducerSettings()
		ps.DeliveryTimeout = 0
		_, err := kafka.NewProducer(setup(), ps)
		require.ErrorContains(t, err, "ProducerSettings.Brokers must list at least one")
		require.ErrorContains(t, err, "ProducerSettings.DeliveryTimeout must be positive")
	})

	t.Run("names each bad broker", func(t *testing.T) {
		t.Parallel()

		_, err := kafka.NewProducer(setup(), producerSettings("kafka-0:9092", "kafka-1"))
		require.ErrorContains(t, err, `ProducerSettings.Brokers "kafka-1" is not host:port`)
	})

	t.Run("accepts DefaultProducerSettings with Brokers set", func(t *testing.T) {
		t.Parallel()

		s := setup()
		p, err := kafka.NewProducer(s, producerSettings("127.0.0.1:9092"))
		require.NoError(t, err)
		require.NotNil(t, p)
		kafka.CloseProducer(p)
	})

	t.Run("opens no connection", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			dialled := false
			c.ControlKey(-1, func(kmsg.Request) (kmsg.Response, error, bool) {
				c.KeepControl()
				dialled = true
				return nil, nil, false
			})
			bad := errors.New("init failed after NewProducer")
			_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				if _, err := kafka.NewProducer(s, producerSettings(c.ListenAddrs()...)); err != nil {
					return nil, err
				}
				return nil, bad
			}, humustest.WithApp(humus.WithHost(h.Host())))

			st := <-status
			require.ErrorIs(t, st.Cause(), bad)
			synctest.Wait()
			require.False(t, dialled, "NewProducer opened a connection")
		})
	})
}
