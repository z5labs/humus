// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package dockertest_test

import (
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/tools/flaky/internal/dockertest"
)

func TestFake_puts_a_docker_command_first_on_PATH_that_keeps_its_state_beside_it(t *testing.T) {
	dir := dockertest.Fake(t)
	path, err := exec.LookPath("docker")
	require.NoError(t, err)
	require.Equal(t, dir+"/docker", path)

	require.Empty(t, dockertest.Read(t, dir, "updates"), "a file it hasn't written reads as empty")
	out, err := exec.CommandContext(t.Context(), "docker", "update", "--cpuset-cpus", "0", "abc").CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Equal(t, "update --cpuset-cpus 0 abc\n", dockertest.Read(t, dir, "updates"))

	dockertest.Touch(t, dir, "down", "")
	out, err = exec.CommandContext(t.Context(), "docker", "version").CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), "Cannot connect to the Docker daemon")
}

func TestBuffer_is_safe_for_concurrent_use(t *testing.T) {
	var b dockertest.Buffer
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _, _ = b.Write([]byte("line\n")) })
	}
	wg.Wait()
	require.Equal(t, strings.Repeat("line\n", 8), b.String())
}
