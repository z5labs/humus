// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/z5labs/humus/tools/flaky/internal/env"
)

// ClusterName returns the name of the kind cluster that the walkthrough
// named walkthrough is brought up on: flaky-<walkthrough>.
func ClusterName(walkthrough string) string {
	return "flaky-" + walkthrough
}

// Context returns the kubectl context that kind gives the cluster name.
func Context(name string) string {
	return "kind-" + name
}

// CheckKind returns an error when kind or kubectl isn't on PATH.
func CheckKind() error {
	for _, name := range []string{"kind", "kubectl"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("the functional level needs kind and kubectl: %w", err)
		}
	}
	return nil
}

// BuildArgs returns the arguments of the docker build command that builds
// the image tagged tag from dockerfile, with the repository's root as the
// context, as CI's kind jobs do.
func BuildArgs(dockerfile, tag string) []string {
	return []string{"build", "--file", dockerfile, "--tag", tag, "."}
}

// Build builds the image tagged tag from dockerfile, in the repository's root
// root, which is the build's context.
func Build(ctx context.Context, root, dockerfile, tag string, childEnv []string) error {
	_, err := command(ctx, root, childEnv, "docker", BuildArgs(dockerfile, tag)...)
	return err
}

// Cluster is a kind cluster to bring a walkthrough up on.
type Cluster struct {
	// Name is the cluster's name.
	Name string
	// Up is the path of the walkthrough's deploy/kind/up.sh.
	Up string
	// Dir is the directory up.sh runs in.
	Dir string
	// Cores are the cores its node containers are pinned to.
	Cores []int
}

// Up brings the cluster c up:
//
//  1. it creates a kind cluster named c.Name;
//  2. it pins each of its node containers to c.Cores, with docker update
//     --cpuset-cpus;
//  3. it runs c.Up with KIND_CLUSTER_NAME set to c.Name, so up.sh deploys onto
//     the cluster that is already there;
//  4. it waits for every Deployment on the cluster to roll out, as CI's kind
//     jobs do.
//
// It returns the first step's error, with what the step printed. It never
// deletes the cluster: Down does, whether Up succeeded or not.
func Up(ctx context.Context, c Cluster, childEnv []string) error {
	if _, err := command(ctx, c.Dir, childEnv, "kind", "create", "cluster", "--name", c.Name, "--wait", "2m"); err != nil {
		return err
	}
	nodes, err := command(ctx, c.Dir, childEnv, "kind", "get", "nodes", "--name", c.Name)
	if err != nil {
		return err
	}
	if len(strings.Fields(nodes)) == 0 {
		return fmt.Errorf("kind lists no nodes of the cluster %s", c.Name)
	}
	for _, node := range strings.Fields(nodes) {
		if err := update(ctx, node, c.Cores, childEnv); err != nil {
			return err
		}
	}
	if _, err := command(ctx, c.Dir, env.With(childEnv, "KIND_CLUSTER_NAME", c.Name), c.Up); err != nil {
		return err
	}
	return RollOut(ctx, c.Name, childEnv)
}

// RollOut waits for every Deployment on the kind cluster name to roll out.
func RollOut(ctx context.Context, name string, childEnv []string) error {
	kubeContext := Context(name)
	deployments, err := command(ctx, "", childEnv, "kubectl", "--context", kubeContext, "get", "deployments", "--all-namespaces",
		"--output", `jsonpath={range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}`)
	if err != nil {
		return err
	}
	for line := range strings.Lines(deployments) {
		namespace, deployment, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if _, err := command(ctx, "", childEnv, "kubectl", "--context", kubeContext, "--namespace", namespace,
			"rollout", "status", "deployment/"+deployment, "--timeout", "5m"); err != nil {
			return err
		}
	}
	return nil
}

// Down deletes the kind cluster name. kind deletes a cluster that isn't there
// without an error.
func Down(ctx context.Context, name string, childEnv []string) error {
	_, err := command(ctx, "", childEnv, "kind", "delete", "cluster", "--name", name)
	return err
}

// command runs name with args in dir, and returns what it printed to stdout.
// Its error gives everything it printed.
func command(ctx context.Context, dir string, childEnv []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = childEnv
	var stdout bytes.Buffer
	all := &lockedBuffer{}
	cmd.Stdout = io.MultiWriter(&stdout, all)
	cmd.Stderr = all
	if err := cmd.Run(); err != nil {
		line := strings.Join(append([]string{name}, args...), " ")
		return "", fmt.Errorf("%s: %w\n%s", line, err, strings.TrimSpace(all.String()))
	}
	return stdout.String(), nil
}

// lockedBuffer is a bytes.Buffer that a command's stdout and stderr can both
// write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
