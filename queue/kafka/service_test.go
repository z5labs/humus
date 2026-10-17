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
		p, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(ctx context.Context, m kafka.Message) error {
			entered <- struct{}{}
			time.Sleep(3 * time.Second)
			require.NoError(t, ctx.Err(), "Process's context is not cancelled when shutdown starts")
			finished <- m.Offset
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))
		<-entered // offset 0 is in flight
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), st.Cause())
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

		revoked := records(tel.Logs(), "partitions revoked")
		require.NotEmpty(t, revoked)
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
		p, status := humustest.Start(t, consumes(settings("127.0.0.1:9092"), func(context.Context, kafka.Message) error {
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		time.Sleep(time.Minute)
		select {
		case st := <-status:
			t.Fatalf("the App exited while the brokers were unreachable: %v", st.Cause())
		default:
		}
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), st.Cause())
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
				"messaging.system":                    "kafka",
				"messaging.consumer.group.name":       group,
				"messaging.destination.name":          topic,
				"messaging.destination.partition.ids": "[0,1]",
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
