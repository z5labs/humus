// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

// joined reports whether svc's Joined monitor is healthy, and fails the
// test if the monitor returns an error.
func joined(t *testing.T, svc *kafka.Service) bool {
	t.Helper()
	ok, err := svc.Joined().Healthy(context.Background())
	require.NoError(t, err, "Joined's monitor never returns an error")
	return ok
}

// check is one check of a Joined monitor.
type check struct {
	healthy bool
	err     error
}

// joinedConsumer returns an Init whose only Service consumes settings. It
// sends the Service on the first channel it returns, and the check of its
// Joined monitor in Init, before Serve, on the second.
func joinedConsumer(settings kafka.Settings) (func(context.Context, *humus.Setup) ([]humus.Service, error), <-chan *kafka.Service, <-chan check) {
	svcs := make(chan *kafka.Service, 1)
	inInit := make(chan check, 1)
	nop := queue.ProcessorFunc[kafka.Message](func(context.Context, kafka.Message) error { return nil })
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := kafka.NewService(s, settings, nop)
		if err != nil {
			return nil, err
		}
		healthy, err := svc.Joined().Healthy(context.Background())
		inInit <- check{healthy, err}
		svcs <- svc
		return []humus.Service{svc}, nil
	}, svcs, inInit
}

func TestService_Joined(t *testing.T) {
	t.Run("before Serve, it is unhealthy", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)

			consume, svcs, inInit := joinedConsumer(settings(c.ListenAddrs()...))
			p, status := humustest.Start(t, consume, humustest.WithApp(humus.WithHost(h.Host())))
			before := <-inInit
			require.NoError(t, before.err, "Joined's monitor never returns an error")
			require.False(t, before.healthy, "unhealthy in Init, before Serve")
			svc := <-svcs

			synctest.Wait()
			require.True(t, joined(t, svc))
			p.Signal(syscall.SIGTERM)
			st := <-status
			require.Zero(t, st.Code(), st.Cause())
		})
	})

	t.Run("it turns healthy once the group is joined", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			produce(t, h, c, 1)

			consume, svcs, _ := joinedConsumer(settings(c.ListenAddrs()...))
			p, status := humustest.Start(t, consume, humustest.WithApp(humus.WithHost(h.Host())))
			svc := <-svcs

			synctest.Wait()
			require.Equal(t, 1, members(c), "the Service joined the group")
			require.True(t, joined(t, svc), "healthy once the group is joined")

			p.Signal(syscall.SIGTERM)
			st := <-status
			require.Zero(t, st.Code(), st.Cause())
		})
	})

	t.Run("a member assigned no partition is healthy too", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1) // one partition, for two members

			first, firstSvcs, _ := joinedConsumer(settings(c.ListenAddrs()...))
			p1, status1 := humustest.Start(t, first, humustest.WithApp(humus.WithHost(h.Host())))
			svc1 := <-firstSvcs
			synctest.Wait()
			require.True(t, joined(t, svc1))

			second, secondSvcs, _ := joinedConsumer(settings(c.ListenAddrs()...))
			p2, status2 := humustest.Start(t, second, humustest.WithApp(humus.WithHost(h.Host())))
			svc2 := <-secondSvcs
			time.Sleep(time.Minute) // the group rebalances
			synctest.Wait()

			require.Equal(t, 2, members(c), "both Services are in the group")
			info := c.GroupInfo(group)
			require.NotNil(t, info)
			var counts []int
			for _, m := range info.Members {
				counts = append(counts, m.NumAssigned())
			}
			require.ElementsMatch(t, []int{0, 1}, counts, "one member holds the partition, and the other none")
			require.True(t, joined(t, svc1), "the first stays healthy through the rebalance")
			require.True(t, joined(t, svc2), "the second is healthy, whichever of the two holds the partition")

			p2.Signal(syscall.SIGTERM)
			st := <-status2
			require.Zero(t, st.Code(), st.Cause())
			p1.Signal(syscall.SIGTERM)
			st = <-status1
			require.Zero(t, st.Code(), st.Cause())
		})
	})

	t.Run("it stays healthy through a broker outage after the join", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)

			consume, svcs, _ := joinedConsumer(settings(c.ListenAddrs()...))
			p, status := humustest.Start(t, consume, humustest.WithApp(humus.WithHost(h.Host())))
			svc := <-svcs
			synctest.Wait()
			require.True(t, joined(t, svc))

			c.Close() // the brokers go away; franz-go retries
			time.Sleep(time.Minute)
			select {
			case st := <-status:
				t.Fatalf("the App exited while the brokers were unreachable: %v", st.Cause())
			default:
			}
			require.True(t, joined(t, svc), "a broker outage after the join doesn't make it unhealthy")

			p.Signal(syscall.SIGTERM)
			<-status
			require.False(t, joined(t, svc), "unhealthy once Serve has returned")
		})
	})

	t.Run("with no broker listening, it stays unhealthy while franz-go retries", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t) // nothing listens on it

			consume, svcs, _ := joinedConsumer(settings("127.0.0.1:9092"))
			p, status := humustest.Start(t, consume, humustest.WithApp(humus.WithHost(h.Host())))
			svc := <-svcs

			time.Sleep(time.Minute)
			select {
			case st := <-status:
				t.Fatalf("the App exited while the brokers were unreachable: %v", st.Cause())
			default:
			}
			require.False(t, joined(t, svc), "never joined, so unhealthy")

			p.Signal(syscall.SIGTERM)
			st := <-status
			require.Zero(t, st.Code(), st.Cause())
			require.False(t, joined(t, svc))
		})
	})

	t.Run("after Serve, it is unhealthy", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)

			consume, svcs, _ := joinedConsumer(settings(c.ListenAddrs()...))
			p, status := humustest.Start(t, consume, humustest.WithApp(humus.WithHost(h.Host())))
			svc := <-svcs
			synctest.Wait()
			require.True(t, joined(t, svc))

			p.Signal(syscall.SIGTERM)
			st := <-status
			require.Zero(t, st.Code(), st.Cause())
			require.Zero(t, members(c), "the Service left the group")
			require.False(t, joined(t, svc), "unhealthy once Serve has returned, after SIGTERM")
		})
	})
}
