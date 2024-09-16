// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/z5labs/bedrock/pkg/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type configSourceFunc func(config.Store) error

func (f configSourceFunc) Apply(store config.Store) error {
	return f(store)
}

type appFunc func(context.Context) error

func (f appFunc) Run(ctx context.Context) error {
	return f(ctx)
}

func TestRun(t *testing.T) {
	t.Run("will log a record to stdout", func(t *testing.T) {
		t.Run("if no otlp target is set", func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "log.json")
			f, err := os.Create(filename)
			if !assert.Nil(t, err) {
				return
			}

			done := make(chan struct{})
			go func() {
				defer close(done)

				stdout := os.Stdout
				defer func() {
					os.Stdout = stdout
				}()

				os.Stdout = f

				app := appFunc(func(ctx context.Context) error {
					log := Logger("app")
					log.InfoContext(ctx, "hello")
					return nil
				})

				Run(strings.NewReader(""), func(ctx context.Context, cfg Config) (App, error) {
					return app, nil
				})
			}()

			select {
			case <-time.After(30 * time.Second):
				t.Fail()
				return
			case <-done:
			}

			err = f.Close()
			if !assert.Nil(t, err) {
				return
			}

			f, err = os.Open(filename)
			if !assert.Nil(t, err) {
				return
			}
			defer f.Close()

			type log struct {
				Body struct {
					Value string `json:"Value"`
				} `json:"Body"`
				Scope struct {
					Name string `json:"Name"`
				} `json:"Scope"`
			}

			var l log
			dec := json.NewDecoder(f)
			err = dec.Decode(&l)
			if !assert.Nil(t, err) {
				return
			}
			if !assert.Equal(t, "app", l.Scope.Name) {
				return
			}
			if !assert.Equal(t, "hello", l.Body.Value) {
				return
			}
		})
	})

	t.Run("will return an error", func(t *testing.T) {
		t.Run("if it fails to read one of the config sources", func(t *testing.T) {
			build := func(ctx context.Context, cfg Config) (App, error) {
				return nil, nil
			}

			srcErr := errors.New("failed to apply config")
			src := configSourceFunc(func(s config.Store) error {
				return srcErr
			})

			r := runner{
				srcs: []config.Source{src},
			}

			err := run(r, build)
			if !assert.Equal(t, srcErr, err) {
				t.Log(err)
				return
			}
		})

		t.Run("if provided build function fails", func(t *testing.T) {
			buildErr := errors.New("failed to build custom app")
			build := func(ctx context.Context, cfg Config) (App, error) {
				return nil, buildErr
			}

			r := runner{
				detectResource: func(ctx context.Context, oc OTelConfig) (*resource.Resource, error) {
					return resource.NewSchemaless(), nil
				},
				newLogExporter: func(ctx context.Context, oc OTelConfig) (sdklog.Exporter, error) {
					return nil, nil
				},
			}

			err := run(r, build)

			if !assert.ErrorIs(t, err, buildErr) {
				t.Log(err)
				return
			}
		})

		t.Run("if the built app returns an error", func(t *testing.T) {
			appErr := errors.New("failed to run app")
			app := appFunc(func(ctx context.Context) error {
				return appErr
			})

			build := func(ctx context.Context, cfg Config) (App, error) {
				return app, nil
			}

			r := runner{
				detectResource: func(ctx context.Context, oc OTelConfig) (*resource.Resource, error) {
					return resource.NewSchemaless(), nil
				},
				newLogExporter: func(ctx context.Context, oc OTelConfig) (sdklog.Exporter, error) {
					return nil, nil
				},
			}

			err := run(r, build)

			if !assert.ErrorIs(t, err, appErr) {
				t.Log(err)
				return
			}
		})
	})

	t.Run("will export signals", func(t *testing.T) {
		t.Run("if the otlp target is set", func(t *testing.T) {
			app := appFunc(func(ctx context.Context) error {
				spanCtx, span := otel.Tracer("app").Start(ctx, "Run")
				defer span.End()

				log := Logger("app")
				log.InfoContext(spanCtx, "hello")

				counter, err := otel.Meter("app").Int64Counter("Run")
				if err != nil {
					return err
				}
				counter.Add(spanCtx, 1)

				return nil
			})

			build := func(ctx context.Context, cfg Config) (App, error) {
				return app, nil
			}

			var tracesBuf bytes.Buffer
			var metricsBuf bytes.Buffer
			var logsBuf bytes.Buffer
			r := runner{
				srcs: []config.Source{
					config.FromJson(strings.NewReader(`{
						"otel": {
							"trace": {
								"sampling": 1.0
							},
							"otlp": {
								"target": "localhost:8080"
							}
						}
					}`)),
				},
				detectResource: func(ctx context.Context, oc OTelConfig) (*resource.Resource, error) {
					return resource.NewSchemaless(), nil
				},
				newTraceExporter: func(ctx context.Context, oc OTelConfig) (sdktrace.SpanExporter, error) {
					return stdouttrace.New(
						stdouttrace.WithWriter(&tracesBuf),
					)
				},
				newMetricExporter: func(ctx context.Context, oc OTelConfig) (sdkmetric.Exporter, error) {
					return stdoutmetric.New(
						stdoutmetric.WithWriter(&metricsBuf),
					)
				},
				newLogExporter: func(ctx context.Context, oc OTelConfig) (sdklog.Exporter, error) {
					return stdoutlog.New(
						stdoutlog.WithWriter(&logsBuf),
					)
				},
			}

			err := run(r, build)
			if !assert.Nil(t, err) {
				return
			}

			type span struct {
				Name string `json:"Name"`
			}

			var spans []span
			dec := json.NewDecoder(&tracesBuf)
			for {
				var span span
				err = dec.Decode(&span)
				if err != nil {
					t.Log(err)
					break
				}
				spans = append(spans, span)
			}
			if !assert.Len(t, spans, 2) {
				return
			}
			if !assert.Equal(t, "buildApp.Build", spans[0].Name) {
				return
			}
			if !assert.Equal(t, "Run", spans[1].Name) {
				return
			}

			type metric struct {
				ScopeMetrics []struct {
					Metrics []struct {
						Name string `json:"Name"`
						Data struct {
							DataPoints []struct {
								Value int `json:"Value"`
							} `json:"DataPoints"`
						} `json:"Data"`
					} `json:"Metrics"`
				} `json:"ScopeMetrics"`
			}

			var metrics []metric
			dec = json.NewDecoder(&metricsBuf)
			for {
				var m metric
				err = dec.Decode(&m)
				if err != nil {
					t.Log(err)
					break
				}
				metrics = append(metrics, m)
			}
			if !assert.Len(t, metrics, 1) {
				return
			}

			m := metrics[0].ScopeMetrics[0].Metrics[0]
			if !assert.Equal(t, "Run", m.Name) {
				return
			}
			if !assert.Equal(t, 1, m.Data.DataPoints[0].Value) {
				return
			}

			type log struct {
				Body struct {
					Value string `json:"Value"`
				} `json:"Body"`
				Scope struct {
					Name string `json:"Name"`
				} `json:"Scope"`
			}

			var logs []log
			dec = json.NewDecoder(&logsBuf)
			for {
				var l log
				err = dec.Decode(&l)
				if err != nil {
					t.Log(err)
					break
				}
				logs = append(logs, l)
			}
			if !assert.Len(t, logs, 1) {
				return
			}

			l := logs[0]
			if !assert.Equal(t, "hello", l.Body.Value) {
				return
			}
			if !assert.Equal(t, "app", l.Scope.Name) {
				return
			}
		})
	})
}

func TestBuildApp(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if the given build function returns an error", func(t *testing.T) {
			buildErr := errors.New("failed to build")
			build := func(ctx context.Context, cfg Config) (App, error) {
				return nil, buildErr
			}

			_, err := buildApp(build, nil).Build(context.Background(), Config{})
			if !assert.Equal(t, buildErr, err) {
				return
			}
		})
	})
}

func TestRunner_InitTracerProvider(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if it fails to detect the resource", func(t *testing.T) {
			detectErr := errors.New("failed to detect resource")
			r := runner{
				detectResource: func(ctx context.Context, oc OTelConfig) (*resource.Resource, error) {
					return nil, detectErr
				},
			}

			var cfg OTelConfig
			cfg.OTLP.Target = "localhost"

			_, err := r.initTracerProvider(cfg, nil)(context.Background())
			if !assert.Equal(t, detectErr, err) {
				return
			}
		})

		t.Run("if it fails to create a new exporter", func(t *testing.T) {
			exporterErr := errors.New("failed to create new exporter")
			r := runner{
				detectResource: detectResource,
				newTraceExporter: func(ctx context.Context, oc OTelConfig) (sdktrace.SpanExporter, error) {
					return nil, exporterErr
				},
			}

			var cfg OTelConfig
			cfg.OTLP.Target = "localhost"

			_, err := r.initTracerProvider(cfg, nil)(context.Background())
			if !assert.Equal(t, exporterErr, err) {
				return
			}
		})
	})
}

func TestRunner_InitMeterProvider(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if it fails to detect the resource", func(t *testing.T) {
			detectErr := errors.New("failed to detect resource")
			r := runner{
				detectResource: func(ctx context.Context, oc OTelConfig) (*resource.Resource, error) {
					return nil, detectErr
				},
			}

			var cfg OTelConfig
			cfg.OTLP.Target = "localhost"

			_, err := r.initMeterProvider(cfg, nil)(context.Background())
			if !assert.Equal(t, detectErr, err) {
				return
			}
		})

		t.Run("if it fails to create a new exporter", func(t *testing.T) {
			exporterErr := errors.New("failed to create new exporter")
			r := runner{
				detectResource: detectResource,
				newMetricExporter: func(ctx context.Context, oc OTelConfig) (sdkmetric.Exporter, error) {
					return nil, exporterErr
				},
			}

			var cfg OTelConfig
			cfg.OTLP.Target = "localhost"

			_, err := r.initMeterProvider(cfg, nil)(context.Background())
			if !assert.Equal(t, exporterErr, err) {
				return
			}
		})
	})
}

func TestRunner_InitLoggerProvider(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if it fails to detect the resource", func(t *testing.T) {
			detectErr := errors.New("failed to detect resource")
			r := runner{
				detectResource: func(ctx context.Context, oc OTelConfig) (*resource.Resource, error) {
					return nil, detectErr
				},
			}

			var cfg OTelConfig
			cfg.OTLP.Target = "localhost"

			_, err := r.initLoggerProvider(cfg, nil)(context.Background())
			if !assert.Equal(t, detectErr, err) {
				return
			}
		})

		t.Run("if it fails to create a new exporter", func(t *testing.T) {
			exporterErr := errors.New("failed to create new exporter")
			r := runner{
				detectResource: detectResource,
				newLogExporter: func(ctx context.Context, oc OTelConfig) (sdklog.Exporter, error) {
					return nil, exporterErr
				},
			}

			var cfg OTelConfig
			cfg.OTLP.Target = "localhost"

			_, err := r.initLoggerProvider(cfg, nil)(context.Background())
			if !assert.Equal(t, exporterErr, err) {
				return
			}
		})
	})
}
