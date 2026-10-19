//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package functional runs the billing job's shared cases, and the checks only a cluster can make, against its deployment.
package functional

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/z5labs/humus/example/billing/cases"
)

var (
	kubeContext     = flag.String("kube-context", "", "the kubeconfig context of the cluster the billing job is deployed to, the current one if empty")
	namespace       = flag.String("namespace", "billing", "the namespace of the billing CronJob")
	cronJobName     = flag.String("cronjob", "billing", "the name of the billing CronJob")
	postgresURL     = flag.String("postgres", "", "a URL of the PostgreSQL server as the tests reach it, as a role that may create databases")
	postgresAddress = flag.String("postgres-address", "", "the PostgreSQL server's host:port as the billing job reaches it, the -postgres URL's if empty")
	wiremockURL     = flag.String("wiremock", "", "the payments API stand-in's base URL as the tests reach it")
	caseTimeout     = flag.Duration("timeout", 5*time.Minute, "how long each case may take")
)

const (
	containerName = "billing"
	exitConfig    = 78
)

type deployment struct {
	t       *testing.T
	client  kubernetes.Interface
	cronJob *batchv1.CronJob
}

func deployed(t *testing.T) deployment {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: *kubeContext},
	).ClientConfig()
	require.NoError(t, err, "loading the kubeconfig")
	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	cronJob, err := client.BatchV1().CronJobs(*namespace).Get(t.Context(), *cronJobName, metav1.GetOptions{})
	require.NoError(t, err, "getting the CronJob %s/%s", *namespace, *cronJobName)
	return deployment{t: t, client: client, cronJob: cronJob}
}

func billingContainer(spec *corev1.PodSpec) (*corev1.Container, error) {
	for i := range spec.Containers {
		if spec.Containers[i].Name == containerName {
			return &spec.Containers[i], nil
		}
	}
	return nil, fmt.Errorf("the pod template has no container %s", containerName)
}

func envValue(c *corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func setEnv(c *corev1.Container, name, value string) {
	for i := range c.Env {
		if c.Env[i].Name == name {
			c.Env[i] = corev1.EnvVar{Name: name, Value: value}
			return
		}
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: name, Value: value})
}

func mounted(spec *corev1.PodSpec, c *corev1.Container, env string) (*corev1.Volume, string, error) {
	file := envValue(c, env)
	if file == "" {
		return nil, "", fmt.Errorf("%s isn't set on the container %s", env, c.Name)
	}
	for _, m := range c.VolumeMounts {
		rel, ok := strings.CutPrefix(file, strings.TrimSuffix(m.MountPath, "/")+"/")
		if !ok {
			continue
		}
		for i := range spec.Volumes {
			if spec.Volumes[i].Name == m.Name {
				return &spec.Volumes[i], rel, nil
			}
		}
	}
	return nil, "", fmt.Errorf("no volume holds %s, the file %s names", file, env)
}

func itemKey(items []corev1.KeyToPath, rel string) string {
	for _, item := range items {
		if item.Path == rel {
			return item.Key
		}
	}
	return rel
}

func (d deployment) template() *corev1.PodSpec {
	return &d.cronJob.Spec.JobTemplate.Spec.Template.Spec
}

func (d deployment) env(name string) (string, error) {
	c, err := billingContainer(d.template())
	if err != nil {
		return "", err
	}
	return envValue(c, name), nil
}

func (d deployment) read(ctx context.Context, env string) ([]byte, error) {
	spec := d.template()
	c, err := billingContainer(spec)
	if err != nil {
		return nil, err
	}
	v, rel, err := mounted(spec, c, env)
	if err != nil {
		return nil, err
	}
	switch {
	case v.Secret != nil:
		s, err := d.client.CoreV1().Secrets(d.cronJob.Namespace).Get(ctx, v.Secret.SecretName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		key := itemKey(v.Secret.Items, rel)
		b, ok := s.Data[key]
		if !ok {
			return nil, fmt.Errorf("the Secret %s has no key %s", s.Name, key)
		}
		return b, nil
	case v.ConfigMap != nil:
		cm, err := d.client.CoreV1().ConfigMaps(d.cronJob.Namespace).Get(ctx, v.ConfigMap.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		key := itemKey(v.ConfigMap.Items, rel)
		if s, ok := cm.Data[key]; ok {
			return []byte(s), nil
		}
		if b, ok := cm.BinaryData[key]; ok {
			return b, nil
		}
		return nil, fmt.Errorf("the ConfigMap %s has no key %s", cm.Name, key)
	default:
		return nil, fmt.Errorf("the volume %s, which holds %s, is neither a Secret nor a ConfigMap", v.Name, env)
	}
}

func (d deployment) newJob(env map[string]string) (*batchv1.Job, error) {
	tmpl := d.cronJob.Spec.JobTemplate
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        d.cronJob.Name + "-" + strings.ToLower(rand.Text()),
			Namespace:   d.cronJob.Namespace,
			Labels:      maps.Clone(tmpl.Labels),
			Annotations: maps.Clone(tmpl.Annotations),
		},
		Spec: *tmpl.Spec.DeepCopy(),
	}
	c, err := billingContainer(&job.Spec.Template.Spec)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(env)) {
		setEnv(c, name, env[name])
	}
	return job, nil
}

func (d deployment) start(ctx context.Context, job *batchv1.Job) (*batchv1.Job, func(), error) {
	jobs := d.client.BatchV1().Jobs(job.Namespace)
	created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, func() {}, err
	}
	return created, func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		background := metav1.DeletePropagationBackground
		err := jobs.Delete(ctx, created.Name, metav1.DeleteOptions{PropagationPolicy: &background})
		if err != nil {
			d.t.Errorf("deleting the Job %s: %v", created.Name, err)
		}
	}, nil
}

func finished(job *batchv1.Job) (batchv1.JobCondition, bool) {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return c, true
		}
	}
	return batchv1.JobCondition{}, false
}

func (d deployment) wait(ctx context.Context, job *batchv1.Job) (*batchv1.Job, batchv1.JobCondition, error) {
	var (
		done      *batchv1.Job
		condition batchv1.JobCondition
	)
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		j, err := d.client.BatchV1().Jobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		c, ok := finished(j)
		if ok {
			done, condition = j, c
		}
		return ok, nil
	})
	if err != nil {
		return nil, condition, fmt.Errorf("waiting for the Job %s: %w", job.Name, err)
	}
	return done, condition, nil
}

func (d deployment) pods(ctx context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	pods, err := d.client.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: batchv1.ControllerUidLabel + "=" + string(job.UID),
	})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func exitCode(pod corev1.Pod) (int, error) {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name != containerName {
			continue
		}
		if s.State.Terminated == nil {
			return 0, fmt.Errorf("the container %s of the pod %s hasn't terminated", containerName, pod.Name)
		}
		return int(s.State.Terminated.ExitCode), nil
	}
	return 0, fmt.Errorf("the pod %s has no status for the container %s", pod.Name, containerName)
}

func (d deployment) logs(ctx context.Context, pod corev1.Pod) string {
	b, err := d.client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: containerName}).DoRaw(ctx)
	if err != nil {
		return fmt.Sprintf("reading the logs: %v", err)
	}
	return string(b)
}

func (d deployment) run(ctx context.Context, databaseURL string, env map[string]string) (int, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return 0, errors.New("the database URL doesn't parse")
	}
	if *postgresAddress != "" {
		u.Host = *postgresAddress
	}

	job, err := d.newJob(env)
	if err != nil {
		return 0, err
	}
	var noRetries int32
	job.Spec.BackoffLimit = &noRetries
	spec := &job.Spec.Template.Spec
	c, err := billingContainer(spec)
	if err != nil {
		return 0, err
	}
	v, rel, err := mounted(spec, c, "DATABASE_URL_FILE")
	if err != nil {
		return 0, err
	}

	secrets := d.client.CoreV1().Secrets(job.Namespace)
	secret, err := secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-database", Namespace: job.Namespace},
		Data:       map[string][]byte{"url": []byte(u.String())},
	}, metav1.CreateOptions{})
	if err != nil {
		return 0, err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := secrets.Delete(ctx, secret.Name, metav1.DeleteOptions{}); err != nil {
			d.t.Errorf("deleting the Secret %s: %v", secret.Name, err)
		}
	}()

	source := &corev1.SecretVolumeSource{
		SecretName: secret.Name,
		Items:      []corev1.KeyToPath{{Key: "url", Path: rel}},
	}
	if v.Secret != nil {
		source.DefaultMode = v.Secret.DefaultMode
	}
	v.VolumeSource = corev1.VolumeSource{Secret: source}

	created, cleanup, err := d.start(ctx, job)
	defer cleanup()
	if err != nil {
		return 0, err
	}
	done, _, err := d.wait(ctx, created)
	if err != nil {
		return 0, err
	}
	pods, err := d.pods(ctx, done)
	if err != nil {
		return 0, err
	}
	if len(pods) != 1 {
		return 0, fmt.Errorf("the Job %s ran %d pods, not one", done.Name, len(pods))
	}
	code, err := exitCode(pods[0])
	if err != nil {
		return 0, err
	}
	d.t.Logf("billing exited %d in the Job %s:\n%s", code, done.Name, d.logs(ctx, pods[0]))
	return code, nil
}

func TestBilling_passes_the_shared_cases_as_deployed(t *testing.T) {
	require.NotEmpty(t, *postgresURL, "-postgres")
	require.NotEmpty(t, *wiremockURL, "-wiremock")
	d := deployed(t)

	key, err := d.read(t.Context(), "PAYMENTS_API_KEY_FILE")
	require.NoError(t, err)
	ca, err := d.read(t.Context(), "PAYMENTS_CA_FILE")
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(ca), "PAYMENTS_CA_FILE holds no PEM certificate")
	paymentsURL, err := d.env("PAYMENTS_URL")
	require.NoError(t, err)
	payments, err := url.Parse(paymentsURL)
	require.NoError(t, err)
	wiremock, err := url.Parse(*wiremockURL)
	require.NoError(t, err)
	schema, err := os.ReadFile(filepath.Join("..", "postgres", "schema.sql"))
	require.NoError(t, err)

	target := cases.Target{
		Postgres: *postgresURL,
		Schema:   string(schema),
		WireMock: wiremock,
		Client: &http.Client{
			Timeout: *caseTimeout,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				RootCAs:    roots,
				ServerName: payments.Hostname(),
			}},
		},
		Key:     strings.TrimSpace(string(key)),
		Run:     d.run,
		Timeout: *caseTimeout,
	}

	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.All {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})
}

func TestCronJob_fails_a_bad_setting_after_one_pod_by_its_pod_failure_policy(t *testing.T) {
	d := deployed(t)
	ctx, cancel := context.WithTimeout(t.Context(), *caseTimeout)
	defer cancel()

	job, err := d.newJob(map[string]string{"BILLING_PERIOD": "last-month"})
	require.NoError(t, err)
	created, cleanup, err := d.start(ctx, job)
	defer cleanup()
	require.NoError(t, err)

	done, condition, err := d.wait(ctx, created)
	require.NoError(t, err)
	require.Equal(t, batchv1.JobFailed, condition.Type)
	require.Equal(t, batchv1.JobReasonPodFailurePolicy, condition.Reason)
	require.EqualValues(t, 1, done.Status.Failed)
	pods, err := d.pods(ctx, done)
	require.NoError(t, err)
	require.Len(t, pods, 1)
	code, err := exitCode(pods[0])
	require.NoError(t, err)
	require.Equal(t, exitConfig, code)
}
