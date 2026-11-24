// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/polls/store/storetest"
	"github.com/z5labs/humus/http/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	addr   = "polls.internal:8443"
	base   = "https://" + addr
	dbHost = "db.internal:5432"
)

func pemPair(t *testing.T, c *humustest.Certificate) (certPEM, keyPEM []byte) {
	t.Helper()
	cert := c.TLS()
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}

func caPEM(cert *humustest.Certificate) []byte {
	chain := cert.TLS().Certificate
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain[len(chain)-1]})
}

func withCertificate(c *http.Client, cert *humustest.Certificate) *http.Client {
	c.Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{cert.TLS()}
	return c
}

type fixture struct {
	h          *humustest.Host
	ca         *humustest.CA
	trust      *humustest.CA
	cert       *humustest.Certificate
	dbPassword string
	files      map[string][]byte
	env        map[string]string
	telemetry  telemetry.Builder
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ca := humustest.NewCA(t)
	trust := humustest.NewCA(t)
	cert := ca.Issue(t, "polls.internal")
	certPEM, keyPEM := pemPair(t, cert)
	_, otherKey := pemPair(t, ca.Issue(t, "polls.internal"))
	password := rand.Text()
	files := map[string][]byte{
		"tls/tls.crt":   certPEM,
		"tls/tls.key":   keyPEM,
		"other/tls.key": otherKey,
		"database/url":  []byte("postgres://polls:" + password + "@" + dbHost + "/polls?sslmode=disable\n"),
		"fault/ca.crt":  caPEM(trust.Issue(t, "cases")),
	}
	return fixture{
		h:          humustest.NewHost(t, hosttest.WithFiles(files)),
		ca:         ca,
		trust:      trust,
		cert:       cert,
		dbPassword: password,
		files:      files,
		env: map[string]string{
			"ADDR":              addr,
			"TLS_CERT_FILE":     "tls/tls.crt",
			"TLS_KEY_FILE":      "tls/tls.key",
			"DATABASE_URL_FILE": "database/url",
		},
		telemetry: telemetry.NoopBuilder,
	}
}

func (f fixture) start(t *testing.T, init func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error)) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	return httpservertest.Start(t, f.h.Host(), init,
		humustest.WithApp(humus.WithTelemetry(f.telemetry), humus.WithFaults(Faults)),
		humustest.WithEnv(f.env),
	)
}

func (f fixture) exit(t *testing.T) bedrock.ExitStatus {
	t.Helper()
	_, status := f.start(t, Init)
	return <-status
}

type fakeDatabase struct {
	*storetest.Store

	mu         sync.Mutex
	down       bool
	pings      int
	listenerUp bool
	started    time.Time
	stopped    time.Time
	holding    chan struct{}
	held       chan struct{}
}

func newFakeDatabase() *fakeDatabase {
	return &fakeDatabase{Store: storetest.New(), listenerUp: true}
}

func (db *fakeDatabase) open(_ context.Context, s *humus.Setup, _ humus.Host, _ *pgxpool.Config) (backend, error) {
	return backend{
		store:     fakeStore{db: db, Store: db.WithFaults(s.Faults)},
		results:   db.Store,
		listen:    db.listen,
		reached:   db.ping,
		listening: db.listening,
	}, nil
}

func (db *fakeDatabase) init(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
	return initWith(ctx, s, h, db.open)
}

type fakeStore struct {
	db *fakeDatabase
	*storetest.Store
}

func (s fakeStore) Vote(ctx context.Context, pollID uuid.UUID, optionID int64, voter string) error {
	s.db.mu.Lock()
	holding, held := s.db.holding, s.db.held
	s.db.mu.Unlock()
	if holding != nil {
		close(held)
		<-holding
	}
	return s.Store.Vote(ctx, pollID, optionID, voter)
}

func (db *fakeDatabase) hold() (held <-chan struct{}, release func()) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.holding, db.held = make(chan struct{}), make(chan struct{})
	return db.held, sync.OnceFunc(func() { close(db.holding) })
}

func (db *fakeDatabase) ping(context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.pings++
	if db.down {
		return errors.New("the database is down")
	}
	return nil
}

func (db *fakeDatabase) pinged() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.pings
}

func (db *fakeDatabase) setDown(down bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.down = down
}

func (db *fakeDatabase) setListenerUp(up bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.listenerUp = up
}

func (db *fakeDatabase) listen(ctx context.Context) {
	db.mu.Lock()
	db.started = time.Now()
	db.mu.Unlock()
	<-ctx.Done()
	db.mu.Lock()
	db.stopped = time.Now()
	db.mu.Unlock()
}

func (db *fakeDatabase) listening(context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.started.IsZero() || !db.stopped.IsZero() || !db.listenerUp {
		return errors.New("not listening for votes")
	}
	return nil
}

func (db *fakeDatabase) listened() (started, stopped time.Time) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.started, db.stopped
}

type service struct {
	f      fixture
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
	client *http.Client
}

func start(t *testing.T, init func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error)) *service {
	t.Helper()
	return newFixture(t).serve(t, init)
}

func (f fixture) serve(t *testing.T, init func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error)) *service {
	t.Helper()
	p, status := f.start(t, init)
	select {
	case <-f.h.Listening(addr):
	case st := <-status:
		t.Fatalf("the polls service exited %d: %v", st.Code(), st.Cause())
	}
	return &service{f: f, p: p, status: status, client: httpservertest.NewClient(f.h, f.ca.Pool())}
}

func (s *service) sigterm(t *testing.T) time.Time {
	t.Helper()
	<-s.p.Notified(syscall.SIGTERM)
	at := time.Now()
	s.p.Signal(syscall.SIGTERM)
	return at
}

func (s *service) exited(t *testing.T) {
	t.Helper()
	st := <-s.status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

func (s *service) stop(t *testing.T) {
	t.Helper()
	s.sigterm(t)
	s.exited(t)
}

type response struct {
	status int
	header http.Header
	body   string
	peer   *x509.Certificate
}

func send(t *testing.T, c *http.Client, method, target string, form url.Values, header map[string]string) response {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, target, body)
	require.NoError(t, err)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 2, resp.ProtoMajor)
	return response{status: resp.StatusCode, header: resp.Header, body: string(b), peer: resp.TLS.PeerCertificates[0]}
}

func (s *service) get(t *testing.T, path string) response {
	t.Helper()
	return send(t, s.client, http.MethodGet, base+path, nil, nil)
}

func browser(t *testing.T, c *http.Client) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	b := *c
	b.Jar = jar
	return &b
}

var sameOrigin = map[string]string{"HX-Request": "true", "Sec-Fetch-Site": "same-origin", "Origin": base}

type poll struct {
	id      uuid.UUID
	options []int64
}

func newPoll(t *testing.T, polls *storetest.Store, question string, options ...string) poll {
	t.Helper()
	id, err := polls.CreatePoll(t.Context(), question, options)
	require.NoError(t, err)
	p, err := polls.Poll(t.Context(), id)
	require.NoError(t, err)
	got := poll{id: id}
	for _, o := range p.Options {
		got.options = append(got.options, o.ID)
	}
	return got
}

func (p poll) url(rest string) string {
	return base + "/polls/" + p.id.String() + rest
}

func counts(t *testing.T, polls *storetest.Store, id uuid.UUID) []int64 {
	t.Helper()
	p, err := polls.Poll(t.Context(), id)
	require.NoError(t, err)
	var got []int64
	for _, o := range p.Options {
		got = append(got, o.Votes)
	}
	return got
}

type feed struct {
	events <-chan string
	ended  <-chan feedEnd
}

type feedEnd struct {
	err error
	at  time.Time
}

func open(t *testing.T, c *http.Client, target string) feed {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	events := make(chan string, 16)
	ended := make(chan feedEnd, 1)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		r := bufio.NewReader(resp.Body)
		var block []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				if errors.Is(err, io.EOF) && line == "" && len(block) == 0 {
					err = nil
				}
				ended <- feedEnd{err: err, at: time.Now()}
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case strings.HasPrefix(line, ":"):
				events <- line
			case line != "":
				block = append(block, line)
			default:
				events <- strings.Join(block, "\n")
				block = nil
			}
		}
	}()
	return feed{events: events, ended: ended}
}

func results(counts ...string) string {
	lines := []string{"event: results"}
	for _, c := range counts {
		text, votes, _ := strings.Cut(c, "=")
		lines = append(lines, `data: <li><span class="option">`+text+`</span> <span class="votes">`+votes+`</span></li>`)
	}
	return strings.Join(lines, "\n")
}

var settingNames = []string{"ADDR", "TLS_CERT_FILE", "TLS_KEY_FILE", "DATABASE_URL_FILE"}

func TestInit_without_its_settings_exits_78_naming_each(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := httpservertest.Start(t, humustest.NewHost(t).Host(), Init,
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
		)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		for _, name := range []string{"ADDR", "TLS_CERT_FILE", "DATABASE_URL_FILE"} {
			require.ErrorContains(t, st.Cause(), "config "+name+": not found")
		}
		require.NotContains(t, st.Cause().Error(), "TLS_KEY_FILE", "the key is read only with a certificate")
	})
}

func TestInit_without_a_setting_exits_78_naming_only_it(t *testing.T) {
	for _, name := range settingNames {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				delete(f.env, name)

				st := f.exit(t)
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), "config "+name+": not found")
				for _, other := range settingNames {
					if other != name {
						require.NotContains(t, st.Cause().Error(), "config "+other+":")
					}
				}
			})
		})
	}
}

func TestInit_with_a_bad_setting_exits_78_naming_it(t *testing.T) {
	for _, tc := range []struct{ name, value, file, cause string }{
		{name: "ADDR", value: "polls.internal", cause: "is not host:port"},
		{name: "ADDR", value: "polls.internal:http", cause: "has no port from 0 to 65535"},
		{name: "TLS_CERT_FILE", value: "missing.crt", cause: "missing.crt"},
		{name: "TLS_CERT_FILE", value: "tls/tls.key", cause: "no PEM certificate in the file"},
		{name: "TLS_KEY_FILE", value: "missing.key", cause: "missing.key"},
		{name: "TLS_KEY_FILE", value: "other/tls.key", cause: "tls/tls.crt"},
		{name: "TLS_KEY_FILE", value: "tls/tls.crt", cause: "tls/tls.crt"},
		{name: "READ_HEADER_TIMEOUT", value: "-1s", cause: "must not be negative"},
		{name: "READ_TIMEOUT", value: "5", cause: "missing unit"},
		{name: "WRITE_TIMEOUT", value: "-10s", cause: "must not be negative"},
		{name: "IDLE_TIMEOUT", value: "soon", cause: "invalid duration"},
		{name: "DATABASE_URL_FILE", value: "missing.url", cause: "missing.url"},
		{name: "DATABASE_URL_FILE", value: "bad.url", file: "postgres://polls:" + rand.Text() + "@db.internal:port/polls", cause: "must hold a PostgreSQL URL"},
		{name: "FAULT_TRUST_CA_FILE", value: "missing.crt", cause: "missing.crt"},
		{name: "FAULT_TRUST_CA_FILE", value: "tls/tls.key", cause: "no PEM certificate in the file"},
		{name: "FAULT_MAX_DELAY", value: "0s", cause: "must be positive"},
		{name: "FAULT_MAX_DELAY", value: "-1s", cause: "must be positive"},
		{name: "FAULT_MAX_DELAY", value: "soon", cause: "invalid duration"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				if tc.file != "" {
					f.h = humustest.NewHost(t, hosttest.WithFiles(f.files), hosttest.WithFiles(map[string][]byte{tc.value: []byte(tc.file)}))
				}
				f.env[tc.name] = tc.value

				st := f.exit(t)
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), "config "+tc.name+": ")
				require.ErrorContains(t, st.Cause(), tc.cause)
				require.NotContains(t, st.Cause().Error(), f.dbPassword)
				if tc.file != "" {
					require.NotContains(t, st.Cause().Error(), tc.file)
				}
			})
		})
	}
}

func TestInit_reads_its_files_only_through_the_Host(t *testing.T) {
	for name, file := range map[string]string{
		"TLS_CERT_FILE":       "tls/tls.crt",
		"TLS_KEY_FILE":        "tls/tls.key",
		"DATABASE_URL_FILE":   "database/url",
		"FAULT_TRUST_CA_FILE": "fault/ca.crt",
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				outside := filepath.Join(t.TempDir(), filepath.Base(file))
				require.NoError(t, os.WriteFile(outside, f.files[file], 0o600))
				f.env[name] = outside

				st := f.exit(t)
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), "config "+name+": ")
			})
		})
	}
}

func TestInit_reads_the_database_URL_from_its_file(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cfg *pgxpool.Config
		db := newFakeDatabase()
		s := start(t, func(ctx context.Context, setup *humus.Setup, h humus.Host) ([]humus.Service, error) {
			return initWith(ctx, setup, h, func(ctx context.Context, setup *humus.Setup, h humus.Host, c *pgxpool.Config) (backend, error) {
				cfg = c
				return db.open(ctx, setup, h, c)
			})
		})

		require.Equal(t, "db.internal", cfg.ConnConfig.Host)
		require.EqualValues(t, 5432, cfg.ConnConfig.Port)
		require.Equal(t, "polls", cfg.ConnConfig.User)
		require.Equal(t, s.f.dbPassword, cfg.ConnConfig.Password)
		require.Equal(t, "polls", cfg.ConnConfig.Database)

		s.stop(t)
	})
}

func TestInit_serves_its_Site_with_the_settings_timeouts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase()
		p := newPoll(t, db.Store, "Tabs or spaces?", "Tabs", "Spaces")
		f := newFixture(t)
		f.env["WRITE_TIMEOUT"] = "3s"
		s := f.serve(t, db.init)

		home := s.get(t, "/")
		require.Equal(t, http.StatusOK, home.status)
		require.Contains(t, home.header.Get("Content-Security-Policy"), "default-src 'self'")
		require.Contains(t, home.body, "<!DOCTYPE html>")

		page := s.get(t, "/polls/"+p.id.String())
		require.Equal(t, http.StatusOK, page.status)
		require.Contains(t, page.body, "<h1>Tabs or spaces?</h1>")

		crossSite := send(t, browser(t, s.client), http.MethodPost, p.url("/votes"), url.Values{"option": {strconv.FormatInt(p.options[0], 10)}}, map[string]string{"Sec-Fetch-Site": "cross-site"})
		require.Equal(t, http.StatusForbidden, crossSite.status, "behind its cross-origin protection")
		tooLarge := send(t, browser(t, s.client), http.MethodPost, p.url("/votes"), url.Values{"option": {strings.Repeat("1", 16<<10)}}, sameOrigin)
		require.Equal(t, http.StatusRequestEntityTooLarge, tooLarge.status, "behind its body limit")
		require.Equal(t, []int64{0, 0}, counts(t, db.Store, p.id))

		fd := open(t, s.client, p.url("/feed"))
		require.Equal(t, results("Tabs=0", "Spaces=0"), <-fd.events)
		opened := time.Now()
		require.Equal(t, ": keep-alive", <-fd.events)
		require.Equal(t, 15*time.Second, time.Since(opened), "an idle feed outlives the 3s WriteTimeout it was given")

		s.stop(t)
	})
}

func TestInit_serves_a_renewed_certificate_with_no_restart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, newFakeDatabase().init)

		r := s.get(t, "/health/liveness")
		require.Equal(t, http.StatusOK, r.status)
		require.True(t, r.peer.Equal(s.f.cert.TLS().Leaf))

		renewed := s.f.ca.Issue(t, "polls.internal")
		certPEM, keyPEM := pemPair(t, renewed)
		for name, data := range map[string][]byte{"tls/tls.crt": certPEM, "tls/tls.key": keyPEM} {
			file, err := s.f.h.Filesystem().OpenFile(s.f.h.Path(name), os.O_WRONLY|os.O_TRUNC, 0)
			require.NoError(t, err)
			_, err = file.Write(data)
			require.NoError(t, err)
			require.NoError(t, file.Close())
		}
		time.Sleep(30 * time.Second)

		r = send(t, httpservertest.NewClient(s.f.h, s.f.ca.Pool()), http.MethodGet, base+"/health/liveness", nil, nil)
		require.Equal(t, http.StatusOK, r.status)
		require.True(t, r.peer.Equal(renewed.TLS().Leaf), "a new connection gets the renewed certificate")
		select {
		case st := <-s.status:
			t.Fatalf("the polls service exited %d: %v", st.Code(), st.Cause())
		default:
		}

		s.stop(t)
	})
}

func TestInit_is_ready_once_it_reaches_the_store_and_its_listener_listens_and_stays_ready(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase()
		db.setDown(true)
		db.setListenerUp(false)
		s := start(t, db.init)

		for range 2 {
			r := s.get(t, "/health/readiness")
			require.Equal(t, http.StatusServiceUnavailable, r.status, "%s", r.body)
			r = s.get(t, "/health/liveness")
			require.Equal(t, http.StatusOK, r.status, "%s", r.body)
		}
		require.Equal(t, 2, db.pinged())

		db.setDown(false)
		r := s.get(t, "/health/readiness")
		require.Equal(t, http.StatusServiceUnavailable, r.status, "the store is reached, but the listener doesn't listen: %s", r.body)
		require.Equal(t, 3, db.pinged())

		db.setListenerUp(true)
		r = s.get(t, "/health/readiness")
		require.Equal(t, http.StatusOK, r.status, "%s", r.body)
		require.Equal(t, "OK", strings.TrimSpace(r.body))
		require.Equal(t, 3, db.pinged())

		db.setDown(true)
		db.setListenerUp(false)
		time.Sleep(time.Minute)
		for range 2 {
			r = s.get(t, "/health/readiness")
			require.Equal(t, http.StatusOK, r.status, "an outage of the store doesn't take the replica out of its Service: %s", r.body)
		}
		require.Equal(t, 3, db.pinged())

		s.stop(t)
	})
}

func TestInit_starts_its_listener_with_the_App_and_stops_it_at_Shutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase()
		s := start(t, db.init)
		synctest.Wait()
		started, stopped := db.listened()
		require.False(t, started.IsZero(), "the listener runs once the App serves")
		require.True(t, stopped.IsZero())

		sigterm := s.sigterm(t)
		s.exited(t)
		_, stopped = db.listened()
		require.Equal(t, 5*time.Second, stopped.Sub(sigterm), "the listener runs through the drain, and stops at Shutdown")
	})
}

func TestInit_ends_an_open_feed_cleanly_after_the_drain_and_counts_a_vote_in_flight_at_SIGTERM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase()
		p := newPoll(t, db.Store, "Tabs or spaces?", "Tabs", "Spaces")
		s := start(t, db.init)

		fd := open(t, s.client, p.url("/feed"))
		require.Equal(t, results("Tabs=0", "Spaces=0"), <-fd.events)

		held, release := db.hold()
		defer release()
		voted := make(chan response, 1)
		voter := browser(t, s.client)
		go func() {
			voted <- send(t, voter, http.MethodPost, p.url("/votes"), url.Values{"option": {strconv.FormatInt(p.options[1], 10)}}, sameOrigin)
		}()
		<-held

		sigterm := s.sigterm(t)
		time.Sleep(time.Second)
		release()
		r := <-voted
		require.Equal(t, http.StatusOK, r.status, "%s", r.body)
		require.Contains(t, r.body, "Your vote is counted.")
		require.Equal(t, results("Tabs=0", "Spaces=1"), <-fd.events, "the open feed gets the vote")

		end := <-fd.ended
		require.NoError(t, end.err, "a clean end, which htmx's SSE extension reconnects after")
		require.Equal(t, 5*time.Second, end.at.Sub(sigterm), "the feed ends as Shutdown begins, after the drain")
		s.exited(t)
		require.Equal(t, []int64{0, 1}, counts(t, db.Store, p.id), "the vote in flight at SIGTERM counts")
	})
}

func TestInit_dials_PostgreSQL_for_its_pool_and_its_listener_through_the_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		db, err := f.h.ListenTCP(t.Context(), "tcp", dbHost)
		require.NoError(t, err)
		var (
			mu       sync.Mutex
			accepted int
			wg       sync.WaitGroup
		)
		wg.Go(func() {
			for {
				conn, err := db.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				accepted++
				mu.Unlock()
				_ = conn.Close()
			}
		})
		dialed := func() int {
			mu.Lock()
			defer mu.Unlock()
			return accepted
		}

		s := f.serve(t, Init)
		synctest.Wait()
		require.Equal(t, 1, dialed(), "the listener dials as the App starts")

		r := s.get(t, "/polls/"+uuid.NewString())
		require.Equal(t, http.StatusServiceUnavailable, r.status)
		require.Contains(t, r.body, "The polls can&#39;t be reached right now.")
		require.NotContains(t, r.body, dbHost)
		require.Greater(t, dialed(), 1, "the pool dials for the page")

		r = s.get(t, "/health/readiness")
		require.Equal(t, http.StatusServiceUnavailable, r.status)

		s.stop(t)
		require.NoError(t, db.Close())
		wg.Wait()
	})
}
