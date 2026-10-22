// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package env builds the environment of the processes flaky starts.
package env

import (
	"strings"

	"github.com/z5labs/bedrock"
)

// names are the variables a child process is given, when they are set. A
// bedrock.Environment can't be listed, so the go command, git and the test
// binaries see these and nothing else.
var names = []string{
	"PATH", "HOME", "USER", "LANG", "TMPDIR", "TZ",
	"XDG_CACHE_HOME", "XDG_CONFIG_HOME",
	"GOROOT", "GOPATH", "GOBIN", "GOCACHE", "GOMODCACHE", "GOENV", "GOFLAGS",
	"GOPROXY", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOSUMDB", "GOINSECURE",
	"GOTOOLCHAIN", "GOWORK", "GOEXPERIMENT", "GODEBUG", "GOAMD64", "GOARM64",
	"CGO_ENABLED", "CC", "CXX", "PKG_CONFIG",
	"DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY",
	"KUBECONFIG", "KIND_CLUSTER_NAME", "KIND_EXPERIMENTAL_PROVIDER",
}

// ghNames are the variables the gh command is given, besides names, when
// they are set: its own, and those its keyring is reached with.
var ghNames = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
	"GH_HOST", "GH_REPO", "GH_CONFIG_DIR", "GH_PROMPT_DISABLED",
	"DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "XDG_DATA_HOME", "XDG_STATE_HOME",
}

// GH returns the environment of the gh command: Child's, and every variable
// gh reads its token, host and repository from, or reaches its keyring with,
// that e has. Only gh is given them, so no test binary sees a token.
func GH(e bedrock.Environment) []string {
	out := Child(e)
	for _, name := range ghNames {
		if v, err := e.Value(name); err == nil {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// Child returns the environment of a child process: every variable in names
// that e has, as NAME=value. It is never nil, since an exec.Cmd with a nil Env
// inherits the real environment.
func Child(e bedrock.Environment) []string {
	out := []string{}
	for _, name := range names {
		if v, err := e.Value(name); err == nil {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// With returns a copy of env, an environment as Child returns, with the
// variable name set to value, in place of any value env gives it.
func With(env []string, name, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return append(out, name+"="+value)
}
