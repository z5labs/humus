// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest

import (
	"context"
	"io"
	"testing"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/internal/defaults"
)

// Option changes the App that Start runs, or the Process it runs on.
type Option func(*startOptions)

// startOptions is what Start's options set: humus's options for the App, in
// the order they were given, and bedrocktest's options for the Process.
type startOptions struct {
	app     []humus.Option
	process []bedrocktest.Option
}

// WithApp applies opts to the App, in order, after Start's defaults. Given
// more than once, it applies every one's options in the order they were
// given, so a later option wins. It panics if one of opts is nil.
func WithApp(opts ...humus.Option) Option {
	for _, opt := range opts {
		if opt == nil {
			panic("humustest: WithApp called with a nil humus.Option")
		}
	}
	opts = append([]humus.Option(nil), opts...)
	return func(o *startOptions) {
		o.app = append(o.app, opts...)
	}
}

// WithEnv, WithArgs and WithStdin set the Process's environment, its
// arguments after the program name, and its standard input. They work like
// bedrocktest's options of the same names: if one is given more than once,
// the last one wins. A helper that sets a base environment and lets a test
// add to it merges the maps itself.
func WithEnv(values map[string]string) Option {
	return withProcess(bedrocktest.WithEnv(values))
}

// WithArgs sets the Process's arguments after the program name, so that
// Args is bedrocktest.ProgramName followed by args. If it is given more
// than once, the last one wins. See [WithEnv].
func WithArgs(args ...string) Option {
	return withProcess(bedrocktest.WithArgs(args...))
}

// WithStdin sets the Process's standard input. If it is given more than
// once, the last one wins. See [WithEnv]. It panics if r is nil.
func WithStdin(r io.Reader) Option {
	if r == nil {
		panic("humustest: WithStdin called with a nil io.Reader")
	}
	return withProcess(bedrocktest.WithStdin(r))
}

// withProcess returns an Option that passes opt to bedrocktest.Start.
// bedrocktest applies its options in order, so for each of its options the
// last one given wins.
func withProcess(opt bedrocktest.Option) Option {
	return func(o *startOptions) {
		o.process = append(o.process, opt)
	}
}

// Start runs the App that humus.Main would run for init with
// bedrocktest.Start, on a new Process. The App has a new Host of this
// package's in place of humus.OSHost(), and opts change the App and the
// Process: humus.WithHost, passed with WithApp, replaces that Host. The
// Host and Init are set before opts are applied, so an Option given with
// WithApp sees both and may wrap them. Call Start inside the
// testing/synctest bubble that runs the App, since the Host is built there.
// It panics if init or one of opts is nil.
func Start(t testing.TB, init func(ctx context.Context, s *humus.Setup) ([]humus.Service, error), opts ...Option) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	if init == nil {
		panic("humustest: Start called with a nil init")
	}
	var o startOptions
	for _, opt := range opts {
		if opt == nil {
			panic("humustest: Start called with a nil Option")
		}
		opt(&o)
	}

	// humus.Main's defaults, from the same internal package, with a test
	// Host in place of humus.OSHost().
	a := humus.App{
		Host:                NewHost(t).Host(),
		ShutdownGracePeriod: defaults.ShutdownGracePeriod,
		Telemetry:           defaults.Telemetry,
		Init:                init,
	}
	for _, opt := range o.app {
		opt(&a)
	}
	return bedrocktest.Start(t, a, o.process...)
}
