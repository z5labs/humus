// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"k8s.io/client-go/rest"

	"github.com/z5labs/humus"
)

// The HTTP/2 health check of the Client's transport. They are the values
// client-go's own transports use, which they read from the environment
// variables HTTP2_READ_IDLE_TIMEOUT_SECONDS and HTTP2_PING_TIMEOUT_SECONDS.
// Neither is a setting.
const (
	// pingAfter is how long a connection goes without a frame before the
	// transport sends it a ping.
	pingAfter = 30 * time.Second

	// pingTimeout is how long the transport waits for the ping's answer
	// before it gives up on the connection.
	pingTimeout = 15 * time.Second
)

// The pool of the Client's http.Transport, as http.DefaultTransport keeps
// it, all to the one API server. None of these is a setting.
const (
	maxIdleConns    = 100
	idleConnTimeout = 90 * time.Second
)

// userAgent is the User-Agent of every request from a Config, in place of
// client-go's default, which it builds from os.Args[0].
const userAgent = "humus-kube"

// Connection is how a Client reaches an API server. Every field is a plain
// value.
type Connection struct {
	// Server is the API server's address, host:port. In a pod, it is
	// KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT, joined.
	Server string

	// TLS is the config the API server is dialled with: the cluster's CA in
	// RootCAs, and a client certificate when the client authenticates with
	// one. ServerName defaults to Server's host.
	TLS *tls.Config

	// Token returns the bearer token every request carries, such as the
	// pod's service account token, read through the Host's FS. nil sends
	// none, for a client that authenticates with a certificate. The Client
	// calls it at most once a minute, and keeps its last token while a call
	// fails, so a token the kubelet rotates is picked up within a minute. A
	// call that returns an empty token has failed.
	Token func(ctx context.Context) (string, error)
}

var errNil = errors.New("must not be nil")

// CheckServer reports why addr can't be an API server's address, or nil if
// it can. addr must be host:port, with a host, and a port from 1 to 65535.
// An IPv6 host is in brackets, as net.JoinHostPort writes it.
func CheckServer(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	if host == "" {
		return errors.New("has no host")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return fmt.Errorf("has the port %q, not a number from 1 to 65535", port)
	}
	return nil
}

// CheckTLS rejects a nil config: the API server is only ever dialled over
// TLS, and the config holds the cluster's CA.
func CheckTLS(c *tls.Config) error {
	if c == nil {
		return errNil
	}
	return nil
}

// checkConnection runs every check on c. It reports every failure in one
// error, each naming its field.
func checkConnection(c Connection) error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
		}
	}
	check("Connection.Server", CheckServer(c.Server))
	check("Connection.TLS", CheckTLS(c.TLS))
	return errors.Join(errs...)
}

// Client reaches one API server through the Host.
type Client struct {
	server    string
	transport http.RoundTripper
	conns     *http.Transport
}

// NewClient returns a Client that dials c.Server with s.Host.Dial, and
// traces and measures every request with s.Telemetry. It opens no
// connection.
//
// NewClient runs every check on c, and reports every bad field in one error
// that names each one. It also rejects a nil s, and an s whose Host has no
// Dial, rather than let net/http fall back to dialling the real network. It
// registers a function with s.OnStop that closes the Client's idle
// connections.
//
// The Client's transport runs TLS with a clone of c.TLS, so the end user's
// config is never changed, and the clone's ServerName defaults to
// c.Server's host. It uses no proxy, whatever the environment says. Over
// HTTP/2, it sends a ping after 30s without a frame, and gives up on the
// connection when 15s pass without the answer. Every request carries
// c.Token's bearer token, and every request but a watch is a client span
// from otelhttp, with s.Telemetry's providers and propagator. A watch lasts
// as long as an informer runs, so it has no span.
//
// Every request is an injection point of s.Faults, named after what it does
// to which resource, such as kube:deployments.apps:patch, and the Client
// strips the failure scenario from every request it sends. In a reconcile
// that carries a scenario, it sets the scenario's annotation on each child
// the reconcile applies. The package doc says how.
func NewClient(s *humus.Setup, c Connection) (*Client, error) {
	if s == nil {
		return nil, errors.New("kube: NewClient: the Setup must not be nil")
	}
	err := checkConnection(c)
	if s.Host.Dial == nil {
		err = errors.Join(err, errors.New("the Setup's Host.Dial must not be nil"))
	}
	if err != nil {
		return nil, fmt.Errorf("kube: NewClient: %w", err)
	}

	tlsConfig := c.TLS.Clone()
	if tlsConfig.ServerName == "" {
		// CheckServer has already split it.
		host, _, _ := net.SplitHostPort(c.Server)
		tlsConfig.ServerName = host
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	tr := &http.Transport{
		DialContext:         s.Host.Dial,
		Proxy:               nil,
		TLSClientConfig:     tlsConfig,
		Protocols:           &protocols,
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConns,
		IdleConnTimeout:     idleConnTimeout,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: pingAfter,
			PingTimeout:     pingTimeout,
		},
	}

	// The layers, outermost first: otelhttp, then failure injection, then
	// the token, then the http.Transport. The token is added inside the
	// span, so a Token call that fails is the span's error. Failure
	// injection is inside the span too, so a delay is in it and its
	// evaluation is an event on it, and it is before the token, so an abort
	// calls no Token. Every provider and the propagator are passed, so
	// otelhttp never falls back to OpenTelemetry's globals.
	var next http.RoundTripper = tr
	if c.Token != nil {
		next = &tokenTransport{token: c.Token, next: tr}
	}
	next = injectFaults{faults: s.Faults, propagator: s.Telemetry.Propagator, next: next}
	instrumented := otelhttp.NewTransport(next,
		otelhttp.WithTracerProvider(s.Telemetry.TracerProvider),
		otelhttp.WithMeterProvider(s.Telemetry.MeterProvider),
		otelhttp.WithPropagators(s.Telemetry.Propagator),
		otelhttp.WithFilter(notWatch),
		otelhttp.WithSpanNameFormatter(spanName),
	)

	s.OnStop(func(context.Context) error {
		tr.CloseIdleConnections()
		return nil
	})
	return &Client{
		server:    c.Server,
		transport: instrumented,
		conns:     tr,
	}, nil
}

// Config returns a rest.Config for client-go's clients, such as the
// clientset kubernetes.NewForConfig returns. They share the Client's
// transport: its dialler, its credentials and its telemetry.
//
// Each call returns a new rest.Config, so changing one changes no other. Its
// Transport is the Client's, and it sets no TLS options and no credentials
// of its own, which client-go refuses beside a Transport. Its QPS is
// negative, so client-go limits no client's rate: the API server's priority
// and fairness limits each client itself. Its UserAgent is set, since
// client-go's default is built from the process's arguments.
func (c *Client) Config() *rest.Config {
	return &rest.Config{
		Host:      "https://" + c.server,
		Transport: c.transport,
		QPS:       -1,
		UserAgent: userAgent,
	}
}

// spanName names a request's span after its method, such as GET, as the
// HTTP semantic conventions name a client span that has no URL template.
// An empty method is GET, as net/http sends it.
func spanName(_ string, r *http.Request) string {
	if r.Method == "" {
		return http.MethodGet
	}
	return r.Method
}

// notWatch reports whether r is anything but a watch, which client-go sends
// as a GET with the query parameter watch=true. otelhttp traces and
// measures only the requests it reports true for.
func notWatch(r *http.Request) bool {
	return !isWatch(r)
}
