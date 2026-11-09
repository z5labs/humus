// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build testcontainers

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus/tools/flaky/results"
)

// On a daemon Ryuk can't run on, such as rootless Podman with SELinux
// enforcing, go test passes the RabbitMQ tests only with
// TESTCONTAINERS_RYUK_DISABLED=true, so flaky stress must pass it on. Only
// such a daemon shows Ryuk dying, which the stand-in docker can't.
func TestStress_passes_a_rabbitmq_testcontainers_test_that_go_test_passes(t *testing.T) {
	needCores(t, 9)
	disabled, err := (bedrock.OSEnvironment{}).Value("TESTCONTAINERS_RYUK_DISABLED")
	if err != nil || disabled != "true" {
		t.Skip("this Docker daemon runs Ryuk, so go test needs no testcontainers configuration either")
	}
	vars := flakyEnv(t)
	for _, name := range []string{"DOCKER_HOST", "TESTCONTAINERS_RYUK_DISABLED"} {
		if v, err := (bedrock.OSEnvironment{}).Value(name); err == nil {
			vars[name] = v
		}
	}
	wd, err := os.Getwd()
	require.NoError(t, err)
	t.Chdir(filepath.Join(wd, "..", ".."))

	const test = "TestPublisher_a_Publish_right_after_close_all_connections_is_confirmed_on_a_new_connection"
	p, status := bedrocktest.Start(t, bedrock.AppFunc(flaky),
		bedrocktest.WithArgs("stress", "-tags", "testcontainers", "-run", "^"+test+"$", "-cpu", "1", "-load", "1", "-count", "1", "-json", "./queue/rabbitmq"),
		bedrocktest.WithEnv(vars))
	st := <-status
	t.Logf("stderr:\n%s", p.Stderr())

	var rep results.Report
	require.NoError(t, json.Unmarshal([]byte(p.Stdout()), &rep), "stdout:\n%s", p.Stdout())
	for _, f := range rep.Failing {
		for _, pair := range f.Pairs {
			t.Logf("failing: %s cpu %d load %d: %d of %d runs failed", f.Test, pair.CPU, pair.Load, pair.Failures, pair.Runs)
		}
		t.Logf("first failure of %s:\n%s", f.Test, strings.Join(f.FirstFailure, "\n"))
	}
	require.Equal(t, 0, st.Code(), "the test passes under flaky stress, as it does under go test")
	require.Empty(t, rep.Failing)
	require.Equal(t, 1, rep.Passed)
}
