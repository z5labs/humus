// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpchealth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
)

// loggerName is the name of the logger the server logs through.
const loggerName = "github.com/z5labs/humus/grpc/grpchealth"

// pollInterval is how often a Watch's poller checks its monitor, and how
// long each of its checks may take.
const pollInterval = time.Second

// The names of the two probes, which are also the service names that
// answer them, and the values of a logged error's health.probe attribute.
const (
	liveness  = "liveness"
	readiness = "readiness"
)

// NewServer returns a gRPC health v1 server that answers from p:
// the service names "" and "readiness" from p.Readiness, and "liveness"
// from p.Liveness. Register it on any grpc.ServiceRegistrar, such as a
// *grpcserver.Service:
//
//	healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, hs.Probes()))
//
// It panics if p.Liveness or p.Readiness is nil.
func NewServer(s *humus.Setup, p health.Probes) healthpb.HealthServer {
	if p.Liveness == nil {
		panic("grpchealth: NewServer called with a nil Probes.Liveness")
	}
	if p.Readiness == nil {
		panic("grpchealth: NewServer called with a nil Probes.Readiness")
	}
	live := probe{name: liveness, monitor: p.Liveness}
	ready := probe{name: readiness, monitor: p.Readiness}
	return &server{
		logger: s.Telemetry.LoggerProvider.Logger(loggerName),
		probes: map[string]probe{
			"":        ready,
			readiness: ready,
			liveness:  live,
		},
		pollers: make(map[string]*poller),
	}
}

// probe is one of the two probes: its name, and the monitor that answers
// it.
type probe struct {
	name    string
	monitor health.Monitor
}

// server implements gRPC health v1 from an App's probes.
type server struct {
	healthpb.UnimplementedHealthServer

	logger log.Logger
	probes map[string]probe

	// mu guards pollers, which holds the poller of each name that has
	// watchers, and nothing else.
	mu      sync.Mutex
	pollers map[string]*poller
}

// check checks pr's monitor once, with ctx, logs its error, if it returned
// one, and returns the status it answers.
func (srv *server) check(ctx context.Context, pr probe) healthpb.HealthCheckResponse_ServingStatus {
	healthy, err := pr.monitor.Healthy(ctx)
	if err != nil {
		srv.logError(ctx, pr.name, err)
	}
	return servingStatus(healthy, err)
}

// servingStatus is the status a monitor's result answers.
func servingStatus(healthy bool, err error) healthpb.HealthCheckResponse_ServingStatus {
	if !healthy || err != nil {
		return healthpb.HealthCheckResponse_NOT_SERVING
	}
	return healthpb.HealthCheckResponse_SERVING
}

// logError emits err as one ERROR record, in ctx, whose attribute
// health.probe is name.
func (srv *server) logError(ctx context.Context, name string, err error) {
	var rec log.Record
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(log.SeverityError)
	rec.SetSeverityText(log.SeverityError.String())
	rec.SetBody(attribute.StringValue(err.Error()))
	rec.AddAttributes(attribute.String("health.probe", name))
	srv.logger.Emit(ctx, rec)
}

// Check checks the monitor of req's service once, with the RPC's context,
// and adds no timeout of its own. Any name but the three fails with
// codes.NotFound.
func (srv *server) Check(ctx context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	pr, ok := srv.probes[req.GetService()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "unknown service %q", req.GetService())
	}
	return &healthpb.HealthCheckResponse{Status: srv.check(ctx, pr)}, nil
}

// List answers the three names with their current status. It checks each
// monitor once, with the RPC's context, so "" and "readiness" always
// answer the same.
func (srv *server) List(ctx context.Context, _ *healthpb.HealthListRequest) (*healthpb.HealthListResponse, error) {
	live := srv.check(ctx, srv.probes[liveness])
	ready := srv.check(ctx, srv.probes[readiness])
	return &healthpb.HealthListResponse{Statuses: map[string]*healthpb.HealthCheckResponse{
		"":        {Status: ready},
		readiness: {Status: ready},
		liveness:  {Status: live},
	}}, nil
}

// Watch sends the status of req's service as soon as its poller knows it,
// and then each change of it, until the stream's context is done. An
// unknown name gets one SERVICE_UNKNOWN, and nothing else.
func (srv *server) Watch(req *healthpb.HealthCheckRequest, stream grpc.ServerStreamingServer[healthpb.HealthCheckResponse]) error {
	ctx := stream.Context()
	name := req.GetService()
	pr, ok := srv.probes[name]
	if !ok {
		if err := stream.Send(&healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVICE_UNKNOWN}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}

	w := srv.watch(name, pr)
	defer srv.unwatch(name, w)

	// The poller sends only changes, but a watcher that falls behind sees
	// only the latest of them, which can be the status it last sent.
	var last healthpb.HealthCheckResponse_ServingStatus
	sent := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case st := <-w.statuses:
			if sent && st == last {
				continue
			}
			if err := stream.Send(&healthpb.HealthCheckResponse{Status: st}); err != nil {
				return err
			}
			last, sent = st, true
		}
	}
}

// watch adds a watcher to name's poller, starting the poller if name has
// no other watcher. The watcher is sent the poller's status at once, if
// the poller knows it already.
func (srv *server) watch(name string, pr probe) *watcher {
	srv.mu.Lock()
	defer srv.mu.Unlock()

	p, ok := srv.pollers[name]
	if !ok {
		p = newPoller(srv, pr)
		srv.pollers[name] = p
	}
	return p.add()
}

// unwatch removes w from name's poller, and stops the poller once it has
// no watcher left.
func (srv *server) unwatch(name string, w *watcher) {
	srv.mu.Lock()
	defer srv.mu.Unlock()

	p := srv.pollers[name]
	if p.remove(w) {
		delete(srv.pollers, name)
	}
}

// watcher is one Watch of a name. statuses holds the latest status its
// poller sent it that Watch has not yet received.
type watcher struct {
	statuses chan healthpb.HealthCheckResponse_ServingStatus
}

// set replaces any status w has not yet received with st. Only a holder of
// the poller's lock calls it, so the send never blocks.
func (w *watcher) set(st healthpb.HealthCheckResponse_ServingStatus) {
	select {
	case <-w.statuses:
	default:
	}
	w.statuses <- st
}

// poller checks one name's monitor once a second while the name has
// watchers, and sends each change of its status to every one of them.
type poller struct {
	srv  *server
	pr   probe
	stop context.CancelFunc

	// mu guards the fields below it.
	mu       sync.Mutex
	watchers map[*watcher]struct{}
	known    bool
	status   healthpb.HealthCheckResponse_ServingStatus
}

// newPoller starts a poller of pr, with no watcher yet.
func newPoller(srv *server, pr probe) *poller {
	// The poller outlives the Watch that started it, so its checks are
	// done with a context of its own, which stop cancels.
	ctx, stop := context.WithCancel(context.Background())
	p := &poller{
		srv:      srv,
		pr:       pr,
		stop:     stop,
		watchers: make(map[*watcher]struct{}),
	}
	go p.run(ctx)
	return p
}

// add adds a watcher, and sends it the poller's status if it is known.
func (p *poller) add() *watcher {
	p.mu.Lock()
	defer p.mu.Unlock()

	w := &watcher{statuses: make(chan healthpb.HealthCheckResponse_ServingStatus, 1)}
	p.watchers[w] = struct{}{}
	if p.known {
		w.set(p.status)
	}
	return w
}

// remove removes w, and stops the poller if it was the last watcher. It
// reports whether it stopped the poller.
func (p *poller) remove(w *watcher) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.watchers, w)
	if len(p.watchers) > 0 {
		return false
	}
	p.stop()
	return true
}

// run checks the monitor at once and then once a second, until ctx is
// done.
func (p *poller) run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		p.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// poll checks the monitor once, with a context that is done after
// pollInterval, and sends the status to every watcher if it changed. A
// check that runs out of time, or panics, is NOT_SERVING. A failing
// monitor's error is logged only when the status changes.
func (p *poller) poll(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, pollInterval)
	healthy, err := check(checkCtx, p.pr.monitor)
	if err == nil && checkCtx.Err() != nil {
		healthy, err = false, checkCtx.Err()
	}
	cancel()
	if ctx.Err() != nil {
		return // the poller was stopped during the check
	}
	st := servingStatus(healthy, err)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.known && st == p.status {
		return
	}
	p.known, p.status = true, st
	if err != nil {
		p.srv.logError(ctx, p.pr.name, err)
	}
	for w := range p.watchers {
		w.set(st)
	}
}

// check checks m once with ctx. A panic of m is returned as an error: the
// poller's goroutine is no RPC's, so no server recovers it, and a monitor
// that panics there would otherwise end the process.
func check(ctx context.Context, m health.Monitor) (healthy bool, err error) {
	defer func() {
		if v := recover(); v != nil {
			healthy, err = false, fmt.Errorf("grpchealth: the monitor panicked: %v", v)
		}
	}()
	return m.Healthy(ctx)
}
