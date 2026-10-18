// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpchealth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/z5labs/humus"
	grpcsrv "github.com/z5labs/humus/grpc/internal/server"
	"github.com/z5labs/humus/health"
)

// Service is a humus.Service that serves NewServer's server, and nothing
// else, over plaintext gRPC on a port of its own. It owns the App's
// readiness as a health.Service does, so the end user's Init returns no
// health Service next to it. It is safe for concurrent use.
type Service struct {
	hs     *health.Service
	srv    *grpcsrv.Server
	logger log.Logger
}

// The Service is a humus.Service.
var _ humus.Service = (*Service)(nil)

// NewService checks settings and opts, reporting every problem in one
// error, listens on settings.Addr through s.Host, and registers NewServer's
// server, and nothing else. It registers the listener's close with
// s.OnStop, so a listener whose Service never serves, because Init failed
// after NewService, is still closed.
//
// The server answers from the probes of a health.Service built from opts,
// as httphealth.NewService does:
//
//   - readiness is false until Serve starts, false for good once Serve's
//     context is done or Shutdown is called, and otherwise checks the
//     monitors health.Readiness adds, in the order they were added;
//   - liveness checks the monitors health.Liveness adds, in the order they
//     were added, and is healthy when there are none.
//
// NewService rejects a nil Option.
//
// It is served and shut down as a grpcserver.Service is, without TLS and
// without instrumentation: it serves plain text, records no span and no
// metric, and extracts no trace context. A panic of a monitor fails only
// its RPC, with codes.Internal, and is logged as one ERROR record under
// this package's import path.
func NewService(s *humus.Setup, settings Settings, opts ...health.Option) (*Service, error) {
	if err := checkSettings(settings, opts); err != nil {
		return nil, fmt.Errorf("grpchealth: NewService: %w", err)
	}

	hs := health.NewService(opts...)

	// No credentials and no stats handler: the server speaks plain text,
	// and a probe records no span and no metric. A recovered panic is
	// logged under this package's scope.
	srv, err := grpcsrv.New(s, loggerName, settings.Addr)
	if err != nil {
		return nil, fmt.Errorf("grpchealth: NewService: %w", err)
	}
	if err := srv.RegisterService(&healthpb.Health_ServiceDesc, NewServer(s, hs.Probes())); err != nil {
		// A new server has nothing registered and is not serving, so this
		// never happens.
		return nil, fmt.Errorf("grpchealth: NewService: %w", err)
	}
	return &Service{hs: hs, srv: srv, logger: s.Telemetry.LoggerProvider.Logger(loggerName)}, nil
}

// checkSettings runs every check on settings, and rejects a nil Option,
// naming its index. It reports every failure in one error, each naming its
// field.
func checkSettings(settings Settings, opts []health.Option) error {
	var errs []error
	if err := CheckAddr(settings.Addr); err != nil {
		errs = append(errs, fmt.Errorf("Settings.Addr %w", err))
	}
	for i, opt := range opts {
		if opt == nil {
			errs = append(errs, fmt.Errorf("opts[%d] must not be nil", i))
		}
	}
	return errors.Join(errs...)
}

// Addr returns the address the Service listens on, with the port the Host
// picked when settings.Addr has port 0.
func (svc *Service) Addr() net.Addr {
	return svc.srv.Addr()
}

// Serve logs "listening" at INFO, with the address as server.address,
// marks the Service ready, and serves until Shutdown. It keeps serving
// after ctx is done, so the probes are answered, readiness NOT_SERVING,
// through the App's drain. It returns nil once Shutdown has stopped it,
// including a Shutdown called before Serve.
func (svc *Service) Serve(ctx context.Context) error {
	var rec log.Record
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(log.SeverityInfo)
	rec.SetSeverityText(log.SeverityInfo.String())
	rec.SetBody(attribute.StringValue("listening"))
	rec.AddAttributes(attribute.String("server.address", svc.Addr().String()))
	svc.logger.Emit(context.Background(), rec)

	// The health Service runs with Serve's context, so readiness turns false
	// for good once ctx is done, while the server goes on answering.
	readiness := make(chan struct{})
	go func() {
		defer close(readiness)
		_ = svc.hs.Serve(ctx) // it always returns nil
	}()

	err := svc.srv.Serve(ctx)

	// The server has stopped, after Shutdown or on an error of its own: the
	// Service is not ready for good either way, and the health Service's
	// Serve returns before this one does.
	svc.stopReadiness()
	<-readiness
	if err != nil {
		return fmt.Errorf("grpchealth: %w", err)
	}
	return nil
}

// Shutdown marks the Service not ready for good, then ends every stream, a
// Watch included, with codes.Unavailable, and stops the server gracefully:
// it stops accepting connections, sends every client a GOAWAY, and waits
// for the RPCs in flight. If ctx is done first, or is done already, it
// closes every connection, cancels every RPC, waits for every handler to
// return, and returns an error that wraps ctx's error. Calling it again
// returns nil.
func (svc *Service) Shutdown(ctx context.Context) error {
	svc.stopReadiness()
	if err := svc.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("grpchealth: %w", err)
	}
	return nil
}

// stopReadiness marks the Service not ready for good, and stops its health
// Service's Serve, if it is running.
func (svc *Service) stopReadiness() {
	// The health Service's Serve returns as soon as it is stopped, so its
	// Shutdown never waits for long, and with a context that is never done
	// it returns no error.
	_ = svc.hs.Shutdown(context.Background())
}
