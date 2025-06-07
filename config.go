// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"os"

	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/internal/otel"

	bedrockcfg "github.com/z5labs/bedrock/config"
)

// ConfigSource standardizes the template for configuration of humus applications.
// The [io.Reader] is expected to be YAML with support for Go templating. Currently,
// only 2 template functions are supported:
//   - env - this allows environment variables to be substituted into the YAML
//   - default - define a default value in case the original value is nil
func ConfigSource(r io.Reader) bedrockcfg.Source {
	return bedrockcfg.FromYaml(
		bedrockcfg.RenderTextTemplate(
			r,
			bedrockcfg.TemplateFunc("env", func(key string) any {
				v, ok := os.LookupEnv(key)
				if ok {
					return v
				}
				return nil
			}),
			bedrockcfg.TemplateFunc("default", func(def, v any) any {
				if v == nil {
					return def
				}
				return v
			}),
		),
	)
}

//go:embed default_config.yaml
var defaultConfig []byte

// DefaultConfig returns the default config source which corresponds to the [Config] type.
func DefaultConfig() bedrockcfg.Source {
	return ConfigSource(bytes.NewReader(defaultConfig))
}

// Config defines the common configuration for all humus based applications.
type Config struct {
	OTel config.OTel `config:"otel"`
}

// InitializeOTel implements the [appbuilder.OTelInitializer] interface.
func (cfg Config) InitializeOTel(ctx context.Context) error {
	return otel.Initialize(ctx, cfg.OTel)
}
