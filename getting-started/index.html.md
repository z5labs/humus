# Getting started

The service is a job: it greets the name its environment gives it, once, and
exits. It needs Go 1.27 or later, and nothing else. Its code is
[`example/job/hello`](https://github.com/z5labs/humus/tree/main/example/job/hello).

## 1. Create the module

```sh
mkdir hello && cd hello
go mod init example.com/hello
go get github.com/z5labs/humus@main
```

## 2. Write Init

`Init` builds the service. It reads `HELLO_NAME` from the environment and
returns a job, whose run greets it. Put it in `hello.go`:

```go
package main

import (
	"context"
	"fmt"

	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/job"
)

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	name, err := config.Env(s.Process.Environment, "HELLO_NAME").Read(ctx)
	if err != nil {
		return nil, err
	}

	out := s.Process.Stdout
	greet := func(context.Context) error {
		_, err := fmt.Fprintf(out, "Hello, %s!\n", name)
		return err
	}
	return []humus.Service{job.NewService(s, greet)}, nil
}
```

## 3. Write main

`humus.Main` runs `Init`'s Services until they return, then exits with their
status. Put it in `main.go`:

```go
// Command hello greets the name its environment gives it, once.
package main

import "github.com/z5labs/humus"

func main() {
	humus.Main(Init)
}
```

## 4. Run it

```sh
go mod tidy
HELLO_NAME=Gopher go run .
```

The greeting comes between the App's own log records:

```text
{"timestamp":"2026-10-23T06:04:36.982513033Z","severity_text":"INFO","severity_number":9,"body":"serving","scope":"github.com/z5labs/humus"}
Hello, Gopher!
{"timestamp":"2026-10-23T06:04:36.982631066Z","severity_text":"INFO","severity_number":9,"body":"shutting down","scope":"github.com/z5labs/humus"}
{"timestamp":"2026-10-23T06:04:36.982656447Z","severity_text":"INFO","severity_number":9,"body":"stopped","scope":"github.com/z5labs/humus"}
```

Without `HELLO_NAME`, `Init` fails, and the service exits 78:

```sh
go run .
```

```text
{"timestamp":"2026-10-23T06:04:37.083766967Z","severity_text":"ERROR","severity_number":17,"body":"init: config HELLO_NAME: not found","scope":"github.com/z5labs/humus"}
exit status 78
```

## 5. Test it

`humustest.Start` runs `Init` in the App `humus.Main` would run, on a test
process whose environment the test sets and whose output it reads. Put the
tests in `hello_test.go`:

```go
package main

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/z5labs/humus/humustest"
)

func TestInit_greets_the_name_it_is_given(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, status := humustest.Start(t, Init,
			humustest.WithEnv(map[string]string{"HELLO_NAME": "Gopher"}),
		)

		st := <-status
		if st.Code() != 0 {
			t.Fatalf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if !strings.Contains(p.Stdout(), "Hello, Gopher!\n") {
			t.Errorf("stdout = %q, want it to greet Gopher", p.Stdout())
		}
	})
}

func TestInit_fails_without_a_name(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init)

		st := <-status
		if st.Code() != 78 {
			t.Errorf("code = %d, want 78: %v", st.Code(), st.Cause())
		}
		if !strings.Contains(fmt.Sprint(st.Cause()), "HELLO_NAME") {
			t.Errorf("cause = %v, want it to name HELLO_NAME", st.Cause())
		}
	})
}
```

```sh
go test ./...
```

## Next

[Fundamentals](/humus/fundamentals/) covers what this service
already has, as every humus service does: the App lifecycle, configuration,
telemetry, health and testing. Then [Service types](/humus/services/)
has a section for each kind of service humus runs:
[HTTP](/humus/services/http/),
[gRPC](/humus/services/grpc/),
[jobs](/humus/services/jobs/),
[Kafka](/humus/services/kafka/),
[RabbitMQ](/humus/services/rabbitmq/) and
[Kubernetes controllers](/humus/services/kubernetes/).

Next: [Fundamentals](/humus/fundamentals/)
