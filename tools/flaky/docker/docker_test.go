// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package docker_test

import (
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
	require.NoError(t, docker.Check(t.Context(), childEnv))

	dockertest.Touch(t, dir, "down", "")
	err := docker.Check(t.Context(), childEnv)
	require.ErrorContains(t, err, "the testcontainers level needs Docker")
	require.ErrorContains(t, err, "Cannot connect to the Docker daemon")
}
