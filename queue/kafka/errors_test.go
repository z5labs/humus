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
	"fmt"
	"io"
	"net"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/kafka"
)

// The Kafka error codes the tests expect, as the package reports them.
var (
	invalidRecord            = &kafka.Error{Code: 87, Name: "INVALID_RECORD"}
	messageTooLarge          = &kafka.Error{Code: 10, Name: "MESSAGE_TOO_LARGE"}
	notEnoughReplicas        = &kafka.Error{Code: 19, Name: "NOT_ENOUGH_REPLICAS", Retriable: true}
	topicAuthorizationFailed = &kafka.Error{Code: 29, Name: "TOPIC_AUTHORIZATION_FAILED"}
	groupAuthorizationFailed = &kafka.Error{Code: 30, Name: "GROUP_AUTHORIZATION_FAILED"}
	saslAuthenticationFailed = &kafka.Error{Code: 58, Name: "SASL_AUTHENTICATION_FAILED"}
	unknownTopicOrPartition  = &kafka.Error{Code: 3, Name: "UNKNOWN_TOPIC_OR_PARTITION", Retriable: true}
)

// requireCode requires that errors.AsType finds an *kafka.Error in err with
// want's number, name and retriability, and that errors.Is matches err with
// want.
func requireCode(t *testing.T, err error, want *kafka.Error, msgAndArgs ...any) {
	t.Helper()
	got, ok := errors.AsType[*kafka.Error](err)
	require.True(t, ok, "errors.As finds no *kafka.Error in %v", err)
	require.Equal(t, *want, *got, msgAndArgs...)
	require.ErrorIs(t, err, want, msgAndArgs...)
}

// requireNoFranzGo walks err's Unwrap, both kinds, and requires that no
// error in it has a type from a franz-go package or is one of the error
// values franz-go exports.
func requireNoFranzGo(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var walk func(err error, at string)
	walk = func(err error, at string) {
		typ := reflect.TypeOf(err)
		elem := typ
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if path := elem.PkgPath(); path == "github.com/twmb/franz-go" || strings.HasPrefix(path, "github.com/twmb/franz-go/") {
			t.Errorf("%s: %v is a %s, a type of franz-go's", at, err, typ)
		}
		for name, v := range kafka.FranzGoValues {
			if typ == reflect.TypeOf(v) && err == v {
				t.Errorf("%s: %v is %s", at, err, name)
			}
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			if w := u.Unwrap(); w != nil {
				walk(w, at+".Unwrap()")
			}
		case interface{ Unwrap() []error }:
			for i, w := range u.Unwrap() {
				if w != nil {
					walk(w, fmt.Sprintf("%s.Unwrap()[%d]", at, i))
				}
			}
		}
	}
	walk(err, "err")
}

// franzGoName matches a type's name as reflect or semconv.ErrorType writes
// it, when the type is one of franz-go's packages'.
var franzGoName = regexp.MustCompile(`franz-go|^\*?(kerr|kgo|kbin|kmsg|kversion|sasl|plain|scram|oauth|aws|kotel)\.`)

// requireErrorTypeNotFranzGo requires that errType, an error.type, names no
// type of franz-go's.
func requireErrorTypeNotFranzGo(t *testing.T, errType string) {
	t.Helper()
	require.NotEmpty(t, errType)
	require.NotRegexp(t, franzGoName, errType, "error.type names a franz-go type")
}

func TestError(t *testing.T) {
	t.Parallel()

	require.Equal(t, "kafka: TOPIC_AUTHORIZATION_FAILED (error code 29)", topicAuthorizationFailed.Error())
	require.Equal(t, "kafka: error code 29", (&kafka.Error{Code: 29}).Error())

	wrapped := fmt.Errorf("kafka: fetching orders, partition 0: %w", topicAuthorizationFailed)
	require.ErrorIs(t, wrapped, &kafka.Error{Code: 29}, "errors.Is compares codes alone")
	require.NotErrorIs(t, wrapped, &kafka.Error{Code: 30, Name: "TOPIC_AUTHORIZATION_FAILED"})
	require.NotErrorIs(t, wrapped, (*kafka.Error)(nil))
	require.NotErrorIs(t, wrapped, errors.New("kafka: TOPIC_AUTHORIZATION_FAILED (error code 29)"))
}

func TestFromClient(t *testing.T) {
	t.Parallel()

	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tlsErr := &tls.CertificateVerificationError{Err: errors.New("bad certificate")}
	testCases := []struct {
		name    string
		err     error
		code    *kafka.Error // found with errors.As, when not nil
		matches []error      // each found with errors.Is
	}{
		{name: "a Kafka error code", err: kerr.TopicAuthorizationFailed, code: topicAuthorizationFailed},
		{name: "a retriable Kafka error code", err: kerr.UnknownTopicOrPartition, code: unknownTopicOrPartition},
		{
			name: "a Kafka error code franz-go wraps",
			err:  fmt.Errorf("%w (uncompressed_bytes=2000000)", kerr.MessageTooLarge),
			code: messageTooLarge,
		},
		{
			name: "a refusal with the broker's message",
			err:  fmt.Errorf("%s: %w", "Authentication failed", kerr.SaslAuthenticationFailed),
			code: saslAuthenticationFailed,
		},
		{
			name:    "franz-go's timeout, with its last error",
			err:     fmt.Errorf("%w, last err: %w", kgo.ErrRecordTimeout, kerr.NotEnoughReplicas),
			code:    notEnoughReplicas,
			matches: []error{},
		},
		{name: "franz-go's closed client", err: kgo.ErrClientClosed},
		{name: "a connection franz-go failed", err: fmt.Errorf("unable to dial: %w", opErr), matches: []error{opErr}},
		{name: "a TLS failure", err: fmt.Errorf("unable to dial: %w", tlsErr), matches: []error{tlsErr}},
		{name: "a franz-go type wrapping io.EOF", err: &kgo.ErrFirstReadEOF{}, matches: []error{}},
		{name: "context", err: fmt.Errorf("%w: %w", kgo.ErrClientClosed, context.Canceled), matches: []error{context.Canceled}},
		{name: "joined", err: errors.Join(kerr.InvalidRecord, io.ErrUnexpectedEOF), code: invalidRecord, matches: []error{io.ErrUnexpectedEOF}},
	}
	for name, v := range kafka.FranzGoValues {
		testCases = append(testCases, struct {
			name    string
			err     error
			code    *kafka.Error
			matches []error
		}{name: name, err: v})
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := kafka.FromClient(tc.err)
			requireNoFranzGo(t, got)
			if _, code := tc.err.(*kerr.Error); code {
				require.Equal(t, tc.code.Error(), got.Error(), "a code is humus's *kafka.Error")
			} else {
				require.Equal(t, tc.err.Error(), got.Error(), "the client's message is kept")
			}
			if tc.code != nil {
				requireCode(t, got, tc.code)
			}
			for _, m := range tc.matches {
				require.ErrorIs(t, got, m)
			}
		})
	}

	t.Run("an error with nothing of franz-go's is kept as it is", func(t *testing.T) {
		t.Parallel()

		err := fmt.Errorf("kafka: committing: %w", context.DeadlineExceeded)
		require.Same(t, err, kafka.FromClient(err))
		require.NoError(t, kafka.FromClient(nil))
	})
}

func TestRecordError(t *testing.T) {
	t.Parallel()

	timeouts := []struct {
		name string
		err  error
	}{
		{name: "the call's DeliveryTimeout deadline", err: context.DeadlineExceeded},
		{name: "the client's own timeout", err: kgo.ErrRecordTimeout},
		{name: "the client's own timeout, with its last error", err: fmt.Errorf("%w, last err: %w", kgo.ErrRecordTimeout, kerr.NotEnoughReplicas)},
	}
	for _, tc := range timeouts {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := kafka.RecordError(tc.err)
			require.ErrorIs(t, got, kafka.ErrDeliveryTimeout)
			require.ErrorIs(t, got, context.DeadlineExceeded)
			require.ErrorContains(t, got, kafka.ErrDeliveryTimeout.Error())
			requireNoFranzGo(t, got)
		})
	}

	t.Run("a closed client", func(t *testing.T) {
		t.Parallel()

		got := kafka.RecordError(kgo.ErrClientClosed)
		require.ErrorIs(t, got, kafka.ErrProducerClosed)
		requireNoFranzGo(t, got)
	})

	t.Run("a Kafka error code", func(t *testing.T) {
		t.Parallel()

		got := kafka.RecordError(kerr.MessageTooLarge)
		requireCode(t, got, messageTooLarge)
		require.NotErrorIs(t, got, kafka.ErrDeliveryTimeout)
		require.NotErrorIs(t, got, context.DeadlineExceeded)
	})

	t.Run("an acknowledged record", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, kafka.RecordError(nil))
	})
}

func TestProduce_a_record_refused_with_MESSAGE_TOO_LARGE_returns_an_Error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		c.Fault(kfake.Fault{Keys: []kmsg.Key{kmsg.Produce}, Topic: topic, Err: kerr.MessageTooLarge, Count: -1})

		var err error
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx, value(0))
			return nil
		})

		requireCode(t, err, messageTooLarge)
		require.ErrorContains(t, err, "kafka: producing to orders, partition 0: ")
		requireNoFranzGo(t, err)

		errType := spanAttrs(spanNamed(t, tel, sendSpan))["error.type"].AsString()
		requireErrorTypeNotFranzGo(t, errType)
		sent, ok := metricNamed(t, tel, serviceScope, "messaging.client.sent.messages").Data.(metricdata.Sum[int64])
		require.True(t, ok)
		require.Len(t, sent.DataPoints, 1)
		require.Equal(t, errType, setAttrs(sent.DataPoints[0].Attributes)["error.type"])
	})
}

func TestServe_ended_by_TOPIC_AUTHORIZATION_FAILED_returns_an_Error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 3)
		c.Fault(kfake.Fault{Keys: []kmsg.Key{kmsg.Fetch}, Topic: topic, Err: kerr.TopicAuthorizationFailed, Count: -1})

		_, status := humustest.Start(t, consumes(settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error {
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		requireCode(t, st.Cause(), topicAuthorizationFailed)
		require.ErrorContains(t, st.Cause(), "kafka: fetching orders, partition 0: ")
		requireNoFranzGo(t, st.Cause())
		requireErrorTypeNotFranzGo(t, semconv.ErrorType(st.Cause()).Value.AsString())
	})
}

func TestService_refused_SASL_credentials_give_SASL_AUTHENTICATION_FAILED(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		user, password := rand.Text(), rand.Text()
		c, _ := saslCluster(t, h, "PLAIN", user, password)

		s := settings(c.ListenAddrs()...)
		s.SASL = kafka.Plain(user, rand.Text())
		_, status := humustest.Start(t, consumes(s, func(context.Context, kafka.Message) error {
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host())))

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		requireCode(t, st.Cause(), saslAuthenticationFailed)
		requireNoFranzGo(t, st.Cause())
		requireErrorTypeNotFranzGo(t, semconv.ErrorType(st.Cause()).Value.AsString())
	})
}

func TestProducer_refused_SASL_credentials_give_SASL_AUTHENTICATION_FAILED(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		user, password := rand.Text(), rand.Text()
		c, _ := saslCluster(t, h, "PLAIN", user, password)

		ps := producerSettings(c.ListenAddrs()...)
		ps.SASL = kafka.Plain(user, rand.Text())
		var err error
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx, value(0))
			return nil
		})

		requireCode(t, err, saslAuthenticationFailed)
		requireNoFranzGo(t, err)
		refusals := records(tel.Logs(), refusedBody)
		require.Len(t, refusals, 1)
		requireErrorTypeNotFranzGo(t, attrs(refusals[0])["error.type"])
	})
}

func TestProduce_a_record_past_DeliveryTimeout_matches_ErrDeliveryTimeout(t *testing.T) {
	t.Run("the call's deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			holdProduce(t, c) // released when the test ends

			ps := producerSettings(c.ListenAddrs()...)
			ps.DeliveryTimeout = 2 * time.Second
			var err error
			tel := humustest.NewTelemetry()
			producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
				err = p.Produce(ctx, value(0))
				return nil
			})

			require.ErrorIs(t, err, kafka.ErrDeliveryTimeout)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			requireNoFranzGo(t, err)
			requireErrorTypeNotFranzGo(t, spanAttrs(spanNamed(t, tel, sendSpan))["error.type"].AsString())
		})
	})

	t.Run("while the cluster answers with a retriable error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			answerProduce(c, kerr.NotEnoughReplicas.Code, always)

			ps := producerSettings(c.ListenAddrs()...)
			ps.DeliveryTimeout = 2 * time.Second
			var err error
			tel := humustest.NewTelemetry()
			producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
				err = p.Produce(ctx, value(0))
				return nil
			})

			require.ErrorIs(t, err, kafka.ErrDeliveryTimeout)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			requireNoFranzGo(t, err)
			requireErrorTypeNotFranzGo(t, spanAttrs(spanNamed(t, tel, sendSpan))["error.type"].AsString())
		})
	})
}

func TestProducer_a_closed_Producer_fails_records_with_ErrProducerClosed(t *testing.T) {
	t.Run("a Produce after the OnStop function has run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)

			after := make(chan struct{})
			produced := make(chan error, 1)
			tel := humustest.NewTelemetry()
			_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(_ context.Context, p *kafka.Producer) error {
				go func() {
					<-after
					produced <- p.Produce(context.Background(), value(0))
				}()
				return nil
			}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))

			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
			close(after)
			err := <-produced
			require.ErrorIs(t, err, kafka.ErrProducerClosed)
			requireNoFranzGo(t, err)
			requireErrorTypeNotFranzGo(t, semconv.ErrorType(err).Value.AsString())
		})
	})

	t.Run("a record the grace period cut short", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			held, _ := holdProduce(t, c) // released when the test ends

			produced := make(chan error, 1)
			tel := humustest.NewTelemetry()
			_, status := humustest.Start(t, produces(producerSettings(c.ListenAddrs()...), func(_ context.Context, p *kafka.Producer) error {
				go func() { produced <- p.Produce(context.Background(), value(0)) }()
				<-held
				return nil
			}),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
				humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s"}),
			)

			st := <-status
			require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
			err := <-produced
			require.ErrorIs(t, err, kafka.ErrProducerClosed)
			requireNoFranzGo(t, err)
			requireErrorTypeNotFranzGo(t, spanAttrs(spanNamed(t, tel, sendSpan))["error.type"].AsString())
		})
	})
}
