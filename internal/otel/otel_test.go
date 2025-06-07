// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel

import (
	"testing"

	"github.com/z5labs/humus/config"

	"github.com/stretchr/testify/require"
)

func TestInitialize(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		testCases := []struct {
			Name   string
			Config config.OTel
			Assert func(*testing.T, error)
		}{
			{
				Name: "if a unknown otlp conn type is configured for span exporting",
				Config: config.OTel{
					Trace: config.Trace{
						Exporter: config.SpanExporter{
							Type: config.OTLPSpanExporterType,
							OTLP: config.OTLP{
								Type: "unknown",
							},
						},
					},
				},
				Assert: func(t *testing.T, err error) {
					var uerr UnknownOTLPConnTypeError
					require.ErrorAs(t, err, &uerr)
					require.Equal(t, config.OTLPConnType("unknown"), uerr.Type)
					require.NotEmpty(t, uerr.Error())
				},
			},
			{
				Name: "if a unknown span processor type is configured",
				Config: config.OTel{
					Trace: config.Trace{
						Processor: config.SpanProcessor{
							Type: "unknown",
						},
					},
					Metric: config.Metric{
						Reader: config.MetricReader{
							Type: config.PeriodicReaderType,
						},
					},
					Log: config.Log{
						Processor: config.LogProcessor{
							Type: config.BatchLogProcessorType,
						},
					},
				},
				Assert: func(t *testing.T, err error) {
					var uerr UnknownSpanProcessorTypeError
					require.ErrorAs(t, err, &uerr)
					require.Equal(t, config.SpanProcessorType("unknown"), uerr.Type)
					require.NotEmpty(t, uerr.Error())
				},
			},
			{
				Name: "if a unknown otlp conn type is configured for metric exporting",
				Config: config.OTel{
					Trace: config.Trace{
						Processor: config.SpanProcessor{
							Type: config.BatchSpanProcessorType,
						},
					},
					Metric: config.Metric{
						Exporter: config.MetricExporter{
							Type: config.OTLPMetricExporterType,
							OTLP: config.OTLP{
								Type: "unknown",
							},
						},
					},
				},
				Assert: func(t *testing.T, err error) {
					var uerr UnknownOTLPConnTypeError
					require.ErrorAs(t, err, &uerr)
					require.Equal(t, config.OTLPConnType("unknown"), uerr.Type)
					require.NotEmpty(t, uerr.Error())
				},
			},
			{
				Name: "if a unknown metric reader type is configured",
				Config: config.OTel{
					Trace: config.Trace{
						Processor: config.SpanProcessor{
							Type: config.BatchSpanProcessorType,
						},
					},
					Metric: config.Metric{
						Reader: config.MetricReader{
							Type: "unknown",
						},
					},
					Log: config.Log{
						Processor: config.LogProcessor{
							Type: config.BatchLogProcessorType,
						},
					},
				},
				Assert: func(t *testing.T, err error) {
					var uerr UnknownMetricReaderTypeError
					require.ErrorAs(t, err, &uerr)
					require.Equal(t, config.MetricReaderType("unknown"), uerr.Type)
					require.NotEmpty(t, uerr.Error())
				},
			},
			{
				Name: "if a unknown otlp conn type is configured for log exporting",
				Config: config.OTel{
					Trace: config.Trace{
						Processor: config.SpanProcessor{
							Type: config.BatchSpanProcessorType,
						},
					},
					Metric: config.Metric{
						Reader: config.MetricReader{
							Type: config.PeriodicReaderType,
						},
					},
					Log: config.Log{
						Exporter: config.LogExporter{
							Type: config.OTLPLogExporterType,
							OTLP: config.OTLP{
								Type: "unknown",
							},
						},
					},
				},
				Assert: func(t *testing.T, err error) {
					var uerr UnknownOTLPConnTypeError
					require.ErrorAs(t, err, &uerr)
					require.Equal(t, config.OTLPConnType("unknown"), uerr.Type)
					require.NotEmpty(t, uerr.Error())
				},
			},
			{
				Name: "if a unknown log processor type is configured",
				Config: config.OTel{
					Trace: config.Trace{
						Processor: config.SpanProcessor{
							Type: config.BatchSpanProcessorType,
						},
					},
					Metric: config.Metric{
						Reader: config.MetricReader{
							Type: config.PeriodicReaderType,
						},
					},
					Log: config.Log{
						Processor: config.LogProcessor{
							Type: "unknown",
						},
					},
				},
				Assert: func(t *testing.T, err error) {
					var uerr UnknownLogProcessorTypeError
					require.ErrorAs(t, err, &uerr)
					require.Equal(t, config.LogProcessorType("unknown"), uerr.Type)
					require.NotEmpty(t, uerr.Error())
				},
			},
		}

		for _, testCase := range testCases {
			t.Run(testCase.Name, func(t *testing.T) {
				t.Parallel()

				err := Initialize(t.Context(), testCase.Config)
				testCase.Assert(t, err)
			})
		}
	})
}
