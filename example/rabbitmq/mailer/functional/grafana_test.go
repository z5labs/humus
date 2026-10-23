//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package functional

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/z5labs/humus/example/rabbitmq/mailer/cases"
)

const providerScope = `otel_scope_name="github.com/z5labs/humus/httpclient"`

var alertRules = []struct {
	title  string
	series string
}{
	{title: "MailBacklog", series: `rabbitmq_message_current{rabbitmq_queue_name="mail.send", state="ready"}`},
	{title: "MailNotConsuming", series: `rabbitmq_consumer_count{rabbitmq_queue_name="mail.send"}`},
	{title: "MailParked", series: `rabbitmq_message_current{rabbitmq_queue_name="mail.send.dead"}`},
	{title: "MailGivingUp", series: `mail_requests{job="mailer", mail_outcome="gave_up"}`},
	{title: "MailRetrying", series: `mail_requests{job="mailer", mail_outcome="retried"}`},
	{title: "MailProviderBreakerOpen", series: `humus_circuit_state{job="mailer", ` + providerScope + `, humus_circuit_state="open"}`},
	{title: "MailRetriesThrottled", series: `humus_retry_decisions{job="mailer", humus_retry_decision="throttled"}`},
	{title: "MailCrashLooping", series: `k8s_container_restarts{k8s_container_name="mailer"}`},
}

type basicAuth struct {
	username, password string
	next               http.RoundTripper
}

func (b basicAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.SetBasicAuth(b.username, b.password)
	return b.next.RoundTrip(req)
}

func TestGrafana_alerts_on_the_mailer_and_has_its_dashboard(t *testing.T) {
	for _, d := range []string{"mailer", "otel-collector", "mimir", "grafana"} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

	target := sharedTarget(t)
	mimir := "http://" + forward(t, "mimir", 9009)
	grafana := "http://" + forward(t, "grafana", 3000)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	admin := &http.Client{Timeout: 10 * time.Second, Transport: basicAuth{
		username: secret(t, "grafana-admin", "username"),
		password: secret(t, "grafana-admin", "password"),
		next:     &http.Transport{},
	}}

	loaded, cancel := context.WithTimeout(t.Context(), telemetryTimeout)
	defer cancel()

	t.Run("Grafana loaded every rule", func(t *testing.T) {
		want := make([]string, 0, len(alertRules))
		for _, r := range alertRules {
			want = append(want, r.title)
		}
		slices.Sort(want)
		var got []string
		eventually(loaded, t, "the rules "+fmt.Sprint(want)+" in Grafana", func(ctx context.Context) (bool, error) {
			var rules []struct {
				Title string `json:"title"`
			}
			ok, err := getJSON(ctx, admin, grafana+"/api/v1/provisioning/alert-rules", &rules)
			if !ok || err != nil {
				return false, err
			}
			got = got[:0]
			for _, r := range rules {
				got = append(got, r.Title)
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				return false, fmt.Errorf("Grafana has the rules %v", got)
			}
			return true, nil
		})
	})

	t.Run("Grafana loaded the dashboard", func(t *testing.T) {
		eventually(loaded, t, "the dashboard mailer in Grafana", func(ctx context.Context) (bool, error) {
			var body struct {
				Dashboard struct {
					Title  string            `json:"title"`
					Panels []json.RawMessage `json:"panels"`
				} `json:"dashboard"`
				Meta struct {
					Provisioned bool `json:"provisioned"`
				} `json:"meta"`
			}
			ok, err := getJSON(ctx, admin, grafana+"/api/dashboards/uid/mailer", &body)
			if !ok || err != nil {
				return false, err
			}
			if body.Dashboard.Title != "Mailer" || !body.Meta.Provisioned || len(body.Dashboard.Panels) == 0 {
				return false, fmt.Errorf("the dashboard mailer is %q, provisioned %t, with %d panels", body.Dashboard.Title, body.Meta.Provisioned, len(body.Dashboard.Panels))
			}
			return true, nil
		})
	})

	started := time.Now()
	t.Run("ProviderDown", func(t *testing.T) {
		cases.ProviderDown(t, target)
	})

	ctx, cancel := context.WithTimeout(t.Context(), telemetryTimeout)
	defer cancel()

	t.Run("Mimir holds every series the rules read", func(t *testing.T) {
		for _, r := range alertRules {
			q := url.Values{
				"match[]": {r.series},
				"start":   {strconv.FormatInt(started.Unix(), 10)},
			}
			eventually(ctx, t, r.title+"'s "+r.series+" in Mimir", func(ctx context.Context) (bool, error) {
				var body struct {
					Data []map[string]string `json:"data"`
				}
				ok, err := getJSON(ctx, client, mimir+"/prometheus/api/v1/series?"+q.Encode(), &body)
				return ok && len(body.Data) > 0, err
			})
		}
	})

	held := time.Now()
	t.Run("each rule evaluates without error", func(t *testing.T) {
		for _, r := range alertRules {
			eventually(ctx, t, r.title+" evaluated without error since "+held.Format(time.RFC3339), func(ctx context.Context) (bool, error) {
				var body struct {
					Data struct {
						Groups []struct {
							Rules []struct {
								Name           string    `json:"name"`
								Health         string    `json:"health"`
								LastError      string    `json:"lastError"`
								LastEvaluation time.Time `json:"lastEvaluation"`
							} `json:"rules"`
						} `json:"groups"`
					} `json:"data"`
				}
				ok, err := getJSON(ctx, admin, grafana+"/api/prometheus/grafana/api/v1/rules", &body)
				if !ok || err != nil {
					return false, err
				}
				for _, g := range body.Data.Groups {
					for _, rule := range g.Rules {
						if rule.Name != r.title {
							continue
						}
						if rule.LastEvaluation.Before(held) {
							return false, nil
						}
						if rule.Health != "ok" || rule.LastError != "" {
							return false, fmt.Errorf("%s is %s: %s", r.title, rule.Health, rule.LastError)
						}
						return true, nil
					}
				}
				return false, fmt.Errorf("Grafana has no rule %s", r.title)
			})
		}
	})
}
