// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app_test

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/mailer/app"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	probesAddr = "localhost:8443"
	brokerAddr = "rabbitmq.internal:5672"
)

type mailer struct {
	h      *humustest.Host
	ca     *humustest.CA
	env    map[string]string
	client *http.Client
}

func newMailer(t *testing.T, files map[string][]byte) *mailer {
	t.Helper()
	ca := humustest.NewCA(t)
	all := map[string][]byte{
		"secrets/rabbitmq-password": []byte(rand.Text() + "\n"),
		"secrets/provider-api-key":  []byte(rand.Text()),
		"secrets/probes.pem":        pemOf(t, ca.Issue(t, "localhost")),
		"certs/rabbitmq-ca.pem":     caPEM(t, ca),
		"certs/provider-ca.pem":     caPEM(t, ca),
	}
	maps.Copy(all, files)
	h := humustest.NewHost(t, humustest.WithFiles(all))

	client := h.HTTPClient()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	return &mailer{
		h:  h,
		ca: ca,
		env: map[string]string{
			"RABBITMQ_ADDR":          brokerAddr,
			"RABBITMQ_USERNAME":      "mailer",
			"RABBITMQ_PASSWORD_FILE": h.Path("secrets/rabbitmq-password"),
			"RABBITMQ_CA_FILE":       h.Path("certs/rabbitmq-ca.pem"),
			"MAIL_FROM":              "mailer@example.com",
			"PROVIDER_URL":           "https://provider.internal:8443",
			"PROVIDER_API_KEY_FILE":  h.Path("secrets/provider-api-key"),
			"PROVIDER_CA_FILE":       h.Path("certs/provider-ca.pem"),
			"PROBES_ADDR":            probesAddr,
			"PROBES_TLS_PEM_FILE":    h.Path("secrets/probes.pem"),
		},
		client: client,
	}
}

func (m *mailer) start(t *testing.T, env map[string]string, opts ...humus.Option) (signal func(os.Signal), status <-chan bedrock.ExitStatus) {
	t.Helper()
	merged := maps.Clone(m.env)
	for name, value := range env {
		if value == unset {
			delete(merged, name)
			continue
		}
		merged[name] = value
	}
	p, st := humustest.Start(t, app.Init,
		humustest.WithApp(append([]humus.Option{humus.WithHost(m.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)}, opts...)...),
		humustest.WithEnv(merged),
	)
	return p.Signal, st
}

const unset = "\x00unset"

func (m *mailer) probe(t *testing.T, path string) int {
	t.Helper()
	resp, err := m.client.Get("https://" + probesAddr + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode
}

func pemOf(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var out []byte
	for _, der := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}

func caPEM(t *testing.T, ca *humustest.CA) []byte {
	t.Helper()
	c := ca.Issue(t, "unused").TLS()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[len(c.Certificate)-1]})
}

func exitsConfig(t *testing.T, env map[string]string, files map[string][]byte) error {
	t.Helper()
	var cause error
	synctest.Test(t, func(t *testing.T) {
		m := newMailer(t, files)
		_, status := m.start(t, env, humus.WithFaults(app.Faults))
		st := <-status
		cause = st.Cause()
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", cause)
	})
	require.Error(t, cause)
	return cause
}

func TestInit_without_its_settings_exits_78(t *testing.T) {
	required := []string{
		"RABBITMQ_ADDR",
		"RABBITMQ_USERNAME",
		"RABBITMQ_PASSWORD_FILE",
		"MAIL_FROM",
		"PROVIDER_URL",
		"PROVIDER_API_KEY_FILE",
		"PROBES_ADDR",
		"PROBES_TLS_PEM_FILE",
	}
	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			cause := exitsConfig(t, map[string]string{name: unset}, nil)
			require.ErrorContains(t, cause, "config "+name+": not found")
		})
	}

	t.Run("all of them", func(t *testing.T) {
		env := map[string]string{}
		for _, name := range required {
			env[name] = unset
		}
		cause := exitsConfig(t, env, nil)
		for _, name := range required {
			require.ErrorContains(t, cause, "config "+name+": not found")
		}
	})
}

func TestInit_with_a_bad_setting_exits_78(t *testing.T) {
	testCases := map[string]struct {
		name   string
		value  string
		files  map[string][]byte
		secret bool
	}{
		"an AMQP URI as the address":             {name: "RABBITMQ_ADDR", value: "amqp://mailer:" + rand.Text() + "@" + brokerAddr, secret: true},
		"an address with no port":                {name: "RABBITMQ_ADDR", value: "rabbitmq.internal"},
		"an empty username":                      {name: "RABBITMQ_USERNAME", value: ""},
		"an empty vhost":                         {name: "RABBITMQ_VHOST", value: ""},
		"a password file that doesn't exist":     {name: "RABBITMQ_PASSWORD_FILE", value: "secrets/missing"},
		"an empty password file":                 {name: "RABBITMQ_PASSWORD_FILE", value: "secrets/empty", files: map[string][]byte{"secrets/empty": []byte("\n")}},
		"a broker CA file that doesn't exist":    {name: "RABBITMQ_CA_FILE", value: "certs/missing.pem"},
		"a broker CA file with no certificate":   {name: "RABBITMQ_CA_FILE", value: "certs/junk.pem", files: map[string][]byte{"certs/junk.pem": []byte("junk")}},
		"a prefetch that isn't a number":         {name: "RABBITMQ_PREFETCH", value: "many"},
		"a prefetch of 0":                        {name: "RABBITMQ_PREFETCH", value: "0"},
		"a concurrency of 0":                     {name: "RABBITMQ_CONCURRENCY", value: "0"},
		"a concurrency above the prefetch":       {name: "RABBITMQ_CONCURRENCY", value: "17"},
		"an empty sender":                        {name: "MAIL_FROM", value: ""},
		"max attempts of 0":                      {name: "MAIL_MAX_ATTEMPTS", value: "0"},
		"negative max attempts":                  {name: "MAIL_MAX_ATTEMPTS", value: "-1"},
		"max attempts that aren't a number":      {name: "MAIL_MAX_ATTEMPTS", value: "five"},
		"a provider URL that isn't http":         {name: "PROVIDER_URL", value: "ftp://provider.internal"},
		"a relative provider URL":                {name: "PROVIDER_URL", value: "/v1"},
		"an API key file that doesn't exist":     {name: "PROVIDER_API_KEY_FILE", value: "secrets/missing"},
		"an empty API key file":                  {name: "PROVIDER_API_KEY_FILE", value: "secrets/empty", files: map[string][]byte{"secrets/empty": nil}},
		"a provider CA file that doesn't exist":  {name: "PROVIDER_CA_FILE", value: "certs/missing.pem"},
		"a provider timeout of 0":                {name: "PROVIDER_TIMEOUT", value: "0s"},
		"a provider timeout with no unit":        {name: "PROVIDER_TIMEOUT", value: "10"},
		"a negative attempt timeout":             {name: "PROVIDER_ATTEMPT_TIMEOUT", value: "-1s"},
		"provider max attempts of 0":             {name: "PROVIDER_MAX_ATTEMPTS", value: "0"},
		"a probes address with no port":          {name: "PROBES_ADDR", value: "localhost"},
		"a probes PEM file that doesn't exist":   {name: "PROBES_TLS_PEM_FILE", value: "secrets/missing.pem"},
		"a probes PEM file with no key":          {name: "PROBES_TLS_PEM_FILE", value: "secrets/ca.pem", files: map[string][]byte{"secrets/ca.pem": []byte("not a certificate")}},
		"an empty trusted publisher":             {name: "MAIL_TRUSTED_PUBLISHERS", value: "orders,,billing"},
		"a trailing comma in trusted publishers": {name: "MAIL_TRUSTED_PUBLISHERS", value: "orders,"},
		"a fault max delay of 0":                 {name: "FAULT_MAX_DELAY", value: "0s"},
		"a negative fault max delay":             {name: "FAULT_MAX_DELAY", value: "-1s"},
		"a fault max delay with no unit":         {name: "FAULT_MAX_DELAY", value: "10"},
	}
	for desc, tc := range testCases {
		t.Run(desc, func(t *testing.T) {
			cause := exitsConfig(t, map[string]string{tc.name: tc.value}, tc.files)
			require.ErrorContains(t, cause, "config "+tc.name+":")
			if tc.secret {
				require.NotContains(t, cause.Error(), tc.value)
			}
		})
	}
}

func TestInit_reads_its_secrets_through_the_Host(t *testing.T) {
	ca := humustest.NewCA(t)
	for name, contents := range map[string][]byte{
		"RABBITMQ_PASSWORD_FILE": []byte(rand.Text()),
		"RABBITMQ_CA_FILE":       caPEM(t, ca),
		"PROVIDER_API_KEY_FILE":  []byte(rand.Text()),
		"PROVIDER_CA_FILE":       caPEM(t, ca),
		"PROBES_TLS_PEM_FILE":    pemOf(t, ca.Issue(t, "localhost")),
	} {
		t.Run(name, func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(outside, contents, 0o600))

			cause := exitsConfig(t, map[string]string{name: outside}, nil)
			require.ErrorContains(t, cause, "config "+name+": ")
			require.ErrorContains(t, cause, "path escapes from parent")
		})
	}
}

func TestInit_waits_for_the_broker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMailer(t, nil)
		tel := humustest.NewTelemetry()
		signal, status := m.start(t, nil, humus.WithTelemetry(tel.Builder()))
		<-m.h.Listening(probesAddr)
		synctest.Wait()

		require.Equal(t, http.StatusServiceUnavailable, m.probe(t, "/health/readiness"))
		require.Equal(t, http.StatusOK, m.probe(t, "/health/liveness"))

		time.Sleep(time.Minute)
		synctest.Wait()

		require.Equal(t, http.StatusServiceUnavailable, m.probe(t, "/health/readiness"))
		require.Equal(t, http.StatusOK, m.probe(t, "/health/liveness"))
		require.Greater(t, failedConnects(tel), 1)

		signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func failedConnects(tel *humustest.Telemetry) int {
	n := 0
	for _, r := range tel.Logs() {
		if r.Body().AsString() == "connecting failed" {
			n++
		}
	}
	return n
}

func TestBuildTelemetry_with_a_bad_variable_exits_78(t *testing.T) {
	for name, value := range map[string]string{
		"OTEL_SDK_DISABLED":           "maybe",
		"OTEL_SERVICE_NAME":           "",
		"OTEL_TRACES_EXPORTER":        "zipkin",
		"OTEL_METRICS_EXPORTER":       "prometheus",
		"OTEL_LOGS_EXPORTER":          "syslog",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "collector.internal",
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := newMailer(t, nil)
				_, status := m.start(t, map[string]string{name: value}, humus.WithTelemetry(app.BuildTelemetry))

				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
				require.ErrorContains(t, st.Cause(), "config "+name+":")
			})
		})
	}
}

func TestBuildTelemetry_writes_the_logs_to_stdout_by_default(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMailer(t, nil)
		p, status := humustest.Start(t, app.Init,
			humustest.WithApp(humus.WithHost(m.h.Host()), humus.WithTelemetry(app.BuildTelemetry)),
			humustest.WithEnv(m.env),
		)
		<-m.h.Listening(probesAddr)
		time.Sleep(time.Minute)
		synctest.Wait()

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Contains(t, string(p.Stdout()), `"body":"connecting failed"`)
	})
}
