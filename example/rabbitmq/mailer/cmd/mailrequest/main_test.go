// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"crypto/rand"
	"maps"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const unset = "\x00unset"

type run struct {
	code   int
	cause  error
	stdout string
	stderr string
}

func start(t *testing.T, files map[string][]byte, env map[string]string, build telemetry.Builder, args ...string) run {
	t.Helper()
	all := map[string][]byte{
		"secrets/rabbitmq-password": []byte(rand.Text() + "\n"),
		"certs/rabbitmq-ca.pem":     []byte("not a certificate"),
	}
	maps.Copy(all, files)
	h := humustest.NewHost(t, humustest.WithFiles(all))

	merged := map[string]string{
		"RABBITMQ_ADDR":          "rabbitmq.internal:5671",
		"RABBITMQ_USERNAME":      "mailrequest",
		"RABBITMQ_PASSWORD_FILE": h.Path("secrets/rabbitmq-password"),
	}
	for name, value := range env {
		if value == unset {
			delete(merged, name)
			continue
		}
		if _, ok := all[value]; ok {
			value = h.Path(value)
		}
		merged[name] = value
	}
	p, status := humustest.Start(t, Init,
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(build)),
		humustest.WithEnv(merged),
		humustest.WithArgs(args...),
	)
	st := <-status
	return run{code: st.Code(), cause: st.Cause(), stdout: p.Stdout(), stderr: p.Stderr()}
}

func TestInit_exits_ExitConfig_on_a_bad_setting(t *testing.T) {
	request := []string{"-to", "someone@example.com", "-subject", "Hello", "-text", "Hello, world."}
	testCases := map[string]struct {
		files map[string][]byte
		env   map[string]string
		args  []string
		want  []string
	}{
		"no settings at all": {
			env: map[string]string{"RABBITMQ_ADDR": unset, "RABBITMQ_USERNAME": unset, "RABBITMQ_PASSWORD_FILE": unset},
			want: []string{
				"config -to: not found",
				"config -subject: not found",
				"config -text: not found",
				"config RABBITMQ_ADDR: not found",
				"config RABBITMQ_USERNAME: not found",
				"config RABBITMQ_PASSWORD_FILE: not found",
			},
		},
		"an empty flag": {
			args: []string{"-to", "", "-subject", "Hello", "-text", "Hello, world.", "-message-id", ""},
			want: []string{"config -to: must not be empty", "config -message-id: must not be empty"},
		},
		"an unknown flag": {
			args: append([]string{"-cc", "someone@example.com"}, request...),
			want: []string{"config arguments: flag provided but not defined: -cc"},
		},
		"an argument that is not a flag": {
			args: append(append([]string{}, request...), "extra"),
			want: []string{`config arguments: takes flags only, not "extra"`},
		},
		"a bad broker address": {
			env:  map[string]string{"RABBITMQ_ADDR": "rabbitmq.internal"},
			args: request,
			want: []string{"config RABBITMQ_ADDR:"},
		},
		"an empty password file": {
			files: map[string][]byte{"secrets/rabbitmq-password": []byte("\n")},
			args:  request,
			want:  []string{"config RABBITMQ_PASSWORD_FILE: the file is empty"},
		},
		"a CA file with no certificate": {
			env:  map[string]string{"RABBITMQ_CA_FILE": "certs/rabbitmq-ca.pem"},
			args: request,
			want: []string{"config RABBITMQ_CA_FILE: no PEM certificate in the file"},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var got run
			synctest.Test(t, func(t *testing.T) {
				got = start(t, tc.files, tc.env, telemetry.NoopBuilder, tc.args...)
			})
			require.Equal(t, humus.ExitConfig, got.code, "cause: %v", got.cause)
			require.Empty(t, got.stdout)
			for _, want := range tc.want {
				require.ErrorContains(t, got.cause, want)
			}
		})
	}
}

func TestBuildTelemetry_writes_the_logs_to_stderr_and_leaves_stdout_to_the_MessageId(t *testing.T) {
	var got run
	synctest.Test(t, func(t *testing.T) {
		got = start(t, nil, map[string]string{"RABBITMQ_ADDR": unset}, buildTelemetry)
	})
	require.Equal(t, humus.ExitConfig, got.code, "cause: %v", got.cause)
	require.Empty(t, got.stdout)
	require.Contains(t, got.stderr, `"severity_text":"ERROR"`)
	require.Contains(t, got.stderr, "config RABBITMQ_ADDR: not found")
}
