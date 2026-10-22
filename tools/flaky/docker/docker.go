// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package docker keeps the containers that the testcontainers tests start off
// the lanes' cores.
//
// A testcontainers test's containers, such as Kafka, PostgreSQL or k3s, are
// started by dockerd, so pinning the test's process doesn't pin them. So
// flaky watches Docker's events, and pins each container labelled
// org.testcontainers to the cores kept back from the lanes as it starts, with
// docker update --cpuset-cpus. Only the code under test, which runs in the
// test's process, gets the run's CPU count and load.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/z5labs/humus/tools/flaky/run"
)

// Label is the label testcontainers-go puts on every container it starts.
const Label = "org.testcontainers"

// Check returns an error when the docker command can't reach a daemon.
func Check(ctx context.Context, env []string) error {
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the testcontainers level needs Docker: docker version: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// EventsArgs returns the arguments of the docker events command that lists
// the ID of each container labelled Label as it starts, from since on.
func EventsArgs(since time.Time) []string {
	return []string{
		"events",
		"--since", strconv.FormatInt(since.Unix(), 10),
		"--filter", "type=container",
		"--filter", "event=start",
		"--filter", "label=" + Label,
		"--format", "{{.Actor.ID}}",
	}
}

// UpdateArgs returns the arguments of the docker update command that pins
// the container id to cores.
func UpdateArgs(id string, cores []int) []string {
	return []string{"update", "--cpuset-cpus", run.CoreList(cores), id}
}

// Pinner pins each container labelled Label to its cores as the container
// starts, until it is stopped.
type Pinner struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	err error
}

// Pin starts watching Docker's events, and pins each container labelled Label
// to cores as it starts, until Stop is called. Every container that starts
// once Pin has returned is pinned, and some that started in the second before.
//
// A container it can't pin, and the watch ending before Stop, are warned of
// on log.
func Pin(ctx context.Context, cores []int, env []string, log io.Writer) (*Pinner, error) {
	if len(cores) == 0 {
		return nil, errors.New("no cores to pin the testcontainers containers to")
	}
	ctx, cancel := context.WithCancel(ctx)
	// The watch stops on its own when its output can't be read, and
	// cmdCtx is how it stops docker events then.
	cmdCtx, stopCmd := context.WithCancel(ctx)
	// --since replays the events from just before the watch started, so
	// a container that starts while docker events is connecting is still
	// pinned.
	cmd := exec.CommandContext(cmdCtx, "docker", EventsArgs(time.Now().Add(-time.Second))...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		stopCmd()
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stopCmd()
		cancel()
		return nil, fmt.Errorf("watching Docker's events: %w", err)
	}
	p := &Pinner{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer stopCmd()
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			id := strings.TrimSpace(sc.Text())
			if id == "" {
				continue
			}
			if err := update(ctx, id, cores, env); err != nil && ctx.Err() == nil {
				warn(log, "warning: %v\n", err)
			}
		}
		scanErr := sc.Err()
		if scanErr != nil {
			stopCmd()
		}
		waitErr := cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		err := fmt.Errorf("docker events ended: %v: %s", waitErr, strings.TrimSpace(stderr.String()))
		if scanErr != nil {
			err = fmt.Errorf("reading Docker's events: %w", scanErr)
		}
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		warn(log, "warning: %v; the testcontainers containers started from now on aren't pinned\n", err)
	}()
	return p, nil
}

// Stop stops watching, and returns the error that ended the watch before
// Stop, if one did.
func (p *Pinner) Stop() error {
	p.cancel()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func update(ctx context.Context, id string, cores []int, env []string) error {
	cmd := exec.CommandContext(ctx, "docker", UpdateArgs(id, cores)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pinning container %s to cores %s: %w: %s", id, run.CoreList(cores), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// warn writes to the log. A failed write there has nowhere left to be
// reported, so it is dropped.
func warn(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
