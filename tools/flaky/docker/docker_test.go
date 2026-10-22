// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package docker_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/tools/flaky/docker"
	"github.com/z5labs/humus/tools/flaky/internal/dockertest"
	"github.com/z5labs/humus/tools/flaky/internal/env"
)

func TestEventsArgs_list_each_testcontainers_container_as_it_starts(t *testing.T) {
	require.Equal(t, []string{
		"events", "--since", "1700000000",
		"--filter", "type=container", "--filter", "event=start", "--filter", "label=org.testcontainers",
		"--format", "{{.Actor.ID}}",
	}, docker.EventsArgs(time.Unix(1700000000, 5)))
}

func TestUpdateArgs_pin_a_container_to_its_cores(t *testing.T) {
	require.Equal(t, []string{"update", "--cpuset-cpus", "0,1,2", "abc"}, docker.UpdateArgs("abc", []int{0, 1, 2}))
}

func TestPin_pins_each_container_that_starts_and_warns_of_one_it_cannot(t *testing.T) {
	dir := dockertest.Fake(t)
	dockertest.Touch(t, dir, "ids", "abc\n\nbad1\ndef\n")
	var log dockertest.Buffer
	p, err := docker.Pin(t.Context(), []int{0, 1}, env.Child(bedrock.OSEnvironment{}), &log)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Count(dockertest.Read(t, dir, "updates"), "\n") == 3
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, p.Stop())

	require.Equal(t, "update --cpuset-cpus 0,1 abc\nupdate --cpuset-cpus 0,1 bad1\nupdate --cpuset-cpus 0,1 def\n", dockertest.Read(t, dir, "updates"))
	require.Contains(t, dockertest.Read(t, dir, "events.args"), "--filter label=org.testcontainers")
	require.Contains(t, log.String(), "warning: pinning container bad1 to cores 0,1: exit status 1: Error: No such container: bad1\n")
	require.NotContains(t, log.String(), "abc")
	require.NotContains(t, log.String(), "docker events ended", "Stop ends the watch without a warning")
}

func TestPin_warns_when_the_watch_ends_before_it_is_stopped(t *testing.T) {
	dir := dockertest.Fake(t)
	dockertest.Touch(t, dir, "die", "")
	var log dockertest.Buffer
	p, err := docker.Pin(t.Context(), []int{3}, env.Child(bedrock.OSEnvironment{}), &log)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(log.String(), "aren't pinned")
	}, 10*time.Second, 10*time.Millisecond)
	err = p.Stop()
	require.ErrorContains(t, err, "docker events ended: exit status 1: events died")
	require.Contains(t, log.String(), "warning: docker events ended: exit status 1: events died; the testcontainers containers started from now on aren't pinned\n")
}

func TestPin_warns_when_it_cannot_read_the_watch_and_stops_it(t *testing.T) {
	dir := dockertest.Fake(t)
	dockertest.Touch(t, dir, "ids", strings.Repeat("x", 70_000)+"\n")
	var log dockertest.Buffer
	p, err := docker.Pin(t.Context(), []int{3}, env.Child(bedrock.OSEnvironment{}), &log)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(log.String(), "aren't pinned")
	}, 10*time.Second, 10*time.Millisecond, "the watch ends though docker events is still running")
	err = p.Stop()
	require.ErrorContains(t, err, "reading Docker's events: bufio.Scanner: token too long")
	require.Contains(t, log.String(), "warning: reading Docker's events: bufio.Scanner: token too long; the testcontainers containers started from now on aren't pinned\n")
	require.Empty(t, dockertest.Read(t, dir, "updates"))
}

func TestPin_fails_with_no_cores_or_no_docker_command(t *testing.T) {
	_, err := docker.Pin(t.Context(), nil, nil, &dockertest.Buffer{})
	require.ErrorContains(t, err, "no cores to pin")

	t.Setenv("PATH", t.TempDir())
	_, err = docker.Pin(t.Context(), []int{0}, nil, &dockertest.Buffer{})
	require.ErrorContains(t, err, "watching Docker's events")
}

func TestCheck_fails_when_docker_cannot_reach_a_daemon(t *testing.T) {
	dir := dockertest.Fake(t)
	childEnv := env.Child(bedrock.OSEnvironment{})
	require.NoError(t, docker.Check(t.Context(), "testcontainers", childEnv))

	dockertest.Touch(t, dir, "down", "")
	err := docker.Check(t.Context(), "testcontainers", childEnv)
	require.ErrorContains(t, err, "the testcontainers level needs Docker")
	require.ErrorContains(t, err, "Cannot connect to the Docker daemon")
}

func TestClusterName_and_Context_are_kind_s(t *testing.T) {
	require.Equal(t, "flaky-wallet", docker.ClusterName("wallet"))
	require.Equal(t, "kind-flaky-wallet", docker.Context("flaky-wallet"))
}

func TestBuild_builds_an_image_from_the_repository_s_root_tagged_as_asked(t *testing.T) {
	dir := dockertest.Fake(t)
	root := t.TempDir()
	childEnv := env.Child(bedrock.OSEnvironment{})
	require.NoError(t, docker.Build(t.Context(), root, root+"/example/wallet/Dockerfile", "wallet", childEnv))
	require.Equal(t, "build --file "+root+"/example/wallet/Dockerfile --tag wallet . in "+root+"\n", dockertest.Read(t, dir, "builds"))

	dockertest.Touch(t, dir, "build-fails", "")
	err := docker.Build(t.Context(), root, root+"/example/wallet/Dockerfile", "wallet", childEnv)
	require.ErrorContains(t, err, "docker build --file "+root+"/example/wallet/Dockerfile --tag wallet .: exit status 1\nfailed to solve: no Dockerfile")
}

// upScript writes an up.sh to a new directory that records the
// KIND_CLUSTER_NAME it ran with beside it, and exits with code, and returns
// its path.
func upScript(t *testing.T, code int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "up.sh")
	script := "#!/usr/bin/env bash\necho \"$KIND_CLUSTER_NAME in $PWD\" >>\"$(dirname \"$0\")/up.log\"\necho deploying\nexit " + strconv.Itoa(code) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestUp_creates_the_cluster_pins_its_node_runs_up_sh_on_it_and_waits_for_its_Deployments(t *testing.T) {
	dir := dockertest.Fake(t)
	up := upScript(t, 0)
	work := t.TempDir()
	childEnv := env.With(env.Child(bedrock.OSEnvironment{}), "KIND_CLUSTER_NAME", "kind")
	require.NoError(t, docker.Up(t.Context(), docker.Cluster{Name: "flaky-wallet", Up: up, Dir: work, Cores: []int{4, 5, 6, 7}}, childEnv))

	require.Equal(t, "create flaky-wallet\n", dockertest.Read(t, dir, "kind.log"))
	require.Equal(t, "update --cpuset-cpus 4,5,6,7 flaky-wallet-control-plane\n", dockertest.Read(t, dir, "updates"), "its node lands on its cores")
	require.Equal(t, "flaky-wallet in "+work+"\n", dockertest.Read(t, filepath.Dir(up), "up.log"), "up.sh runs with KIND_CLUSTER_NAME set, in place of the one flaky was given")
	kubectl := strings.Split(strings.TrimSpace(dockertest.Read(t, dir, "kubectl.log")), "\n")
	require.Len(t, kubectl, 3)
	require.Contains(t, kubectl[0], "--context kind-flaky-wallet get deployments --all-namespaces")
	require.Equal(t, []string{
		"--context kind-flaky-wallet --namespace app rollout status deployment/web --timeout 5m",
		"--context kind-flaky-wallet --namespace kube-system rollout status deployment/coredns --timeout 5m",
	}, kubectl[1:], "it waits for every Deployment on the cluster")

	require.NoError(t, docker.Down(t.Context(), "flaky-wallet", childEnv))
	require.Equal(t, "create flaky-wallet\ndelete flaky-wallet\n", dockertest.Read(t, dir, "kind.log"))
}

func TestUp_fails_with_what_the_step_that_failed_printed(t *testing.T) {
	dir := dockertest.Fake(t)
	childEnv := env.Child(bedrock.OSEnvironment{})
	up := upScript(t, 3)
	err := docker.Up(t.Context(), docker.Cluster{Name: "flaky-mailer", Up: up, Dir: t.TempDir(), Cores: []int{0}}, childEnv)
	require.ErrorContains(t, err, up+": exit status 3\ndeploying")
	require.Empty(t, dockertest.Read(t, dir, "kubectl.log"), "it waits for no Deployment once up.sh has failed")

	dockertest.Touch(t, dir, "create-fails-flaky-billing", "")
	err = docker.Up(t.Context(), docker.Cluster{Name: "flaky-billing", Up: up, Dir: t.TempDir(), Cores: []int{0}}, childEnv)
	require.ErrorContains(t, err, "kind create cluster --name flaky-billing --wait 2m: exit status 1\nERROR: failed to create cluster")
	require.NotContains(t, dockertest.Read(t, dir, "updates"), "flaky-billing", "it pins nothing once the cluster can't be created")
}

func TestCheckKind_needs_kind_and_kubectl_on_PATH(t *testing.T) {
	dir := dockertest.Fake(t)
	require.NoError(t, docker.CheckKind())

	require.NoError(t, os.Remove(filepath.Join(dir, "kubectl")))
	t.Setenv("PATH", dir)
	require.ErrorContains(t, docker.CheckKind(), `the functional level needs kind and kubectl: exec: "kubectl": executable file not found in $PATH`)
}
