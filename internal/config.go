// Copyright (c) 2024 Z5labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package internal

import (
	"io"
	"os"

	"github.com/z5labs/bedrock/config"
)

func ConfigSource(r io.Reader) config.Source {
	return config.FromYaml(
		config.RenderTextTemplate(
			r,
			config.TemplateFunc("env", func(key string) any {
				v, ok := os.LookupEnv(key)
				if ok {
					return v
				}
				return nil
			}),
			config.TemplateFunc("default", func(def, v any) any {
				if v == nil {
					return def
				}
				return v
			}),
		),
	)
}
