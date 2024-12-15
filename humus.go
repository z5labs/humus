// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humus provides a base config and abstraction for running apps.
package humus

import (
	_ "embed"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
)

//go:embed default_config.yaml
var DefaultConfig []byte

// OTelConfig
type OTelConfig struct {
	ServiceName    string `config:"service_name"`
	ServiceVersion string `config:"service_version"`

	Trace struct {
		Enabled      bool          `config:"enabled"`
		Sampling     float64       `config:"sampling"`
		BatchTimeout time.Duration `config:"batch_timeout"`
	} `config:"trace"`

	Metric struct {
		Enabled      bool          `config:"enabled"`
		ExportPeriod time.Duration `config:"export_period"`
	} `config:"metric"`

	Log struct {
		Enabled      bool          `config:"enabled"`
		BatchTimeout time.Duration `config:"batch_timeout"`
	} `config:"log"`

	OTLP struct {
		Target string `config:"target"`
	} `config:"otlp"`
}

// LoggingConfig
type LoggingConfig struct {
	Level slog.Level `config:"level"`
}

// Config
type Config struct {
	OTel    OTelConfig    `config:"otel"`
	Logging LoggingConfig `config:"logging"`
}

// Logger
func Logger(name string) *slog.Logger {
	return otelslog.NewLogger(name)
}
