// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"bytes"
	"context"
	_ "embed"

	"github.com/z5labs/humus/app"
	appconfig "github.com/z5labs/humus/example/job/1brc-walkthrough/app"

	"github.com/z5labs/bedrock/config"
)

//go:embed config.yaml
var configBytes []byte

func main() {
	// Create a builder that reads config and initializes the runtime
	builder := app.BuilderFunc[app.Runtime](func(ctx context.Context) (app.Runtime, error) {
		// Read and parse config using bedrock (temporary - will be migrated to config.Reader)
		var cfg appconfig.Config
		src := config.FromYaml(bytes.NewReader(configBytes))
		mgr, err := config.Read(src)
		if err != nil {
			return nil, err
		}
		err = mgr.Unmarshal(&cfg)
		if err != nil {
			return nil, err
		}

		// Initialize runtime using the config
		runtime, err := appconfig.Init(ctx, cfg)
		if err != nil {
			return nil, err
		}

		return runtime, nil
	})

	// Run the application
	app.Run(context.Background(), builder)
}
