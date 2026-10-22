// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build testcontainers

package docker_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
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
	"github.com/z5labs/humus/tools/flaky/lanes"
	"github.com/z5labs/humus/tools/flaky/run"
)

// image is a small image whose sleep keeps a container running.
const image = "busybox:1.37"

// dockerRun runs docker with args, and returns what it printed.
func dockerRun(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "docker", args...)
	cmd.Env = env.Child(bedrock.OSEnvironment{})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "docker %v: %s", args, stderr.String())
	return strings.TrimSpace(string(out))
}

// start starts a container that sleeps, with args before the image, and
// removes it at the end of the test.
func start(t *testing.T, args ...string) string {
	t.Helper()
	id := dockerRun(t, append(append([]string{"run", "-d"}, args...), image, "sleep", "300")...)
	t.Cleanup(func() {
		cmd := exec.Command("docker", "rm", "-f", id)
		cmd.Env = env.Child(bedrock.OSEnvironment{})
		_ = cmd.Run()
	})
	return id
}

func cpuset(t *testing.T, id string) string {
	return dockerRun(t, "inspect", "--format", "{{.HostConfig.CpusetCpus}}", id)
}

func TestPin_puts_a_testcontainers_container_on_the_kept_back_cores(t *testing.T) {
	childEnv := env.Child(bedrock.OSEnvironment{})
	require.NoError(t, docker.Check(t.Context(), "testcontainers", childEnv))
	cores, err := run.Affinity()
	require.NoError(t, err)
	if len(cores) < 2 {
		t.Skipf("this process may run on %d cores, and the test needs 2", len(cores))
	}
	kept := lanes.Kept(cores, 1)
	dockerRun(t, "pull", "-q", image)

	var log dockertest.Buffer
	p, err := docker.Pin(t.Context(), kept, childEnv, &log)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, p.Stop()) })

	labelled := start(t, "--label", docker.Label+"=true")
	unlabelled := start(t)
	require.Eventually(t, func() bool {
		return cpuset(t, labelled) == run.CoreList(kept)
	}, 30*time.Second, 100*time.Millisecond, "the container labelled %s lands on the kept-back cores %v; log:\n%s", docker.Label, kept, log.String())
	require.Empty(t, cpuset(t, unlabelled), "a container without the label isn't pinned")
	require.Empty(t, log.String())
}

func TestUp_puts_a_cluster_s_node_container_on_its_lane(t *testing.T) {
	childEnv := env.Child(bedrock.OSEnvironment{})
	require.NoError(t, docker.Check(t.Context(), "functional", childEnv))
	require.NoError(t, docker.CheckKind())
	cores, err := run.Affinity()
	require.NoError(t, err)
	if len(cores) < 2 {
		t.Skipf("this process may run on %d cores, and the test needs 2", len(cores))
	}
	laneOf, err := lanes.Split(cores, 1, min(len(cores)-1, 4))
	require.NoError(t, err)
	lane := laneOf[0]

	dir := t.TempDir()
	up := filepath.Join(dir, "up.sh")
	require.NoError(t, os.WriteFile(up, []byte("#!/usr/bin/env bash\necho \"$KIND_CLUSTER_NAME\" >\"$(dirname \"$0\")/cluster\"\n"), 0o755))
	name := docker.ClusterName("dockertest-" + strconv.FormatInt(time.Now().UnixNano()%1_000_000, 10))
	t.Cleanup(func() {
		require.NoError(t, docker.Down(context.WithoutCancel(t.Context()), name, childEnv))
	})
	require.NoError(t, docker.Up(t.Context(), docker.Cluster{Name: name, Up: up, Dir: dir, Cores: lane}, childEnv))

	require.Equal(t, run.CoreList(lane), cpuset(t, name+"-control-plane"), "the cluster's node lands on its lane's cores")
	cluster, err := os.ReadFile(filepath.Join(dir, "cluster"))
	require.NoError(t, err)
	require.Equal(t, name+"\n", string(cluster), "up.sh runs with KIND_CLUSTER_NAME set to the cluster's name")
}
