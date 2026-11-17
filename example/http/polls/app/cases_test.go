// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/polls/cases"
	"github.com/z5labs/humus/httpserver/httpservertest"
)

var replicaAddrs = []string{addr, "polls.internal:9443"}

func startReplicas(t *testing.T) (cases.Target, func()) {
	t.Helper()
	f := newFixture(t)
	db := newFakeDatabase()

	var (
		running  []*service
		replicas []*url.URL
	)
	for _, a := range replicaAddrs {
		r := f
		r.env = maps.Clone(f.env)
		r.env["ADDR"] = a
		p, status := r.start(t, db.init)
		select {
		case <-f.h.Listening(a):
		case st := <-status:
			t.Fatalf("the replica at %s exited %d: %v", a, st.Code(), st.Cause())
		}
		running = append(running, &service{f: r, p: p, status: status})
		replicas = append(replicas, &url.URL{Scheme: "https", Host: a})
	}

	client := httpservertest.NewClient(f.h, f.ca.Pool())
	target := cases.Target{
		Replicas: replicas,
		Browser: func() *http.Client {
			jar, err := cookiejar.New(nil)
			require.NoError(t, err)
			b := *client
			b.Jar = jar
			return &b
		},
		Timeout: time.Minute,
	}
	return target, func() {
		for _, s := range running {
			s.stop(t)
		}
	}
}

func TestInit_passes_the_shared_cases_on_two_replicas_sharing_a_Store(t *testing.T) {
	for _, c := range cases.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				target, stop := startReplicas(t)
				c.Run(t, target)
				stop()
			})
		})
	}
}
