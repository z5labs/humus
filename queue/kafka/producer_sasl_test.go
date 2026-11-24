// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"crypto/rand"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kfake"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/kafka"
)

// refusedBody is the body of the Producer's record of a refusal.
const refusedBody = "a broker refused the credentials"

// errorRecords returns the records at ERROR or above emitted under the
// package's own scope.
func errorRecords(logs []sdklog.Record) []sdklog.Record {
	var out []sdklog.Record
	for _, r := range logs {
		if r.InstrumentationScope().Name == "github.com/z5labs/humus/queue/kafka" && r.Severity() >= log.SeverityError {
			out = append(out, r)
		}
	}
	return out
}

func TestProducer_SASL(t *testing.T) {
	user, password := rand.Text(), rand.Text()

	for _, m := range saslMechanisms {
		t.Run(m.test, func(t *testing.T) {
			t.Run("with the right password, the Producer produces and logs no error", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					h := humustest.NewHost(t)
					c, seed := saslCluster(t, h, m.name, user, password)

					ps := producerSettings(c.ListenAddrs()...)
					ps.SASL = m.of(user, password)
					tel := humustest.NewTelemetry()
					var err error
					_, status := humustest.Start(t, produces(ps, func(ctx context.Context, p *kafka.Producer) error {
						err = p.Produce(ctx, value(0))
						return nil
					}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))

					st := <-status
					require.Zero(t, st.Code(), "cause: %v", st.Cause())
					require.NoError(t, err)
					got := readAll(t, h, c, topic, seed)
					require.Len(t, got, 1)
					require.Equal(t, "0", string(got[0].Value))
					require.Empty(t, errorRecords(tel.Logs()))
				})
			})

			t.Run("with a wrong password, a call fails with the refusal", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					h := humustest.NewHost(t)
					c, _ := saslCluster(t, h, m.name, user, password)

					ps := producerSettings(c.ListenAddrs()...)
					ps.SASL = m.of(user, rand.Text())
					var err error
					_, status := humustest.Start(t, produces(ps, func(ctx context.Context, p *kafka.Producer) error {
						err = p.Produce(ctx, value(0))
						return nil
					}), humustest.WithApp(humus.WithHost(h.Host())))

					st := <-status
					require.Zero(t, st.Code(), "cause: %v", st.Cause())
					requireCode(t, err, saslAuthenticationFailed)
					require.ErrorContains(t, err, "kafka: authenticating to "+c.ListenAddrs()[0]+": ", "the error names the broker")
				})
			})
		})
	}

	t.Run("with a wrong password, every call fails with the refusal and one record is logged", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1, kfake.EnableSASL(), kfake.Superuser("PLAIN", user, password))
			refuseWrongPasswords(c, password)

			ps := producerSettings(c.ListenAddrs()...)
			ps.SASL = kafka.Plain(user, rand.Text())
			tel := humustest.NewTelemetry()
			var (
				first, second, third error
				tookFirst, tookLater time.Duration
			)
			_, status := humustest.Start(t, produces(ps, func(ctx context.Context, p *kafka.Producer) error {
				start := time.Now()
				first = p.Produce(ctx, value(0))
				tookFirst = time.Since(start)

				start = time.Now()
				second = p.Produce(ctx, value(1))
				third = p.Produce(ctx, value(2), value(3))
				tookLater = time.Since(start)
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))

			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())

			broker := c.ListenAddrs()[0]
			requireCode(t, first, saslAuthenticationFailed)
			require.ErrorContains(t, first, "kafka: authenticating to "+broker+": ", "the error names the broker")
			require.Less(t, tookFirst, ps.DeliveryTimeout, "the first call failed on the refusal, not at its deadline")

			require.Equal(t, first, second, "a later call fails with the same error")
			require.Equal(t, first, third)
			require.Zero(t, tookLater, "later calls fail at once")

			var sends int
			for _, s := range tel.Spans() {
				if s.Name() == sendSpan {
					sends++
				}
			}
			require.Equal(t, 1, sends, "only the first call's record was handed to the client")

			require.Len(t, errorRecords(tel.Logs()), 1, "one ERROR record, however many calls follow")
			refusals := records(tel.Logs(), refusedBody)
			require.Len(t, refusals, 1)
			require.Equal(t, log.SeverityError, refusals[0].Severity())
			host, port, err := net.SplitHostPort(broker)
			require.NoError(t, err)
			a := attrs(refusals[0])
			require.Equal(t, "kafka", a["messaging.system"])
			require.Equal(t, host, a["server.address"])
			require.Equal(t, port, a["server.port"])
			require.Equal(t, first.Error(), a["exception.message"])
		})
	})

	t.Run("calls in flight all fail with the refusal", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1, kfake.EnableSASL(), kfake.Superuser("PLAIN", user, password))
			refuseWrongPasswords(c, password)

			ps := producerSettings(c.ListenAddrs()...)
			ps.SASL = kafka.Plain(user, rand.Text())
			tel := humustest.NewTelemetry()
			errs := make([]error, 5)
			_, status := humustest.Start(t, produces(ps, func(ctx context.Context, p *kafka.Producer) error {
				var wg sync.WaitGroup
				for i := range errs {
					wg.Go(func() {
						errs[i] = p.Produce(ctx, value(i))
					})
				}
				wg.Wait()
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))

			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
			for _, err := range errs {
				requireCode(t, err, saslAuthenticationFailed)
				require.Equal(t, errs[0], err)
			}
			require.Len(t, records(tel.Logs(), refusedBody), 1)
		})
	})
}
