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

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus/example/kube/databases/cases"
)

const (
	controller = `job="databases", humus_kube_controller_name="databases-controller"`
	reconciles = `humus_kube_controller_reconcile_duration_count{` + controller
)

var alertRules = []struct {
	title  string
	series []string
}{
	{title: "DatabasesReconcileFailing", series: []string{reconciles + `, humus_kube_controller_reconcile_outcome="error"}`}},
	{title: "DatabasesReconcilePanicked", series: []string{reconciles + `, humus_kube_controller_reconcile_outcome=~".+"}`}},
	{title: "DatabasesReconcileStuck", series: []string{`humus_kube_controller_reconcile_longest_unfinished_duration{` + controller + `}`}},
	{title: "DatabasesBacklogged", series: []string{
		`humus_kube_controller_queue_depth{` + controller + `, k8s_pod_name!=""}`,
		reconciles + `, k8s_pod_name!=""}`,
	}},
	{title: "DatabasesRestarting", series: []string{`k8s_container_restarts{k8s_namespace_name="databases", k8s_container_name="databases"}`}},
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

func TestGrafana_alerts_on_the_controller_and_has_its_dashboard(t *testing.T) {
	for _, d := range []string{"postgres", "databases", "otel-collector", "mimir", "grafana"} {
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
		eventually(loaded, t, "the dashboard databases in Grafana", func(ctx context.Context) (bool, error) {
			var body struct {
				Dashboard struct {
					Title  string            `json:"title"`
					Panels []json.RawMessage `json:"panels"`
				} `json:"dashboard"`
				Meta struct {
					Provisioned bool `json:"provisioned"`
				} `json:"meta"`
			}
			ok, err := getJSON(ctx, admin, grafana+"/api/dashboards/uid/databases", &body)
			if !ok || err != nil {
				return false, err
			}
			if body.Dashboard.Title != "Databases" || !body.Meta.Provisioned || len(body.Dashboard.Panels) == 0 {
				return false, fmt.Errorf("the dashboard databases is %q, provisioned %t, with %d panels", body.Dashboard.Title, body.Meta.Provisioned, len(body.Dashboard.Panels))
			}
			return true, nil
		})
	})

	started := time.Now()
	t.Run("PostgreSQLDown", func(t *testing.T) {
		cases.PostgreSQLDown(t, target)
	})

	ctx, cancel := context.WithTimeout(t.Context(), telemetryTimeout)
	defer cancel()

	t.Run("Mimir holds every series the rules read", func(t *testing.T) {
		for _, r := range alertRules {
			for _, series := range r.series {
				q := url.Values{
					"match[]": {series},
					"start":   {strconv.FormatInt(started.Unix(), 10)},
				}
				eventually(ctx, t, r.title+"'s "+series+" in Mimir", func(ctx context.Context) (bool, error) {
					var body struct {
						Data []map[string]string `json:"data"`
					}
					ok, err := getJSON(ctx, client, mimir+"/prometheus/api/v1/series?"+q.Encode(), &body)
					return ok && len(body.Data) > 0, err
				})
			}
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

func secret(t *testing.T, name, key string) string {
	t.Helper()
	kc, err := kubernetes.NewForConfig(kubeconfig(t))
	require.NoError(t, err)
	s, err := kc.CoreV1().Secrets(namespace).Get(t.Context(), name, metav1.GetOptions{})
	require.NoError(t, err, "the secret %s/%s", namespace, name)
	value := s.Data[key]
	require.NotEmpty(t, value, "the secret %s/%s has no %s", namespace, name, key)
	return string(value)
}
