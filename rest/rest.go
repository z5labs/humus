// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package rest provides a [humus.App] implementation for building RESTful APIs.
package rest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/internal"
	"github.com/z5labs/humus/internal/global"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/pkg/app"
	"github.com/z5labs/bedrock/pkg/health"
	"github.com/z5labs/bedrock/rest"
)

//go:embed default_config.yaml
var configBytes []byte

func init() {
	global.RegisterConfigSource(internal.ConfigSource(bytes.NewReader(configBytes)))
}

// Config
type Config struct {
	humus.Config `config:",squash"`

	Http struct {
		Port uint `config:"port"`
	} `config:"http"`

	OpenApi struct {
		Title   string `config:"title"`
		Version string `config:"version"`
	} `config:"openapi"`
}

// Option
type Option func(*App)

// Title
func Title(s string) Option {
	return func(a *App) {
		a.restOpts = append(a.restOpts, rest.Title(s))
	}
}

// Version
func Version(s string) Option {
	return func(a *App) {
		a.restOpts = append(a.restOpts, rest.Version(s))
	}
}

// ListenOn
func ListenOn(port uint) Option {
	return func(a *App) {
		a.port = port
	}
}

// LifecycleHook
type LifecycleHook app.LifecycleHook

// PostRun
func PostRun(hook LifecycleHook) Option {
	return func(a *App) {
		a.postRun = append(a.postRun, hook)
	}
}

// Metric
type Metric health.Metric

// Readiness
func Readiness(m Metric) Option {
	return func(a *App) {
		a.readiness = m
	}
}

// Liveness
func Liveness(m Metric) Option {
	return func(a *App) {
		a.liveness = m
	}
}

// App
type App struct {
	readiness health.Metric
	liveness  health.Metric

	port     uint
	restOpts []rest.Option
	postRun  []app.LifecycleHook
}

// New returns an initialized [App].
func New(opts ...Option) *App {
	a := &App{
		readiness: &health.Binary{},
		liveness:  &health.Binary{},
		port:      80,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Run implements the [humus.App] interface.
func (a *App) Run(ctx context.Context) error {
	healthEndpoints := []Endpoint{
		readinessEndpoint(a.readiness),
		livenessEndpoint(a.liveness),
	}
	for _, e := range healthEndpoints {
		a.restOpts = append(a.restOpts, rest.Endpoint(e.method, e.path, e.operation))
	}

	ls, err := net.Listen("tcp", fmt.Sprintf(":%d", a.port))
	if err != nil {
		return err
	}
	a.restOpts = append(a.restOpts, rest.Listener(ls))

	var base bedrock.App = rest.NewApp(a.restOpts...)

	base = app.WithLifecycleHooks(base, app.Lifecycle{
		PostRun: composeLifecycleHooks(a.postRun...),
	})

	return base.Run(ctx)
}

func composeLifecycleHooks(hooks ...app.LifecycleHook) app.LifecycleHook {
	return app.LifecycleHookFunc(func(ctx context.Context) error {
		var errs []error
		for _, hook := range hooks {
			err := runHook(ctx, hook)
			if err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) == 0 {
			return nil
		}
		return errors.Join(errs...)
	})
}

func runHook(ctx context.Context, hook app.LifecycleHook) (err error) {
	defer internal.Recover(&err)

	return hook.Run(ctx)
}
