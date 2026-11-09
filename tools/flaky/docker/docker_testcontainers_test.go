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

// pull pulls the image unless Docker has it already, so the test asks no
// registry for it, and isn't rate limited by one, on every run of a sweep.
func pull(t *testing.T) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "docker", "image", "inspect", image)
	cmd.Env = env.Child(bedrock.OSEnvironment{})
	if cmd.Run() == nil {
		return
	}
	dockerRun(t, "pull", "-q", image)
}

func cpuset(t *testing.T, id string) string {
	return dockerRun(t, "inspect", "--format", "{{.HostConfig.CpusetCpus}}", id)
}

// eventually reports whether cond holds within 30s, polling it every 100ms on
// the test's goroutine, so no docker inspect is still running when it
// returns.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// scope returns a Scope label value no other run of a test uses.
func scope(t *testing.T) string {
	return t.Name() + "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// delayedDocker puts a docker first on PATH that runs the real one, except
// that a docker update sleeps for FLAKY_TEST_UPDATE_DELAY first, and then
// appends its arguments to the file FLAKY_TEST_UPDATE_DONE, when its
// environment sets them.
func delayedDocker(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("docker")
	require.NoError(t, err)
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" != update ]; then exec "` + real + `" "$@"; fi
if [ -n "$FLAKY_TEST_UPDATE_DELAY" ]; then sleep "$FLAKY_TEST_UPDATE_DELAY"; fi
"` + real + `" "$@"
status=$?
if [ -n "$FLAKY_TEST_UPDATE_DONE" ]; then echo "$@" >>"$FLAKY_TEST_UPDATE_DONE"; fi
exit $status
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755))
	path, err := bedrock.OSEnvironment{}.Value("PATH")
	require.NoError(t, err)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+path)
}

// delayed returns childEnv with its docker updates delayed by delay, and
// recorded in the file it returns.
func delayed(t *testing.T, childEnv []string, delay string) ([]string, string) {
	done := filepath.Join(t.TempDir(), "updates")
	childEnv = env.With(childEnv, "FLAKY_TEST_UPDATE_DELAY", delay)
	return env.With(childEnv, "FLAKY_TEST_UPDATE_DONE", done), done
}

// updated waits until the docker updates recorded in done name id.
func updated(t *testing.T, done, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(done)
		return err == nil && strings.Contains(string(b), id)
	}, 30*time.Second, 100*time.Millisecond, "the delayed Pinner pins %s", id)
}

// pinCores returns the test's kept-back core, the first of the cores it may
// run on, as lanes.Kept picks it, and the last of them for the Pinner that
// stands in for the sweep's. In a sweep, the test runs on a lane, and the
// sweep's Pinner pins to cores outside it; here the two only need to differ.
func pinCores(t *testing.T, childEnv []string) (kept, sweep []int) {
	t.Helper()
	require.NoError(t, docker.Check(t.Context(), "testcontainers", childEnv))
	cores, err := run.Affinity()
	require.NoError(t, err)
	if len(cores) < 2 {
		t.Skipf("this process may run on %d cores, and the test needs 2", len(cores))
	}
	pull(t)
	return lanes.Kept(cores, 1), cores[len(cores)-1:]
}

// pinner starts a Pinner on cores, the sweep's when scope is "", and stops it
// at the end of the test.
func pinner(t *testing.T, scope string, cores []int, childEnv []string, log *dockertest.Buffer) {
	t.Helper()
	p, err := docker.PinScope(t.Context(), scope, cores, childEnv, log)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, p.Stop()) })
}

// own starts a container labelled Label, and Scope with scope, as the test's
// own.
func own(t *testing.T, scope string) string {
	return start(t, "--label", docker.Label+"=true", "--label", docker.Scope+"="+scope)
}

// others starts a container labelled Label, as another run's.
func others(t *testing.T) string {
	return start(t, "--label", docker.Label+"=true")
}

func TestPin_puts_a_testcontainers_container_on_the_kept_back_cores(t *testing.T) {
	childEnv := env.Child(bedrock.OSEnvironment{})
	kept, _ := pinCores(t, childEnv)
	scope := scope(t)

	var log dockertest.Buffer
	pinner(t, scope, kept, childEnv, &log)

	labelled := own(t, scope)
	unlabelled := start(t)
	require.True(t, eventually(func() bool {
		return cpuset(t, labelled) == run.CoreList(kept)
	}), "the container labelled %s lands on the kept-back cores %v; log:\n%s", docker.Label, kept, log.String())
	require.Empty(t, cpuset(t, unlabelled), "a container without the label isn't pinned")
	require.Empty(t, log.String())
}

func TestPin_puts_a_testcontainers_container_on_the_kept_back_cores_while_the_sweep_s_update_lands_after(t *testing.T) {
	delayedDocker(t)
	childEnv := env.Child(bedrock.OSEnvironment{})
	kept, sweep := pinCores(t, childEnv)
	scope := scope(t)
	sweepEnv, done := delayed(t, childEnv, "2")
	var sweepLog, log dockertest.Buffer
	pinner(t, "", sweep, sweepEnv, &sweepLog)
	pinner(t, scope, kept, childEnv, &log)

	labelled := own(t, scope)
	unlabelled := start(t)
	other := others(t)
	require.True(t, eventually(func() bool {
		return cpuset(t, labelled) == run.CoreList(kept)
	}), "the container labelled %s lands on the kept-back cores %v; log:\n%s", docker.Label, kept, log.String())
	updated(t, done, other)
	require.Equal(t, run.CoreList(kept), cpuset(t, labelled), "the sweep's Pinner, which has seen it start, leaves it on the kept-back cores %v", kept)
	require.Equal(t, run.CoreList(sweep), cpuset(t, other))
	require.Empty(t, cpuset(t, unlabelled), "a container without the label isn't pinned")
	require.Empty(t, log.String())
	require.Empty(t, sweepLog.String())
}

func TestPin_puts_a_testcontainers_container_on_the_kept_back_cores_when_the_sweep_s_update_lands_first(t *testing.T) {
	delayedDocker(t)
	childEnv := env.Child(bedrock.OSEnvironment{})
	kept, sweep := pinCores(t, childEnv)
	scope := scope(t)
	testEnv, done := delayed(t, childEnv, "2")
	var sweepLog, log dockertest.Buffer
	pinner(t, "", sweep, childEnv, &sweepLog)
	pinner(t, scope, kept, testEnv, &log)

	labelled := own(t, scope)
	unlabelled := start(t)
	updated(t, done, labelled)
	require.Equal(t, run.CoreList(kept), cpuset(t, labelled), "the container labelled %s lands on the kept-back cores %v; log:\n%s", docker.Label, kept, log.String())
	require.Empty(t, cpuset(t, unlabelled), "a container without the label isn't pinned")
	require.Empty(t, log.String())
}

func TestPin_keeps_another_run_s_testcontainers_container_on_the_sweep_s_kept_back_cores(t *testing.T) {
	delayedDocker(t)
	childEnv := env.Child(bedrock.OSEnvironment{})
	kept, sweep := pinCores(t, childEnv)
	scope := scope(t)
	testEnv, done := delayed(t, childEnv, "2")
	var sweepLog, log dockertest.Buffer
	pinner(t, "", sweep, childEnv, &sweepLog)
	pinner(t, scope, kept, testEnv, &log)

	other := others(t)
	labelled := own(t, scope)
	require.True(t, eventually(func() bool {
		return cpuset(t, other) == run.CoreList(sweep)
	}), "another run's container lands on the sweep's kept-back cores %v; log:\n%s", sweep, sweepLog.String())
	updated(t, done, labelled)
	require.Equal(t, run.CoreList(sweep), cpuset(t, other), "the test's Pinner, which has seen it start, leaves another run's container on the sweep's kept-back cores %v", sweep)
	require.Equal(t, run.CoreList(kept), cpuset(t, labelled))
	require.Empty(t, log.String())
	require.Empty(t, sweepLog.String())
}

func TestPin_puts_a_testcontainers_container_on_the_kept_back_cores_while_the_sweep_watches(t *testing.T) {
	childEnv := env.Child(bedrock.OSEnvironment{})
	kept, sweep := pinCores(t, childEnv)
	scope := scope(t)
	var sweepLog, log dockertest.Buffer
	pinner(t, "", sweep, childEnv, &sweepLog)
	pinner(t, scope, kept, childEnv, &log)

	labelled := own(t, scope)
	unlabelled := start(t)
	other := others(t)
	require.True(t, eventually(func() bool {
		return cpuset(t, labelled) == run.CoreList(kept) && cpuset(t, other) == run.CoreList(sweep)
	}), "the container labelled %s lands on the kept-back cores %v, and another run's on the sweep's %v; log:\n%s", docker.Label, kept, sweep, log.String())
	require.Equal(t, run.CoreList(kept), cpuset(t, labelled))
	require.Empty(t, cpuset(t, unlabelled), "a container without the label isn't pinned")
	require.Empty(t, log.String())
	require.Empty(t, sweepLog.String())
}

func TestPin_puts_a_testcontainers_container_on_the_kept_back_cores_just_after_another_run_s_container_is_gone(t *testing.T) {
	childEnv := env.Child(bedrock.OSEnvironment{})
	kept, _ := pinCores(t, childEnv)
	scope := scope(t)
	dockerRun(t, "run", "--rm", "--label", docker.Label+"=true", image, "true")
	var log dockertest.Buffer
	pinner(t, scope, kept, childEnv, &log)

	labelled := own(t, scope)
	unlabelled := start(t)
	require.True(t, eventually(func() bool {
		return cpuset(t, labelled) == run.CoreList(kept)
	}), "the container labelled %s lands on the kept-back cores %v; log:\n%s", docker.Label, kept, log.String())
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
