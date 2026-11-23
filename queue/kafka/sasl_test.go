// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"

	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/kafka"
)

// saslMechanism is a mechanism the SASL tests authenticate a Service and a
// Producer with: name is the one kfake knows it by, and of returns it for a
// user and a password.
type saslMechanism struct {
	name string
	of   func(user, password string) kafka.SASLMechanism
}

// saslMechanisms are the constructors kfake can check, and an end user's
// own PLAIN, which shows that SASLMechanism alone is enough.
var saslMechanisms = []struct {
	test string
	saslMechanism
}{
	{"Plain", saslMechanism{"PLAIN", kafka.Plain}},
	{"ScramSHA256", saslMechanism{"SCRAM-SHA-256", kafka.ScramSHA256}},
	{"ScramSHA512", saslMechanism{"SCRAM-SHA-512", kafka.ScramSHA512}},
	{"an end user's own PLAIN", saslMechanism{"PLAIN", func(user, password string) kafka.SASLMechanism {
		return ownPlain{user: user, password: password}
	}}},
}

// ownPlain is an end user's own PLAIN (RFC 4616), written against
// SASLMechanism alone.
type ownPlain struct {
	user, password string
}

func (ownPlain) Name() string { return "PLAIN" }

func (p ownPlain) Authenticate(context.Context, string) (kafka.SASLSession, []byte, error) {
	// authzid NUL authcid NUL passwd, with no authzid.
	return ownPlainSession{}, []byte("\x00" + p.user + "\x00" + p.password), nil
}

// ownPlainSession is PLAIN's one message: the broker's answer ends it.
type ownPlainSession struct{}

func (ownPlainSession) Challenge([]byte) (bool, []byte, error) { return true, nil, nil }

// saslCluster is cluster requiring SASL: user authenticates with password
// over mechanism, and the test's own clients, which seed and read the
// cluster, with the credentials seed gives them.
func saslCluster(t *testing.T, h *humustest.Host, mechanism, user, password string) (c *kfake.Cluster, seed kgo.Opt) {
	t.Helper()
	admin, adminPassword := rand.Text(), rand.Text()
	c = cluster(t, h, 1,
		kfake.EnableSASL(),
		kfake.Superuser(mechanism, user, password),
		kfake.Superuser("PLAIN", admin, adminPassword),
	)
	return c, kgo.SASL(plain.Auth{User: admin, Pass: adminPassword}.AsMechanism())
}

func TestSASLMechanism_Name_is_the_name_the_brokers_know(t *testing.T) {
	t.Parallel()

	never := func(t *testing.T) {
		t.Helper()
		t.Error("Name called the mechanism's function")
	}
	user, password := rand.Text(), rand.Text()
	cases := map[string]kafka.SASLMechanism{
		"PLAIN":         kafka.Plain(user, password),
		"SCRAM-SHA-256": kafka.ScramSHA256(user, password),
		"SCRAM-SHA-512": kafka.ScramSHA512(user, password),
		"OAUTHBEARER": kafka.OAuthBearer(func(context.Context) (kafka.OAuthToken, error) {
			never(t)
			return kafka.OAuthToken{}, nil
		}),
		"AWS_MSK_IAM": kafka.AWSMSKIAM(func(context.Context) (kafka.AWSCredentials, error) {
			never(t)
			return kafka.AWSCredentials{}, nil
		}),
	}
	for want, m := range cases {
		require.Equal(t, want, m.Name())
	}
}

func TestOAuthBearer_sends_the_token_it_gets_for_each_connection(t *testing.T) {
	t.Parallel()

	tokens := []string{rand.Text(), rand.Text(), rand.Text()}
	var calls int
	m := kafka.OAuthBearer(func(context.Context) (kafka.OAuthToken, error) {
		calls++
		return kafka.OAuthToken{
			Token:      tokens[calls-1],
			Extensions: map[string]string{"logicalCluster": "lkc-1", "identityPoolId": "pool-1"},
		}, nil
	})

	for i, token := range tokens {
		_, first, err := m.Authenticate(t.Context(), "broker-1:9092")
		require.NoError(t, err)
		require.Equal(t, i+1, calls, "token is called once for each connection")
		// RFC 7628's client response: the GS2 header, then the token and
		// each extension as key=value, each ended by %x01, and a last %x01.
		want := "n,,\x01auth=Bearer " + token + "\x01identityPoolId=pool-1\x01logicalCluster=lkc-1\x01\x01"
		require.Equal(t, want, string(first))
	}
}

func TestOAuthBearer_an_error_from_token_fails_the_connection(t *testing.T) {
	t.Parallel()

	refused := errors.New("the identity provider is down")
	m := kafka.OAuthBearer(func(context.Context) (kafka.OAuthToken, error) {
		return kafka.OAuthToken{}, refused
	})
	_, _, err := m.Authenticate(t.Context(), "broker-1:9092")
	require.ErrorIs(t, err, refused)
}

// mskBroker is a broker of an Amazon MSK cluster in us-east-1, as its IAM
// listener's bootstrap string names it.
const mskBroker = "b-1.orders.abc123.c2.kafka.us-east-1.amazonaws.com:9098"

func TestAWSMSKIAM_signs_a_connect_request_with_the_credentials_it_gets_for_each_connection(t *testing.T) {
	t.Parallel()

	creds := make([]kafka.AWSCredentials, 3)
	for i := range creds {
		creds[i] = kafka.AWSCredentials{AccessKey: rand.Text(), SecretKey: rand.Text(), SessionToken: rand.Text()}
	}
	var calls int
	m := kafka.AWSMSKIAM(func(context.Context) (kafka.AWSCredentials, error) {
		calls++
		return creds[calls-1], nil
	})

	for i, c := range creds {
		before := time.Now().UTC()
		session, first, err := m.Authenticate(t.Context(), mskBroker)
		require.NoError(t, err)
		require.Equal(t, i+1, calls, "credentials is called once for each connection")
		requireSignedConnect(t, first, c, before)

		// The broker's answer, a JSON document, ends the exchange.
		done, last, err := session.Challenge([]byte(`{"version":"2020_10_22","request-id":"1"}`))
		require.NoError(t, err)
		require.True(t, done)
		require.Empty(t, last)
	}
}

// requireSignedConnect checks that first is MSK's IAM authentication
// payload for a request to connect to mskBroker's cluster, signed with
// creds with AWS Signature Version 4 at or after before.
func requireSignedConnect(t *testing.T, first []byte, creds kafka.AWSCredentials, before time.Time) {
	t.Helper()
	var fields map[string]string
	require.NoError(t, json.Unmarshal(first, &fields))

	host := strings.TrimSuffix(mskBroker, ":9098")
	require.Equal(t, "2020_10_22", fields["version"])
	require.Equal(t, host, fields["host"])
	require.NotEmpty(t, fields["user-agent"])
	require.Equal(t, "kafka-cluster:Connect", fields["action"])
	require.Equal(t, "AWS4-HMAC-SHA256", fields["x-amz-algorithm"])
	require.Equal(t, "host", fields["x-amz-signedheaders"])
	require.Equal(t, "900", fields["x-amz-expires"])
	require.Equal(t, creds.SessionToken, fields["x-amz-security-token"])

	timestamp := fields["x-amz-date"]
	signed, err := time.Parse("20060102T150405Z", timestamp)
	require.NoError(t, err)
	require.False(t, signed.Before(before.Truncate(time.Second)), "signed at %s, before %s", signed, before)
	date := timestamp[:8]
	scope := date + "/us-east-1/kafka-cluster/aws4_request"
	require.Equal(t, creds.AccessKey+"/"+scope, fields["x-amz-credential"])

	// The signature is SigV4's, of a GET of / on the host whose query is
	// every field the request signs, under a key derived from SecretKey.
	query := url.Values{}
	for name, field := range map[string]string{
		"Action":               "action",
		"X-Amz-Algorithm":      "x-amz-algorithm",
		"X-Amz-Credential":     "x-amz-credential",
		"X-Amz-Date":           "x-amz-date",
		"X-Amz-Expires":        "x-amz-expires",
		"X-Amz-SignedHeaders":  "x-amz-signedheaders",
		"X-Amz-Security-Token": "x-amz-security-token",
	} {
		query.Set(name, fields[field])
	}
	emptyBody := sha256.Sum256(nil)
	canonical := "GET\n/\n" + strings.ReplaceAll(query.Encode(), "+", "%20") + "\nhost:" + host + "\n\nhost\n" + hex.EncodeToString(emptyBody[:])
	canonicalHash := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + timestamp + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	key := []byte("AWS4" + creds.SecretKey)
	for _, part := range []string{date, "us-east-1", "kafka-cluster", "aws4_request", toSign} {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(part))
		key = mac.Sum(nil)
	}
	require.Equal(t, hex.EncodeToString(key), fields["x-amz-signature"])
}

func TestAWSMSKIAM_an_error_from_credentials_fails_the_connection(t *testing.T) {
	t.Parallel()

	refused := errors.New("the instance has no role")
	m := kafka.AWSMSKIAM(func(context.Context) (kafka.AWSCredentials, error) {
		return kafka.AWSCredentials{}, refused
	})
	_, _, err := m.Authenticate(t.Context(), mskBroker)
	require.ErrorIs(t, err, refused)
}

// noSession is an end user's mechanism that, wrongly, starts no session.
type noSession struct{}

func (noSession) Name() string { return "NO-SESSION" }

func (noSession) Authenticate(context.Context, string) (kafka.SASLSession, []byte, error) {
	return nil, nil, nil
}

func TestClientSASL_an_end_users_mechanism_that_starts_no_session_fails_the_connection(t *testing.T) {
	t.Parallel()

	m := kafka.ClientSASL(noSession{})
	require.Equal(t, "NO-SESSION", m.Name())
	session, _, err := m.Authenticate(t.Context(), "broker-1:9092")
	require.Nil(t, session)
	require.EqualError(t, err, "kafka: the NO-SESSION SASLMechanism returned no SASLSession")
}

func TestClientSASL_an_end_users_mechanism_passes_on_its_session_and_errors(t *testing.T) {
	t.Parallel()

	user, password := rand.Text(), rand.Text()
	m := kafka.ClientSASL(ownPlain{user: user, password: password})
	require.Equal(t, "PLAIN", m.Name())
	session, first, err := m.Authenticate(t.Context(), "broker-1:9092")
	require.NoError(t, err)
	require.Equal(t, "\x00"+user+"\x00"+password, string(first))
	done, last, err := session.Challenge(nil)
	require.NoError(t, err)
	require.True(t, done)
	require.Empty(t, last)

	refused := errors.New("the vault is sealed")
	_, _, err = kafka.ClientSASL(failing{err: refused}).Authenticate(t.Context(), "broker-1:9092")
	require.ErrorIs(t, err, refused)
}

// failing is an end user's mechanism that can't start an exchange.
type failing struct{ err error }

func (failing) Name() string { return "FAILING" }

func (f failing) Authenticate(context.Context, string) (kafka.SASLSession, []byte, error) {
	return nil, nil, f.err
}
