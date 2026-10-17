// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest_test

// This file is a whole-App test of a job as an end user writes one, and it
// imports no bedrocktest: TestStart_a_whole_App_test_imports_no_bedrocktest
// checks that it doesn't.

import (
	"bufio"
	"context"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
)

// exporter is the end user's job: it reads rows from stdin and exports each
// one to the bucket its environment names.
type exporter struct {
	setup  *humus.Setup
	bucket string

	mu       sync.Mutex
	exported []string
}

// Serve runs the job once and returns, as a finished job does, which ends
// the App.
func (e *exporter) Serve(ctx context.Context) error {
	_, span := e.setup.Telemetry.TracerProvider.Tracer("exporter").Start(ctx, "job")
	defer span.End()
	rows := bufio.NewScanner(e.setup.Process.Stdin)
	for rows.Scan() {
		e.mu.Lock()
		e.exported = append(e.exported, e.bucket+"/"+rows.Text())
		e.mu.Unlock()
	}
	return rows.Err()
}

func (*exporter) Shutdown(context.Context) error { return nil }

// jobInit is the end user's Init: it reads the bucket from the environment
// and returns the job.
func jobInit(job *exporter) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		bucket, err := s.Process.Environment.Value("EXPORT_BUCKET")
		if err != nil {
			return nil, err
		}
		job.setup = s
		job.bucket = bucket
		return []humus.Service{job}, nil
	}
}

func TestInit_exports_every_row(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := &exporter{}
		host := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		_, status := humustest.Start(t, jobInit(job),
			humustest.WithApp(humus.WithHost(host.Host()), humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{"EXPORT_BUCKET": "nightly"}),
			humustest.WithStdin(strings.NewReader("a\nb\n")),
		)

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, []string{"nightly/a", "nightly/b"}, job.exported)
		require.Equal(t, "job", tel.Spans()[0].Name())
	})
}

func TestInit_fails_without_its_bucket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, status := humustest.Start(t, jobInit(&exporter{}))

		st := <-status
		require.Equal(t, 1, st.Code())
		require.ErrorContains(t, st.Cause(), "EXPORT_BUCKET")
		require.NotEmpty(t, p.Stdout(), "the failure is logged on stdout by Main's telemetry default")
	})
}

func TestStart_a_whole_App_test_imports_no_bedrocktest(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "start_job_test.go", nil, parser.ImportsOnly)
	require.NoError(t, err)
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		require.NoError(t, err)
		require.NotEqual(t, "github.com/z5labs/bedrock/bedrocktest", path)
	}
}
