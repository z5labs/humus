// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package job_test

import (
	"context"
	"fmt"
	"io"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

// exporter is the end user's job. Its Run method is the run.
type exporter struct {
	bucket string
	out    io.Writer
}

// Run does the job's work once, and returns early with ctx's error when ctx
// is done, so that an interrupted run exits 1.
func (e *exporter) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(e.out, "exported to", e.bucket)
	return err
}

// Init is the end user's Init: it builds the run's dependencies and returns
// the job's Service.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	bucket, err := config.Env(s.Process.Environment, "EXPORT_BUCKET").Read(ctx)
	if err != nil {
		return nil, err // exit 78
	}
	e := &exporter{bucket: bucket, out: s.Process.Stdout}
	return []humus.Service{job.NewService(s, e.Run)}, nil
}

// buildTelemetry is the job's telemetry.Builder, which names the service.
func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "exporter"
	return telemetry.New(ctx, s, dial, p.Stdout)
}

// A job's main is one call to humus.Main, whose defaults are a job's, with
// an Init that returns the job's Service. This example has no Output
// comment, so it is compiled but never run: running it would exit the test
// binary.
func Example_main() {
	humus.Main(Init, humus.WithTelemetry(buildTelemetry))
}
