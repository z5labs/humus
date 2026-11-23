// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"fmt"

	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/aws"
	"github.com/twmb/franz-go/pkg/sasl/oauth"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// SASLMechanism is a SASL mechanism (RFC 4422) that the Service or the
// Producer authenticates to each broker with. Plain, ScramSHA256,
// ScramSHA512, OAuthBearer and AWSMSKIAM return the common ones. An end
// user who needs another implements it.
type SASLMechanism interface {
	// Name is the mechanism's name as the brokers know it, such as
	// SCRAM-SHA-512.
	Name() string

	// Authenticate starts an exchange with the broker at addr, host:port.
	// It returns the session, and the first message to send when the
	// mechanism sends first. ctx lasts for the whole exchange.
	Authenticate(ctx context.Context, addr string) (SASLSession, []byte, error)
}

// SASLSession is one exchange with one broker.
type SASLSession interface {
	// Challenge is given the broker's last message. It reports whether the
	// exchange is done and returns the next message to send. When it is
	// done, that message is a last one, which gets no answer. An error
	// ends the exchange.
	Challenge(msg []byte) (done bool, response []byte, err error)
}

// Plain returns PLAIN (RFC 4616), which sends user and password to the
// broker as they are, so it belongs over TLS.
func Plain(user, password string) SASLMechanism {
	return clientMechanism{plain.Auth{User: user, Pass: password}.AsMechanism()}
}

// ScramSHA256 returns SCRAM-SHA-256 (RFC 7677), which proves the password
// to the broker without sending it.
func ScramSHA256(user, password string) SASLMechanism {
	return clientMechanism{scram.Auth{User: user, Pass: password}.AsSha256Mechanism()}
}

// ScramSHA512 returns SCRAM-SHA-512, which is ScramSHA256 with SHA-512.
func ScramSHA512(user, password string) SASLMechanism {
	return clientMechanism{scram.Auth{User: user, Pass: password}.AsSha512Mechanism()}
}

// OAuthBearer returns OAUTHBEARER (RFC 7628), which sends the broker a bearer
// token. OAuthBearer calls token for each connection, so a token that
// expires is refreshed. An error from token fails that connection.
func OAuthBearer(token func(ctx context.Context) (OAuthToken, error)) SASLMechanism {
	return clientMechanism{oauth.Oauth(func(ctx context.Context) (oauth.Auth, error) {
		t, err := token(ctx)
		if err != nil {
			return oauth.Auth{}, err
		}
		return oauth.Auth{Token: t.Token, Extensions: t.Extensions}, nil
	})}
}

// OAuthToken is a bearer token, with the SASL extensions some brokers
// require, such as Confluent Cloud's logicalCluster.
type OAuthToken struct {
	Token      string
	Extensions map[string]string
}

// AWSMSKIAM returns AWS_MSK_IAM, Amazon MSK's IAM authentication, which signs
// a request to connect to the cluster with AWS Signature Version 4. The
// region it signs for is the one in the broker's host name, such as
// us-east-1 in b-1.orders.abc123.c2.kafka.us-east-1.amazonaws.com; a host
// name with none, such as a proxy's, is signed for the region in the
// AWS_REGION or AWS_DEFAULT_REGION environment variable. AWSMSKIAM calls
// credentials for each connection, so temporary credentials are refreshed.
// An error from credentials fails that connection.
func AWSMSKIAM(credentials func(ctx context.Context) (AWSCredentials, error)) SASLMechanism {
	return clientMechanism{aws.ManagedStreamingIAM(func(ctx context.Context) (aws.Auth, error) {
		c, err := credentials(ctx)
		if err != nil {
			return aws.Auth{}, err
		}
		return aws.Auth{AccessKey: c.AccessKey, SecretKey: c.SecretKey, SessionToken: c.SessionToken}, nil
	})}
}

// AWSCredentials sign Amazon MSK's IAM authentication.
type AWSCredentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
}

// clientMechanism is one of the client's own mechanisms, as a SASLMechanism.
// The client's Session has SASLSession's method, so it is one.
type clientMechanism struct {
	m sasl.Mechanism
}

func (c clientMechanism) Name() string { return c.m.Name() }

func (c clientMechanism) Authenticate(ctx context.Context, addr string) (SASLSession, []byte, error) {
	return c.m.Authenticate(ctx, addr)
}

// clientSASL returns m as the client's mechanism: the client's own one
// that m holds, or an adapter onto any other, such as an end user's.
func clientSASL(m SASLMechanism) sasl.Mechanism {
	if c, ok := m.(clientMechanism); ok {
		return c.m
	}
	return saslAdapter{m: m}
}

// saslAdapter is an end user's SASLMechanism as the client's mechanism. A
// SASLSession has the client's Session's method, so it is one.
type saslAdapter struct {
	m SASLMechanism
}

func (a saslAdapter) Name() string { return a.m.Name() }

func (a saslAdapter) Authenticate(ctx context.Context, addr string) (sasl.Session, []byte, error) {
	s, first, err := a.m.Authenticate(ctx, addr)
	if err != nil {
		return nil, nil, err
	}
	if s == nil {
		// The client would call Challenge on it.
		return nil, nil, fmt.Errorf("kafka: the %s SASLMechanism returned no SASLSession", a.m.Name())
	}
	return s, first, nil
}
