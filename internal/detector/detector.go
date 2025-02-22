// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package detector

import (
	"context"
	"os"

	"go.opentelemetry.io/otel/attribute"
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

func String(key attribute.Key, f func() (string, error)) resource.Detector {
	return resource.StringDetector(semconv.SchemaURL, key, f)
}
