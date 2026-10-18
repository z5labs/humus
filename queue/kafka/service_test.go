// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
	"github.com/z5labs/humus/telemetry"
)

func TestService_at_least_once_stops_on_a_failure_and_redelivers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 5) // offsets 0 to 4

		bad := errors.New("cannot process")
		run := func(fail bool) (bedrock.ExitStatus, []int64) {
			var seen []int64
			p, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(ctx context.Context, m kafka.Message) error {
				seen = append(seen, m.Offset)
				if fail && m.Offset == 3 {
					return bad
				}
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host())))
			if !fail {
				synctest.Wait() // every record is processed and committed
				p.Signal(syscall.SIGTERM)
			}
			return <-status, seen
		}

		st, seen := run(true)
		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), bad)
		require.ErrorContains(t, st.Cause(), "orders, partition 0, offset 3")
		require.Equal(t, []int64{0, 1, 2, 3}, seen)
		require.Equal(t, int64(3), committed(c)[0], "0 to 2 are committed, 3 is not")

		st, seen = run(false)
		require.Zero(t, st.Code(), st.Cause())
		require.Equal(t, []int64{3, 4}, seen, "3 is delivered again")
		require.Equal(t, int64(5), committed(c)[0])
	})
}

func TestService_SIGTERM_lets_the_record_in_flight_finish_and_commits_it(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 5)

		entered := make(chan struct{}, 5)
		finished := make(chan int64, 5)
		ctxErrs := make(chan error, 5)
		p, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(ctx context.Context, m kafka.Message) error {
			entered <- struct{}{}
			time.Sleep(3 * time.Second)
			ctxErrs <- ctx.Err()
			finished <- m.Offset
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))
		<-entered // offset 0 is in flight
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), st.Cause())
		require.NoError(t, <-ctxErrs, "Process's context is not cancelled when shutdown starts")
		require.Equal(t, int64(0), <-finished, "0 finished")
		require.Len(t, finished, 0, "1 to 4 never started")
		require.Equal(t, int64(1), committed(c)[0], "0 finished and is committed; 1 to 4 never started")
		require.Zero(t, members(c), "the Service left the group")
	})
}

func TestService_processes_and_commits_two_partitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 2)
		produceTo(t, h, c, map[int32]int{0: 5, 1: 5})

		var (
			mu        sync.Mutex
			seen      = map[int32][]int64{}
			inFlight  int
			maxFlight int
		)
		p, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(ctx context.Context, m kafka.Message) error {
			mu.Lock()
			inFlight++
			maxFlight = max(maxFlight, inFlight)
			mu.Unlock()
			time.Sleep(time.Second)
			mu.Lock()
			inFlight--
			seen[m.Partition] = append(seen[m.Partition], m.Offset)
			mu.Unlock()
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))
		synctest.Wait()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), st.Cause())
		require.Equal(t, map[int32][]int64{0: {0, 1, 2, 3, 4}, 1: {0, 1, 2, 3, 4}}, seen)
		require.Equal(t, 2, maxFlight, "the two partitions are processed concurrently")
		require.Equal(t, map[int32]int64{0: 5, 1: 5}, committed(c))
	})
}

func TestService_at_most_once(t *testing.T) {
	t.Run("a processing error is logged and the next record is processed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			produce(t, h, c, 3)

			s := settings(c.ListenAddrs()...)
			s.Delivery = queue.AtMostOnce
			bad := errors.New("cannot process")
			var seen []int64
			tel := humustest.NewTelemetry()
			p, status := humustest.Start(t, consumes(s, func(ctx context.Context, m kafka.Message) error {
				seen = append(seen, m.Offset)
				if m.Offset == 1 {
					return bad
				}
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
			synctest.Wait()
			p.Signal(syscall.SIGTERM)

			st := <-status
			require.Zero(t, st.Code(), st.Cause())
			require.Equal(t, []int64{0, 1, 2}, seen)
			require.Equal(t, int64(3), committed(c)[0])

			errs := records(tel.Logs(), "processing a record failed")
			require.Len(t, errs, 1)
			require.Equal(t, "ERROR", errs[0].Severity().String())
			a := attrs(errs[0])
			require.Equal(t, "orders", a["messaging.destination.name"])
			require.Equal(t, "0", a["messaging.destination.partition.id"])
			require.Equal(t, "1", a["messaging.kafka.offset"])
			require.Equal(t, "cannot process", a["exception.message"])
			require.Equal(t, "*errors.errorString", a["error.type"])
		})
	})

	t.Run("a batch is committed before its first record is processed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			produce(t, h, c, 5)

			s := settings(c.ListenAddrs()...)
			s.Delivery = queue.AtMostOnce
			var atStart []int64
			p, status := humustest.Start(t, consumes(s, func(ctx context.Context, m kafka.Message) error {
				atStart = append(atStart, committed(c)[0])
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host())))
			synctest.Wait()
			p.Signal(syscall.SIGTERM)

			st := <-status
			require.Zero(t, st.Code(), st.Cause())
			require.Equal(t, []int64{5, 5, 5, 5, 5}, atStart, "the batch, 0 to 4, was committed before 0 was processed")
		})
	})

	t.Run("a partition's records are processed in offset order, one at a time", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			produce(t, h, c, 20)

			s := settings(c.ListenAddrs()...)
			s.Delivery = queue.AtMostOnce
			s.MaxPollRecords = 3
			var (
				mu        sync.Mutex
				seen      []int64
				inFlight  int
				maxFlight int
			)
			p, status := humustest.Start(t, consumes(s, func(ctx context.Context, m kafka.Message) error {
				mu.Lock()
				inFlight++
				maxFlight = max(maxFlight, inFlight)
				mu.Unlock()
				time.Sleep(time.Millisecond)
				mu.Lock()
				inFlight--
				seen = append(seen, m.Offset)
				mu.Unlock()
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host())))
			synctest.Wait()
			time.Sleep(time.Second)
			synctest.Wait()
			p.Signal(syscall.SIGTERM)

			st := <-status
			require.Zero(t, st.Code(), st.Cause())
			want := make([]int64, 20)
			for i := range want {
				want[i] = int64(i)
			}
			require.Equal(t, want, seen)
			require.Equal(t, 1, maxFlight)
		})
	})
}

func TestService_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		c, err := kfake.NewCluster(
			kfake.ListenFn(s.Host.Listen),
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, topic),
		)
		require.NoError(t, err)
		s.OnStop(func(context.Context) error {
			c.Close()
			return nil
		})
		svc, err := kafka.NewService(s, settings(c.ListenAddrs()...), queue.ProcessorFunc[kafka.Message](func(context.Context, kafka.Message) error {
			return nil
		}))
		require.NoError(t, err)
		return svc
	})
}

func TestService_grace_period_cuts_the_record_in_flight_short(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 1)

		entered := make(chan struct{}, 1)
		var cancelledAfter time.Duration
		var signalled time.Time
		p, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(ctx context.Context, m kafka.Message) error {
			entered <- struct{}{}
			<-ctx.Done()
			cancelledAfter = time.Since(signalled)
			return ctx.Err()
		}),
			humustest.WithApp(humus.WithHost(h.Host())),
			humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s"}),
		)
		<-entered
		signalled = time.Now()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		require.True(t, ok, "the cause is a *humus.GracePeriodError: %v", st.Cause())
		require.Equal(t, 2*time.Second, gerr.GracePeriod)
		require.Equal(t, 2*time.Second, cancelledAfter, "Process's context is cancelled when the grace period runs out")
		require.NotContains(t, committed(c), int32(0), "the record cut short is not committed")
	})
}

func TestService_revoke_commits_the_record_in_flight_before_the_partition_moves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 2)
		const n = 20
		produceTo(t, h, c, map[int32]int{0: n, 1: n})

		var (
			mu sync.Mutex
			// processed[app][partition] lists the offsets each App
			// finished processing, in order.
			processed = [2]map[int32][]int64{{}, {}}
			// startedAt is the committed offset of each partition when
			// the second App processed its first record of it.
			startedAt = map[int32]int64{}
		)
		processor := func(app int) queue.ProcessorFunc[kafka.Message] {
			return func(ctx context.Context, m kafka.Message) error {
				if app == 1 {
					mu.Lock()
					if _, ok := startedAt[m.Partition]; !ok {
						startedAt[m.Partition] = committed(c)[m.Partition]
					}
					mu.Unlock()
				}
				time.Sleep(time.Second)
				mu.Lock()
				processed[app][m.Partition] = append(processed[app][m.Partition], m.Offset)
				mu.Unlock()
				return nil
			}
		}

		tel := humustest.NewTelemetry()
		first, firstStatus := humustest.Start(t, consumes(settings(c.ListenAddrs()...), processor(0)),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		time.Sleep(2500 * time.Millisecond) // the first App is mid-record on both partitions

		second, secondStatus := humustest.Start(t, consumes(settings(c.ListenAddrs()...), processor(1)),
			humustest.WithApp(humus.WithHost(h.Host())))
		time.Sleep(time.Minute)
		synctest.Wait()

		first.Signal(syscall.SIGTERM)
		st := <-firstStatus
		require.Zero(t, st.Code(), st.Cause())
		second.Signal(syscall.SIGTERM)
		st = <-secondStatus
		require.Zero(t, st.Code(), st.Cause())

		require.Len(t, startedAt, 1, "one partition moved to the second App")
		for moved, at := range startedAt {
			before := processed[0][moved]
			require.NotEmpty(t, before, "the first App processed the moved partition before the revoke")
			require.Equal(t, before[len(before)-1]+1, at,
				"the record in flight finished and was committed before the partition moved, and the second App started after it")
			require.Equal(t, at, processed[1][moved][0])
			require.Equal(t, append(before, processed[1][moved]...), offsets(n), "no record of the moved partition was processed twice or skipped")
		}
		require.Equal(t, map[int32]int64{0: n, 1: n}, committed(c))

		var moved int32
		for p := range startedAt {
			moved = p
		}
		var rebalanced bool
		for _, r := range records(tel.Logs(), "partitions revoked") {
			a := attrs(r)
			require.Equal(t, topic, a["messaging.destination.name"])
			require.Equal(t, group, a["messaging.consumer.group.name"])
			require.NotContains(t, a, "messaging.destination.partition.ids", "the partitions are humus's own attribute")
			if a["humus.messaging.destination.partition.ids"] == "["+strconv.Itoa(int(moved))+"]" {
				rebalanced = true
			}
		}
		require.True(t, rebalanced, "the first App logged the revoke of the moved partition alone")
	})
}

// offsets returns 0 to n-1.
func offsets(n int) []int64 {
	o := make([]int64, n)
	for i := range o {
		o[i] = int64(i)
	}
	return o
}

func TestService_failures(t *testing.T) {
	t.Run("a commit refused with UNKNOWN_MEMBER_ID is not an error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			produce(t, h, c, 3)
			answerOffsetCommit(c, kerr.UnknownMemberID.Code, false)

			var seen []int64
			p, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(ctx context.Context, m kafka.Message) error {
				seen = append(seen, m.Offset)
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host())))
			synctest.Wait()
			require.Equal(t, []int64{0, 1, 2}, seen)

			produce(t, h, c, 2) // offsets 3 and 4: the Service keeps serving
			synctest.Wait()
			p.Signal(syscall.SIGTERM)

			st := <-status
			require.Zero(t, st.Code(), st.Cause())
			require.Equal(t, []int64{0, 1, 2, 3, 4}, seen)
			require.Equal(t, int64(5), committed(c)[0])
		})
	})

	t.Run("a commit refused with GROUP_AUTHORIZATION_FAILED stops the App", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			produce(t, h, c, 3)
			answerOffsetCommit(c, kerr.GroupAuthorizationFailed.Code, true)

			_, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error {
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host())))

			st := <-status
			require.Equal(t, humus.ExitFailure, st.Code())
			require.ErrorIs(t, st.Cause(), kerr.GroupAuthorizationFailed)
		})
	})

	t.Run("a fetch answered with TOPIC_AUTHORIZATION_FAILED stops the App", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			produce(t, h, c, 3)
			answerFetch(c, kerr.TopicAuthorizationFailed.Code)

			_, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error {
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host())))

			st := <-status
			require.Equal(t, humus.ExitFailure, st.Code())
			require.ErrorIs(t, st.Cause(), kerr.TopicAuthorizationFailed)
		})
	})
}

func TestService_keeps_serving_while_the_brokers_are_unreachable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t) // nothing listens on it
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, consumes(settings("127.0.0.1:9092"), func(context.Context, kafka.Message) error {
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))

		time.Sleep(time.Minute)
		select {
		case st := <-status:
			t.Fatalf("the App exited while the brokers were unreachable: %v", st.Cause())
		default:
		}
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), st.Cause())
		require.Positive(t, sum(t, tel, kotelScope, "messaging.kafka.connect_errors.count"), "kotel counts each failed attempt")
	})
}

func TestService_TLS(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		server := ca.Issue(t, "127.0.0.1")
		client := ca.Issue(t, "consumer")
		c := cluster(t, h, 1, kfake.TLS(&tls.Config{
			Certificates: []tls.Certificate{server.TLS()},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    ca.Pool(),
			MinVersion:   tls.VersionTLS12,
		}))
		cfg := &tls.Config{
			Certificates: []tls.Certificate{client.TLS()},
			RootCAs:      ca.Pool(),
			MinVersion:   tls.VersionTLS12,
		}
		produce(t, h, c, 3, kgo.Dialer(tlsDial(h, cfg)))

		s := settings(c.ListenAddrs()...)
		s.TLS = cfg
		var seen []int64
		p, status := humustest.Start(t, consumes(s, func(ctx context.Context, m kafka.Message) error {
			seen = append(seen, m.Offset)
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), st.Cause())
		require.Equal(t, []int64{0, 1, 2}, seen)
		require.Equal(t, int64(3), committed(c)[0])
		require.Empty(t, cfg.ServerName, "the end user's config is unchanged")
	})
}

func TestService_SASL(t *testing.T) {
	user, password := rand.Text(), rand.Text()

	t.Run("with the right password, the App processes and commits", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1, kfake.EnableSASL(), kfake.Superuser("PLAIN", user, password))
			mechanism := plain.Auth{User: user, Pass: password}.AsMechanism()
			produce(t, h, c, 3, kgo.SASL(mechanism))

			s := settings(c.ListenAddrs()...)
			s.SASL = mechanism
			var seen []int64
			p, status := humustest.Start(t, consumes(s, func(ctx context.Context, m kafka.Message) error {
				seen = append(seen, m.Offset)
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host())))
			synctest.Wait()
			p.Signal(syscall.SIGTERM)

			st := <-status
			require.Zero(t, st.Code(), st.Cause())
			require.Equal(t, []int64{0, 1, 2}, seen)
			require.Equal(t, int64(3), committed(c)[0])
		})
	})

	t.Run("with a wrong password, the App exits 1", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1, kfake.EnableSASL(), kfake.Superuser("PLAIN", user, password))
			refuseWrongPasswords(c, password)

			s := settings(c.ListenAddrs()...)
			s.SASL = plain.Auth{User: user, Pass: rand.Text()}.AsMechanism()
			_, status := humustest.Start(t, consumes(s, func(context.Context, kafka.Message) error {
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host())))

			st := <-status
			require.Equal(t, humus.ExitFailure, st.Code())
			require.ErrorIs(t, st.Cause(), kerr.SaslAuthenticationFailed)
			require.ErrorContains(t, st.Cause(), "kafka: authenticating to 127.0.0.1:", "the broker's refusal stopped the Service")
		})
	})
}

func TestService_logs_the_partitions_it_is_assigned_revoked_and_loses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 2)
		produceTo(t, h, c, map[int32]int{0: 1, 1: 1})

		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error {
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		synctest.Wait()

		// The coordinator no longer knows the member: its partitions are
		// lost, and it joins the group again.
		answerHeartbeat(c, kerr.UnknownMemberID.Code)
		time.Sleep(time.Minute)
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), st.Cause())
		require.Empty(t, p.Stderr())

		for _, body := range []string{"partitions assigned", "partitions lost", "partitions revoked"} {
			rs := records(tel.Logs(), body)
			require.NotEmpty(t, rs, "no %q record", body)
			r := rs[0]
			require.Equal(t, "INFO", r.Severity().String(), body)
			require.Equal(t, map[string]string{
				"messaging.system":                          "kafka",
				"messaging.consumer.group.name":             group,
				"messaging.destination.name":                topic,
				"humus.messaging.destination.partition.ids": "[0,1]",
			}, attrs(r), body)
		}
		require.Len(t, records(tel.Logs(), "partitions assigned"), 2, "assigned when it joined, and again when it rejoined")
	})
}

func TestNewService(t *testing.T) {
	t.Parallel()

	setup := &humus.Setup{Host: humus.OSHost(), Telemetry: telemetry.Noop()}
	nop := queue.ProcessorFunc[kafka.Message](func(context.Context, kafka.Message) error { return nil })

	t.Run("reports every bad field, naming each", func(t *testing.T) {
		t.Parallel()

		s := settings("127.0.0.1:9092")
		s.GroupID = ""
		s.MaxPollRecords = 0
		_, err := kafka.NewService(setup, s, nop)
		require.ErrorContains(t, err, "Settings.GroupID must not be empty")
		require.ErrorContains(t, err, "Settings.MaxPollRecords must be positive")
	})

	t.Run("rejects a nil Processor", func(t *testing.T) {
		t.Parallel()

		_, err := kafka.NewService(setup, settings("127.0.0.1:9092"), nil)
		require.ErrorContains(t, err, "the Processor must not be nil")
	})

	t.Run("accepts DefaultSettings with Brokers, GroupID and Topics set", func(t *testing.T) {
		t.Parallel()

		svc, err := kafka.NewService(setup, settings("127.0.0.1:9092"), nop)
		require.NoError(t, err)
		require.NotNil(t, svc)
	})

	t.Run("builds no client and registers no OnStop function", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// A cluster on the Host would see any connection; Init fails
			// after NewService, and the App exits with nothing left to
			// stop.
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			dialled := false
			c.ControlKey(-1, func(kmsg.Request) (kmsg.Response, error, bool) {
				c.KeepControl()
				dialled = true
				return nil, nil, false
			})
			bad := errors.New("init failed after NewService")
			_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				if _, err := kafka.NewService(s, settings(c.ListenAddrs()...), nop); err != nil {
					return nil, err
				}
				return nil, bad
			}, humustest.WithApp(humus.WithHost(h.Host())))

			st := <-status
			require.ErrorIs(t, st.Cause(), bad)
			require.NotContains(t, st.Cause().Error(), "on stop")
			synctest.Wait()
			require.False(t, dialled, "NewService opened a connection")
		})
	})
}

// contextKey is the key of the value TestService_Process_context_keeps_Serve_context_values
// puts on Serve's context.
type contextKey struct{}

func TestService_Process_context_keeps_Serve_context_values(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 1)

		var got any
		svc, err := kafka.NewService(&humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}, settings(c.ListenAddrs()...),
			queue.ProcessorFunc[kafka.Message](func(ctx context.Context, m kafka.Message) error {
				got = ctx.Value(contextKey{})
				return nil
			}))
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "from Serve"))
		served := make(chan error, 1)
		go func() { served <- svc.Serve(ctx) }()
		synctest.Wait()
		cancel()

		require.NoError(t, <-served)
		require.Equal(t, "from Serve", got)
		require.Equal(t, int64(1), committed(c)[0])
		require.NoError(t, svc.Shutdown(context.Background()), "Shutdown after Serve returned")
	})
}

func TestService_at_least_once_failure_lets_the_other_partitions_finish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 2)
		produceTo(t, h, c, map[int32]int{0: 5, 1: 5})

		bad := errors.New("cannot process")
		var (
			mu   sync.Mutex
			seen = map[int32][]int64{}
		)
		_, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(ctx context.Context, m kafka.Message) error {
			if m.Partition == 0 {
				// Partition 0 fails at its first record while partition
				// 1 is in the middle of its first.
				time.Sleep(time.Second)
				return bad
			}
			time.Sleep(3 * time.Second)
			mu.Lock()
			seen[m.Partition] = append(seen[m.Partition], m.Offset)
			mu.Unlock()
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), bad)
		require.ErrorContains(t, st.Cause(), "orders, partition 0, offset 0")
		require.Equal(t, []int64{0}, seen[1], "partition 1 finished its record in flight, and took no other")
		require.Equal(t, map[int32]int64{1: 1}, committed(c), "partition 1 committed its record in flight; partition 0 committed nothing")
	})
}

func TestService_at_most_once_skips_and_logs_a_batch_whose_commit_was_refused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 3)
		answerOffsetCommit(c, kerr.UnknownMemberID.Code, false)

		s := settings(c.ListenAddrs()...)
		s.Delivery = queue.AtMostOnce
		var seen []int64
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, consumes(s, func(ctx context.Context, m kafka.Message) error {
			seen = append(seen, m.Offset)
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		synctest.Wait()

		produce(t, h, c, 2) // offsets 3 and 4: the Service keeps serving
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), st.Cause())
		require.Equal(t, []int64{3, 4}, seen, "the batch 0 to 2 was not committed, so it was not processed")

		skipped := records(tel.Logs(), "batch skipped: its commit was refused")
		require.Len(t, skipped, 1)
		require.Equal(t, "WARN", skipped[0].Severity().String())
		a := attrs(skipped[0])
		require.Equal(t, "0", a["humus.messaging.kafka.offset.first"])
		require.Equal(t, "2", a["humus.messaging.kafka.offset.last"])
		require.NotContains(t, a, "messaging.kafka.offset.first", "the offsets are humus's own attributes")
		require.NotContains(t, a, "messaging.kafka.offset.last", "the offsets are humus's own attributes")
		require.Equal(t, "0", a["messaging.destination.partition.id"])
	})
}

func TestService_a_lost_partition_commits_nothing_and_is_processed_again(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 1)

		var (
			seen       []int64
			committed0 []int64
		)
		first := true
		p, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(ctx context.Context, m kafka.Message) error {
			seen = append(seen, m.Offset)
			if first {
				first = false
				// The coordinator forgets the member while offset 0 is in
				// flight: the next heartbeat fails, and the partition is
				// lost.
				answerHeartbeat(c, kerr.UnknownMemberID.Code)
				time.Sleep(10 * time.Second)
				committed0 = append(committed0, committed(c)[0])
			}
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))
		time.Sleep(time.Minute)
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), st.Cause())
		require.Equal(t, []int64{0, 0}, seen, "offset 0 was processed again after the partition was lost and assigned again")
		require.Equal(t, []int64{0}, committed0)
		require.Equal(t, int64(1), committed(c)[0])
	})
}

func TestService_a_fenced_member_commits_nothing_more_for_the_partitions_it_held(t *testing.T) {
	// The first App is stuck in a revoke, its records in flight, while the
	// third App's join starts another rebalance. kfake waits
	// RebalanceTimeout for it to rejoin, then drops it and gives both its
	// partitions away. franz-go learns of the fence from a refused commit,
	// rejoins as a new member, and reports the partition the first App
	// still held as revoked. kfake, like Kafka, checks a commit's member ID
	// and generation and not who owns the partition, so the revoke's commit,
	// sent under the new member ID, would rewind the new owner's committed
	// offset.
	//
	// The first App's records are released one at a time so that no commit
	// is called while franz-go joins: such a commit waits on a mutex, which
	// synctest doesn't count as durably blocked, so time would stop. The
	// container tests cover that case against Kafka.
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 2)
		produceTo(t, h, c, map[int32]int{0: 1, 1: 1})

		// sent is every OffsetCommit that reached the cluster.
		type commit struct {
			member    string
			partition int32
			offset    int64
		}
		var (
			mu   sync.Mutex
			sent []commit
		)
		c.ControlKey(kmsg.OffsetCommit.Int16(), func(req kmsg.Request) (kmsg.Response, error, bool) {
			c.KeepControl()
			r := req.(*kmsg.OffsetCommitRequest)
			mu.Lock()
			defer mu.Unlock()
			for _, rt := range r.Topics {
				for _, rp := range rt.Partitions {
					sent = append(sent, commit{r.MemberID, rp.Partition, rp.Offset})
				}
			}
			return nil, nil, false
		})

		s := settings(c.ListenAddrs()...)
		s.RebalanceTimeout = 5 * time.Second

		// gates holds the first App's records of each partition, by
		// offset, until the test releases them.
		type at struct {
			partition int32
			offset    int64
		}
		gates := map[at]chan struct{}{
			{0, 0}: make(chan struct{}), {1, 0}: make(chan struct{}),
			{0, 1}: make(chan struct{}), {1, 1}: make(chan struct{}),
		}
		var (
			pmu       sync.Mutex
			processed = map[string]map[int32][]int64{}
		)
		processor := func(app string) queue.ProcessorFunc[kafka.Message] {
			return func(ctx context.Context, m kafka.Message) error {
				if gate, ok := gates[at{m.Partition, m.Offset}]; ok && app == "first" {
					select {
					case <-gate:
					case <-ctx.Done():
					}
				}
				pmu.Lock()
				defer pmu.Unlock()
				if processed[app] == nil {
					processed[app] = map[int32][]int64{}
				}
				processed[app][m.Partition] = append(processed[app][m.Partition], m.Offset)
				return nil
			}
		}
		type app struct {
			p      *bedrocktest.Process
			status <-chan bedrock.ExitStatus
		}
		start := func(name string, opts ...humus.Option) app {
			p, status := humustest.Start(t, consumes(s, processor(name)),
				humustest.WithApp(append([]humus.Option{humus.WithHost(h.Host())}, opts...)...))
			return app{p, status}
		}

		tel := humustest.NewTelemetry()
		first := start("first", humus.WithTelemetry(tel.Builder()))
		time.Sleep(10 * time.Second) // the first App is mid-record on both partitions
		synctest.Wait()

		// The second App's join moves one partition, whose revoke waits
		// for the record in flight.
		second := start("second")
		time.Sleep(4 * time.Second) // within RebalanceTimeout
		synctest.Wait()
		revoked := records(tel.Logs(), "partitions revoked")
		require.Len(t, revoked, 1, "the first App begins revoking a partition")
		var moved, kept int32 = 0, 1 // kept is the partition the first App still holds when it is fenced
		if attrs(revoked[0])["humus.messaging.destination.partition.ids"] == "[1]" {
			moved, kept = 1, 0
		}

		// The third App's join starts the rebalance the first App can't
		// rejoin while its revoke waits: it is fenced, and the other Apps
		// process both partitions.
		third := start("third")
		time.Sleep(time.Minute)
		synctest.Wait()

		// The kept partition's new owner processes ten more records and
		// commits past them.
		produceTo(t, h, c, map[int32]int{kept: 10}) // offsets 1 to 10
		time.Sleep(10 * time.Second)
		synctest.Wait()
		require.Equal(t, map[int32]int64{moved: 1, kept: 11}, committed(c))

		// The kept partition's record in flight finishes, and its commit
		// is refused: the member was fenced out. The partition takes the
		// batch the first App polled after the fence, and holds offset 1
		// in flight.
		close(gates[at{kept, 0}])
		time.Sleep(time.Second)
		synctest.Wait()

		// The moved partition's record in flight finishes, and the revoke
		// ends. franz-go rejoins as a new member, which the new assignment
		// gives nothing, so it revokes the kept partition, whose revoke
		// waits for offset 1.
		close(gates[at{moved, 0}])
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Len(t, records(tel.Logs(), "partitions revoked"), 2, "the first App revokes the partition it kept")
		require.Equal(t, "["+strconv.Itoa(int(kept))+"]", attrs(records(tel.Logs(), "partitions revoked")[1])["humus.messaging.destination.partition.ids"])

		close(gates[at{kept, 1}])
		close(gates[at{moved, 1}])
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Equal(t, map[int32]int64{moved: 1, kept: 11}, committed(c), "the new owner's committed offset is unchanged")

		for _, a := range []app{first, second, third} {
			a.p.Signal(syscall.SIGTERM)
			st := <-a.status
			require.Zero(t, st.Code(), st.Cause())
		}
		require.Equal(t, map[int32][]int64{moved: {0}, kept: {0, 1}}, processed["first"])
		require.Equal(t, map[int32]int64{moved: 1, kept: 11}, committed(c))

		// The first App's member IDs, as franz-go logged them: the first
		// is the one it was fenced out under.
		var members []string
		for _, r := range tel.Logs() {
			if m := attrs(r)["member_id"]; r.InstrumentationScope().Name == "github.com/twmb/franz-go/pkg/kgo" && m != "" && !slices.Contains(members, m) {
				members = append(members, m)
			}
		}
		require.Greater(t, len(members), 1, "the first App rejoined as a new member")
		mu.Lock()
		defer mu.Unlock()
		for _, cm := range sent {
			if cm.partition == kept && slices.Contains(members[1:], cm.member) {
				t.Errorf("the first App sent a commit of partition %d, offset %d, under its new member ID %s", kept, cm.offset, cm.member)
			}
		}
	})
}
