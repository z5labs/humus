// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package config

import (
	_ "embed"
	"log/slog"
	"time"
)

// Resource
type Resource struct {
	ServiceName    string `config:"service_name"`
	ServiceVersion string `config:"service_version"`
}

// Trace
type Trace struct {
	Enabled      bool          `config:"enabled"`
	Sampling     float64       `config:"sampling"`
	BatchTimeout time.Duration `config:"batch_timeout"`
}

// Metric
type Metric struct {
	Enabled        bool          `config:"enabled"`
	ExportInterval time.Duration `config:"export_interval"`
}

// Log
type Log struct {
	Enabled      bool          `config:"enabled"`
	BatchTimeout time.Duration `config:"batch_timeout"`
	MinLevel     slog.Level    `config:"min_level"`
}

// OTLP
type OTLP struct {
	Enabled bool   `config:"enabled"`
	Target  string `config:"target"`
}

// OTel
type OTel struct {
	Resource Resource `config:"resource"`
	Trace    Trace    `config:"trace"`
	Metric   Metric   `config:"metric"`
	Log      Log      `config:"log"`
	OTLP     OTLP     `config:"otlp"`
}
