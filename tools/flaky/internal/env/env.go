// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package env builds the environment of the processes flaky starts.
package env

import "github.com/z5labs/bedrock"

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
	"KUBECONFIG", "KIND_CLUSTER_NAME",
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
