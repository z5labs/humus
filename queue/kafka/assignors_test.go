// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"slices"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

// describeGroup returns the group as kadm's DescribeGroups reports it,
// through a client that dials h.
func describeGroup(t *testing.T, h *humustest.Host, c *kfake.Cluster) kadm.DescribedGroup {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(c.ListenAddrs()...), kgo.Dialer(h.Dial))
	require.NoError(t, err)
	defer cl.Close()
	groups, err := kadm.NewClient(cl).DescribeGroups(context.Background(), group)
	require.NoError(t, err)
	g, ok := groups[group]
	require.True(t, ok, "DescribeGroups reports the group")
	require.NoError(t, g.Err)
	return g
}

// partitionsOf records, for each App, the partitions of the records it
// processed. It is safe for concurrent use.
type partitionsOf struct {
	mu   sync.Mutex
	apps map[int][]int32
}

func (p *partitionsOf) processor(app int) queue.ProcessorFunc[kafka.Message] {
	return func(_ context.Context, m kafka.Message) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !slices.Contains(p.apps[app], m.Partition) {
			p.apps[app] = append(p.apps[app], m.Partition)
		}
		return nil
	}
}

func (p *partitionsOf) of(app int) []int32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps := slices.Clone(p.apps[app])
	slices.Sort(ps)
	return ps
}

// assignorsPartitions is how many partitions topic has in
// TestService_Assignors: four, so that range's assignment of them to two
// members differs from roundrobin's.
const assignorsPartitions = 4

func TestService_Assignors(t *testing.T) {
	testCases := []struct {
		name string
		// first and second are the Assignors of the first and the second
		// App's Service.
		first, second []kafka.Assignor
		// protocol is the protocol the group runs.
		protocol string
	}{
		{name: "cooperative-sticky", first: []kafka.Assignor{kafka.CooperativeStickyAssignor}, second: []kafka.Assignor{kafka.CooperativeStickyAssignor}, protocol: "cooperative-sticky"},
		{name: "sticky", first: []kafka.Assignor{kafka.StickyAssignor}, second: []kafka.Assignor{kafka.StickyAssignor}, protocol: "sticky"},
		{name: "range", first: []kafka.Assignor{kafka.RangeAssignor}, second: []kafka.Assignor{kafka.RangeAssignor}, protocol: "range"},
		{name: "roundrobin", first: []kafka.Assignor{kafka.RoundRobinAssignor}, second: []kafka.Assignor{kafka.RoundRobinAssignor}, protocol: "roundrobin"},
		{
			name:     "the first that both offer, when both prefer it",
			first:    []kafka.Assignor{kafka.RangeAssignor, kafka.RoundRobinAssignor},
			second:   []kafka.Assignor{kafka.RangeAssignor, kafka.RoundRobinAssignor},
			protocol: "range",
		},
		{
			name:     "the only one that both offer, though it is not the first's first",
			first:    []kafka.Assignor{kafka.RoundRobinAssignor, kafka.RangeAssignor},
			second:   []kafka.Assignor{kafka.RangeAssignor},
			protocol: "range",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := humustest.NewHost(t)
				c := cluster(t, h, assignorsPartitions)
				processed := &partitionsOf{apps: map[int][]int32{}}

				start := func(app int, assignors []kafka.Assignor) func() {
					s := settings(c.ListenAddrs()...)
					s.Assignors = assignors
					p, status := humustest.Start(t, consumes(s, processed.processor(app)), humustest.WithApp(humus.WithHost(h.Host())))
					return func() {
						p.Signal(syscall.SIGTERM)
						st := <-status
						require.Zero(t, st.Code(), st.Cause())
					}
				}
				stopFirst := start(0, tc.first)
				synctest.Wait()
				stopSecond := start(1, tc.second)
				time.Sleep(time.Minute) // the group rebalances
				synctest.Wait()

				g := describeGroup(t, h, c)
				require.Equal(t, "Stable", g.State)
				require.Equal(t, "consumer", g.ProtocolType)
				require.Equal(t, tc.protocol, g.Protocol, "the group runs the protocol of that name")
				require.Len(t, g.Members, 2)

				// Each partition gets a record once the group is stable,
				// so each App processes only what it was assigned.
				produceTo(t, h, c, map[int32]int{0: 1, 1: 1, 2: 1, 3: 1})
				time.Sleep(10 * time.Second)
				synctest.Wait()

				info := c.GroupInfo(group)
				require.NotNil(t, info)
				require.Len(t, info.Members, 2)
				var assigned [][]int32
				for _, m := range info.Members {
					ps := slices.Clone(m.Assignment[topic])
					slices.Sort(ps)
					assigned = append(assigned, ps)
				}
				require.ElementsMatch(t, assigned, [][]int32{processed.of(0), processed.of(1)},
					"each App processed the partitions its Service was assigned")
				if tc.protocol == "range" {
					// Range sorts the members by member ID, as GroupInfo
					// does, and gives each a range of the partitions.
					require.Equal(t, [][]int32{{0, 1}, {2, 3}}, assigned, "each Service's assignment is range's")
				}

				stopSecond()
				stopFirst()
			})
		})
	}
}
