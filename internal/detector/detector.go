// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package detector

import (
	"context"
	"os"
	"path/filepath"

	"go.opentelemetry.io/otel/sdk"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

type telemetrySDK struct{}

func TelemetrySDK() resource.Detector {
	return telemetrySDK{}
}

func (telemetrySDK) Detect(context.Context) (*resource.Resource, error) {
	return resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.TelemetrySDKName("opentelemetry"),
		semconv.TelemetrySDKLanguageGo,
		semconv.TelemetrySDKVersion(sdk.Version()),
	), nil
}

func Host() resource.Detector {
	return resource.StringDetector(semconv.SchemaURL, semconv.HostNameKey, os.Hostname)
}

func ServiceName(name string) resource.Detector {
	return resource.StringDetector(semconv.SchemaURL, semconv.ServiceNameKey, func() (string, error) {
		if len(name) > 0 {
			return name, nil
		}
		executable, err := os.Executable()
		if err != nil {
			return "unknown_service:go", nil
		}
		return "unknown_service:" + filepath.Base(executable), nil
	})
}

func ServiceVersion(version string) resource.Detector {
	return resource.StringDetector(semconv.SchemaURL, semconv.ServiceVersionKey, func() (string, error) {
		return version, nil
	})
}
