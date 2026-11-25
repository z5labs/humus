// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/fit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/http/httpclient"
	"github.com/z5labs/humus/http/httpserver"
	"github.com/z5labs/humus/http3/http3client"
	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
)

// The tests in this file run each case twice, each in a synctest bubble of
// its own: once through httpclient, over HTTP/2 and TCP, and once through
// http3client, over HTTP/3 and UDP. The dependency at payments:8443 is one
// App that serves the case's handler with httpserver and http3server, on
// one port number. The two runs must come out the same: the same answers
// and errors, the same requests seen by the dependency, the same time
// taken, and the same counts in humus's instruments. Each run's outcome is
// compared outside its bubble.

// scenarioKey is the baggage member the tests' failure scenarios are in.
const scenarioKey = "acme.fault"

// paymentsPoint is the injection point of the payments dependency.
const paymentsPoint = "http:payments:8443"

// aCase is one case: the handler the dependency serves, the client's
// Settings, as httpclient's, and the calls.
type aCase struct {
	name string

	// handler returns the dependency's handler, which counts each request
	// it serves in hits.
	handler func(hits *atomic.Int32) http.Handler

	// settings changes the client's Settings, which start as
	// paymentsSettings has them.
	settings func(s *httpclient.Settings)

	// calls is how many calls are made, one after another.
	calls int

	// scenario, if it is set, is carried by every call.
	scenario *fit.Scenario

	// between, if it is set, runs after call i, before the next.
	between func(i int)

	// want is each call's answer, or its error's kind.
	want []string
}

// outcome is what one run of a case recorded.
type outcome struct {
	// results is each call's answer, or its error's kind.
	results []string

	// hits is how many requests the dependency served.
	hits int32

	// took is how long each call took, to the second.
	took []time.Duration

	// The counts of humus's instruments, by their attribute.
	decisions, calls, transitions map[string]int64
}

// transport is a client of the dependency to run a case through.
type transport struct {
	name  string
	scope string
	build func(s *humus.Setup, h humus.Host, settings httpclient.Settings) (*http.Client, error)
}

var transports = []transport{
	{
		name:  "httpclient",
		scope: "github.com/z5labs/humus/http/httpclient",
		build: func(s *humus.Setup, h humus.Host, settings httpclient.Settings) (*http.Client, error) {
			return httpclient.NewClient(s, h, settings)
		},
	},
	{
		name:  "http3client",
		scope: scope,
		build: func(s *humus.Setup, h humus.Host, settings httpclient.Settings) (*http.Client, error) {
			return http3client.NewClient(s, h, http3Settings(settings))
		},
	},
}

// http3Settings returns http3client's Settings of the same values as s,
// with http3client's default Classify.
func http3Settings(s httpclient.Settings) http3client.Settings {
	return http3client.Settings{
		BaseURL:         s.BaseURL,
		TLS:             s.TLS,
		Timeout:         s.Timeout,
		AttemptTimeout:  s.AttemptTimeout,
		MaxAttempts:     s.MaxAttempts,
		BackoffBase:     s.BackoffBase,
		BackoffMax:      s.BackoffMax,
		RetryBudget:     s.RetryBudget,
		Classify:        http3client.DefaultSettings().Classify,
		Breaker:         s.Breaker,
		BreakerPolicy:   s.BreakerPolicy,
		ForwardScenario: s.ForwardScenario,
	}
}

// startDependency serves handler at payments:8443, over HTTP/2 and
// HTTP/1.1 with httpserver, and over HTTP/3 with http3server. It returns a
// function that stops the dependency.
func startDependency(t *testing.T, h *humustest.Host, ca *humustest.CA, handler http.Handler) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, "payments")
	p, status := humustest.Start(t, h.Host(), func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
		tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		tcp := httpserver.DefaultSettings()
		tcp.Addr, tcp.TLS, tcp.WriteTimeout = ":8443", tlsConfig, time.Minute
		udp := http3server.DefaultSettings()
		udp.Addr, udp.TLS, udp.WriteTimeout = ":8443", tlsConfig, time.Minute
		h2, err := httpserver.NewService(ctx, s, h, tcp, handler)
		if err != nil {
			return nil, err
		}
		h3, err := http3server.NewService(ctx, s, h, udp, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{h2, h3}, nil
	}, humustest.WithApp(http3server.Defaults()...))
	<-h.Listening(paymentsAddr)
	synctest.Wait()
	return func() {
		p.Signal(syscall.SIGTERM)
		<-status
	}
}

// run runs c through tr, in a bubble of its own, and returns what it
// recorded.
func run(t *testing.T, c aCase, tr transport) (out outcome) {
	t.Helper()
	t.Run(tr.name, func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			var hits atomic.Int32
			stop := startDependency(t, h, ca, c.handler(&hits))
			defer stop()

			codec, err := jsonscenario.New(scenarioKey)
			require.NoError(t, err)
			settings := httpclient.DefaultSettings()
			settings.BaseURL = mustParse(t, "https://"+paymentsAddr)
			settings.TLS = &tls.Config{RootCAs: ca.Pool()}
			if c.settings != nil {
				c.settings(&settings)
			}

			tel := humustest.NewTelemetry()
			p, status := humustest.Start(t, h.Host(), func(_ context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
				client, err := tr.build(s, h, settings)
				if err != nil {
					return nil, err
				}
				return []humus.Service{job.NewService(s, func(ctx context.Context) error {
					if c.scenario != nil {
						ctx, err = fit.ContextWithScenario(ctx, codec, *c.scenario)
						if err != nil {
							return err
						}
					}
					for i := range c.calls {
						start := time.Now()
						r, err := get(ctx, client, "https://payments:8443/")
						out.took = append(out.took, time.Since(start).Round(time.Second))
						out.results = append(out.results, kind(r, err))
						if c.between != nil {
							c.between(i)
						}
					}
					return nil
				})}, nil
			}, humustest.WithApp(humus.WithTelemetry(tel.Builder()), humus.WithFaults(func(context.Context, *humus.Setup, humus.Host) (fault.Settings, error) {
				f := fault.DefaultSettings()
				f.Decoder = codec
				return f, nil
			})))
			requireExitZero(t, <-status)
			require.Empty(t, p.Stderr())

			out.hits = hits.Load()
			out.decisions = countsIn(t, tel, tr.scope, "humus.retry.decisions", "humus.retry.decision")
			out.calls = countsIn(t, tel, tr.scope, "humus.circuit.calls", "humus.circuit.outcome")
			out.transitions = countsIn(t, tel, tr.scope, "humus.circuit.transitions", "humus.circuit.state")
		})
	})
	return out
}

// kind is a call's answer, or the kind of its error.
func kind(r response, err error) string {
	var injected *fault.InjectedError
	switch {
	case err == nil:
		return fmt.Sprintf("%d %s", r.code, r.body)
	case errors.Is(err, circuit.ErrRejected):
		return "rejected"
	case errors.As(err, &injected):
		return "injected"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	default:
		return "error: " + err.Error()
	}
}

// answers returns a handler that answers its requests with codes, one
// after another, and with the last of them once they run out.
func answers(codes ...int) func(hits *atomic.Int32) http.Handler {
	return func(hits *atomic.Int32) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := int(hits.Add(1))
			code := codes[min(n, len(codes))-1]
			w.WriteHeader(code)
			_, _ = io.WriteString(w, http.StatusText(code))
		})
	}
}

// hangs returns a handler whose first n requests wait for their context to
// end, and whose later ones answer ok at once.
func hangs(n int32) func(hits *atomic.Int32) http.Handler {
	return func(hits *atomic.Int32) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hits.Add(1) <= n {
				<-r.Context().Done()
				return
			}
			_, _ = io.WriteString(w, "ok")
		})
	}
}

func TestClient_behaves_as_httpclient_does(t *testing.T) {
	for _, c := range []aCase{
		{
			name:    "retries a 503 until it succeeds",
			handler: answers(503, 503, 200),
			calls:   1,
			want:    []string{"200 OK"},
		},
		{
			name:    "stops on a 500",
			handler: answers(500),
			calls:   1,
			want:    []string{"500 Internal Server Error"},
		},
		{
			name:    "stops retrying once the budget is spent",
			handler: answers(503),
			settings: func(s *httpclient.Settings) {
				s.RetryBudget = retry.BudgetConfig{MaxTokens: 4, TokenRatio: 0.001}
				s.Breaker.Window = circuit.MaxCount
				s.BreakerPolicy.Trip = func(circuit.Counts) bool { return false }
			},
			calls: 4,
			want:  slices.Repeat([]string{"503 Service Unavailable"}, 4),
		},
		{
			name:    "the breaker opens, rejects, probes and closes",
			handler: answers(502, 502, 502, 502, 502, 200),
			settings: func(s *httpclient.Settings) {
				s.MaxAttempts = 1
			},
			calls: 10,
			between: func(i int) {
				if i == 5 {
					time.Sleep(10 * time.Second) // OpenFor
				}
			},
			want: slices.Concat(
				slices.Repeat([]string{"502 Bad Gateway"}, 5),
				[]string{"rejected"},
				slices.Repeat([]string{"200 OK"}, 4),
			),
		},
		{
			name:    "Timeout ends the call",
			handler: hangs(10),
			settings: func(s *httpclient.Settings) {
				s.Timeout = 3 * time.Second
				s.AttemptTimeout = 2 * time.Second
			},
			calls: 1,
			want:  []string{"deadline exceeded"},
		},
		{
			name:    "AttemptTimeout ends an attempt, which is retried",
			handler: hangs(1),
			settings: func(s *httpclient.Settings) {
				s.AttemptTimeout = time.Second
			},
			calls: 1,
			want:  []string{"200 ok"},
		},
		{
			name:     "an injected abort fails each attempt, sending nothing",
			handler:  answers(200),
			calls:    2,
			scenario: &fit.Scenario{Rules: []fit.Rule{{Point: paymentsPoint, Fault: fit.Fault{Abort: true}}}},
			want:     []string{"injected", "injected"},
		},
		{
			name:     "an injected delay delays each attempt",
			handler:  answers(200),
			calls:    2,
			scenario: &fit.Scenario{Rules: []fit.Rule{{Point: paymentsPoint, Fault: fit.Fault{Delay: 2 * time.Second}}}},
			want:     []string{"200 OK", "200 OK"},
		},
		{
			name:    "an injected delay past AttemptTimeout fails the attempt",
			handler: answers(200),
			settings: func(s *httpclient.Settings) {
				s.AttemptTimeout = time.Second
			},
			calls:    1,
			scenario: &fit.Scenario{Rules: []fit.Rule{{Point: paymentsPoint, Fault: fit.Fault{Delay: 3 * time.Second}}}},
			want:     []string{"injected"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h2 := run(t, c, transports[0])
			h3 := run(t, c, transports[1])
			require.Equal(t, c.want, h2.results, "httpclient's answers")
			require.Equal(t, h2, h3, "http3client behaves as httpclient does")
		})
	}
}
