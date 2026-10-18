// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package fault builds a humus App's failure injection, on bedrock's fit.
//
// # The model
//
// Failure injection lets one request fail on purpose, in production or in a
// test, without failing anyone else's. A request carries a failure scenario,
// a fit.Scenario, in its OpenTelemetry baggage. Each injection point the
// request passes through, such as a call to a dependency, looks itself up in
// the scenario and learns which fit.Fault to emulate: a Delay, an abort, or
// both. A request that carries no scenario is never touched.
//
// fit ships that model and leaves its conventions to humus, and humus leaves
// one of them to the end user: the wire format. A fit.Decoder of the end
// user's reads a scenario from a request's baggage and strips it, and the
// edge that writes scenarios and every service that reads them agree on it.
// There is no default Decoder.
//
// # Building the Injector
//
// An App's failure injection is an [Injector], which the App builds before
// Init from the [Settings] that a function of the end user's returns, and
// puts in Setup.Faults. The end user passes that function with
// humus.WithFaults, and sources the Settings as they source any other:
//
//	func main() {
//		humus.Main(app.Init, humus.WithFaults(app.Faults))
//	}
//
//	// Faults is the service's failure injection.
//	func Faults(ctx context.Context, s *humus.Setup) (fault.Settings, error) {
//		d := fault.DefaultSettings()
//		return config.Struct(func(r *config.Resolver) fault.Settings {
//			c := d
//			c.Decoder = codec // the end user's fit.Decoder
//			c.MaxDelay = r.Get(config.Env(s.Process.Environment, "FAULT_MAX_DELAY").
//				Parse(time.ParseDuration).Default(d.MaxDelay).Validate(fault.CheckMaxDelay))
//			return c
//		}).Read(ctx)
//	}
//
// The function is given the Setup, so a Decoder can load what it needs
// through s.Host, such as a registry of scenario IDs from a file, and
// register its release with s.OnStop. The App builds the Injector with
// [New], which checks the Settings. An error from either fails startup,
// as an error from Init does. Without humus.WithFaults, Setup.Faults is the
// zero Injector, which is off: it evaluates and strips nothing, and nothing
// changes.
//
// Every humus server and client takes the Injector from the Setup it is
// built with, so none can miss it. A unit test of the end user's code that
// takes a Setup sets Setup.Faults from New.
//
// # Injection points
//
// Every humus client is an injection point, named after its dependency. The
// end user's own code can be one too, by evaluating a point of its own with
// [Injector.Eval]:
//
//	if err := s.Faults.Eval(ctx, "postgresql:orders").Inject(ctx); err != nil {
//		return err
//	}
//
// Name such a point <system>:<name>, after the protocol's *.system
// attribute, such as postgresql:orders or redis:sessions, as humus's own
// points are named: http:payments:8443 or kafka:orders. A rule can then
// target one dependency, or every dependency of a system with a prefix such
// as postgresql:*, and the names line up with the trace.
//
// [Injector.Strip] removes a scenario from a context, keeping every other
// baggage member, where a request crosses a trust boundary.
//
// # InjectedError
//
// An attempt that a fault ended fails with an [InjectedError] naming its
// point. With Err nil it is an abort, and it matches fit.ErrInjected with
// errors.Is. With Err set it is a Delay that Err, the attempt's context's
// cause, cut short, and it matches Err, such as context.DeadlineExceeded,
// and not fit.ErrInjected. Either way its error.type is
// github.com/z5labs/humus/fault.InjectedError, which [InjectedError.ErrorType]
// returns, so a query finds every injected failure in every client's spans
// and metrics, and tells it from a real one.
//
// # Telemetry
//
// On a request that carries a scenario, every evaluation adds a
// humus.fit.evaluation span event to the span in its context, modelled on
// OpenTelemetry's feature_flag.evaluation event, with these attributes:
//
//	humus.fit.point  always: the point evaluated
//	humus.fit.rule   when a rule matched: the rule's Point
//	humus.fit.delay  when a rule matched: the Delay, clamped to MaxDelay, in seconds
//	humus.fit.abort  when a rule matched: whether the point aborts
//	error.type       when the Decoder rejected the scenario
//	error.message    when the Decoder rejected the scenario
//
// An unmatched evaluation still names its point, which is how the points a
// request passes through are discovered. A scenario the Decoder rejects
// gives the zero Fault, and the request goes on unharmed. A request with no
// scenario adds no event.
//
// The package emits no log record and creates no instrument, and it reads
// and sets no OpenTelemetry global: the event goes on the span already in
// the context, so the Injector needs no provider.
package fault
