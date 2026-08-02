// Package lifecycle owns process startup ordering, signal handling, and
// graceful shutdown.
//
// # The problem
//
// When Kubernetes deletes a pod, two things happen CONCURRENTLY and in no
// guaranteed order: the kubelet sends SIGTERM to the container, and the
// endpoint controller starts removing the pod from its Service's endpoints.
// The second is eventually consistent across every kube-proxy, every Ingress
// controller, and every client-side load balancer in the cluster. It takes
// hundreds of milliseconds to several seconds.
//
// A process that exits promptly on SIGTERM therefore stops answering while
// traffic is still being routed to it. The symptom is a burst of
// connection-refused errors on every single deploy, blamed on the network for
// months.
//
// The standard fix is a preStop hook that sleeps before SIGTERM is delivered:
//
//	lifecycle: { preStop: { exec: { command: ["sleep", "5"] } } }
//
// That fix is IMPOSSIBLE here. The deployment image is distroless static:
// there is no /bin/sh, no sleep, no coreutils. exec-based preStop hooks cannot
// run at all. So the delay has to live in the process, which is what this
// package is.
//
// # The sequence
//
// On SIGTERM (or SIGINT, or parent context cancellation, or a worker failure):
//
//  1. Readiness.SetShuttingDown() fires IMMEDIATELY. /readyz starts answering
//     503, which is what tells the endpoint controller to remove this pod.
//     Worker contexts are cancelled at the same instant, so nothing picks up a
//     new unit of background work.
//
//  2. The listeners KEEP SERVING for PropagationDelay (default 4s). This is
//     the window in which endpoint removal propagates. Requests arriving here
//     are real requests from clients that have not yet been told to stop, and
//     they are answered normally. Shutting down at step 1 instead is the bug
//     this whole package exists to prevent.
//
//  3. http.Server.Shutdown drains in-flight requests on every listener in
//     parallel, bounded by DrainTimeout. Connections still open when the
//     budget expires are closed outright -- a request that will not finish
//     must not hold the pod until SIGKILL.
//
//  4. Workers that declared FinishCurrentCycle are waited for, bounded by
//     WorkerStopTimeout.
//
//  5. Flush runs LAST, after the drain, bounded by FlushTimeout, on a context
//     that is NOT cancelled even though the parent almost certainly is. Wire
//     obs.Providers.Shutdown here. Flushing before the drain silently drops
//     the final requests' spans and metrics on every deploy -- which is
//     exactly the telemetry you go looking for when a deploy goes wrong.
//
// Steps 3 through 5 run even when shutdown was triggered by a failure, and
// step 5 runs even when step 3 timed out.
//
// # The propagation delay is pure downtime under replicas: 1 + Recreate
//
// Step 2 buys time for OTHER endpoints to take the traffic this pod is being
// removed from. That premise fails in one common topology, and the package
// used to describe the delay thoroughly without ever naming it.
//
// With replicas: 1 and strategy: Recreate there is no other replica. The old
// pod is fully gone before the new one is created, so during the delay nothing
// else is serving: every second is added to the deploy outage and no client is
// spared a connection-refused, because there is nowhere else to route them.
// The delay costs 4 seconds per deploy and buys nothing.
//
// Set [Spec.PropagationDelay] to [NoPropagationDelay] for that topology, and
// say why in a comment where it is set. THE DEFAULT DOES NOT CHANGE: it is
// correct for the multi-replica case, which is the case a service should be in,
// and a service that later scales past one replica must put the delay back.
//
// Do not reach for this for a rolling update. With replicas > 1 and
// RollingUpdate the delay is exactly what stops the connection-refused burst,
// and removing it reintroduces the bug this package exists to prevent.
//
// # terminationGracePeriodSeconds
//
// The kubelet sends SIGKILL terminationGracePeriodSeconds after SIGTERM. If
// that is shorter than this sequence, the process is killed mid-drain and
// every guarantee above is decoration. The chart MUST derive it from
// [Spec.TerminationGracePeriodSeconds], which sums PropagationDelay,
// DrainTimeout, WorkerStopTimeout, and FlushTimeout, plus [GracePeriodMargin].
//
// At the defaults that is 44 seconds -- comfortably above Kubernetes' own
// default of 30, so a chart that leaves the default in place is already wrong.
// That is why the number is exported rather than merely described.
//
// # Composition
//
//	lifecycle.Run(ctx, lifecycle.Spec{
//	    Servers:   []*http.Server{api.Server, admin.Server},
//	    Readiness: ready,          // httpapi.NewReadiness()
//	    Flush:     obsp.Shutdown,  // obs.Providers.Shutdown, runs LAST
//	    Workers:   []lifecycle.Worker{{Name: "ingest", Run: ingest.Loop}},
//	})
//
// A service with N independent background loops -- one per source, one per
// tenant -- wants per-loop failure isolation rather than a process crash:
//
//	Workers: []lifecycle.Worker{{
//	    Name:      "poll-ebay",
//	    Run:       ebay.Loop,
//	    OnFailure: lifecycle.RestartWithBackoff,
//	}}
//
// Run blocks until the sequence is complete. It starts the servers; do not
// call ListenAndServe yourself.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Sequence defaults. Every one is a Spec field with this as its zero-value
// fallback.
const (
	// DefaultPropagationDelay is how long the listeners keep serving after the
	// readiness flip. Four seconds covers endpoint propagation to kube-proxy
	// on every node plus an Ingress controller's own refresh, with margin. Too
	// short reintroduces the connection-refused burst; too long just makes
	// deploys slower.
	DefaultPropagationDelay = 4 * time.Second

	// DefaultDrainTimeout bounds the in-flight request drain.
	DefaultDrainTimeout = 20 * time.Second

	// DefaultFlushTimeout bounds the final telemetry flush. It matches the
	// budget obs.Providers.Shutdown applies internally.
	DefaultFlushTimeout = 5 * time.Second

	// DefaultWorkerStopTimeout bounds the wait for finish-current-cycle
	// workers.
	DefaultWorkerStopTimeout = 10 * time.Second

	// GracePeriodMargin is added to the sum of the phases when deriving
	// terminationGracePeriodSeconds. It absorbs scheduling jitter, a slow
	// container runtime, and the fact that the phases are budgets rather than
	// guarantees.
	GracePeriodMargin = 5 * time.Second

	// NoPropagationDelay disables step 2 entirely. Assign it to
	// [Spec.PropagationDelay] rather than writing a bare negative duration, so
	// the intent is greppable and reviewable.
	//
	// It is correct in exactly two situations: a process that is not behind a
	// Service at all, and a Deployment with replicas: 1 and strategy: Recreate,
	// where there is no other pod to receive the traffic and the wait is pure
	// added downtime. See "The propagation delay is pure downtime under
	// replicas: 1 + Recreate" in the package doc. It is WRONG for anything
	// behind a Service with more than one replica.
	NoPropagationDelay = -1 * time.Nanosecond
)

// Worker restart defaults, used when [Worker.OnFailure] is
// [RestartWithBackoff]. Each is a [Backoff] field with this as its zero-value
// fallback.
const (
	// DefaultRestartInitialDelay is the wait before the first restart.
	DefaultRestartInitialDelay = 1 * time.Second

	// DefaultRestartMaxDelay caps the exponential growth. A minute is short
	// enough that a source which recovers is polled again promptly, and long
	// enough that a source which is down for hours is not hammered.
	DefaultRestartMaxDelay = 1 * time.Minute

	// DefaultRestartFactor is the multiplier applied after each failure.
	DefaultRestartFactor = 2.0

	// DefaultRestartJitter is the fraction of the computed delay applied as
	// random spread, in each direction. Six pollers that all fail the instant a
	// shared database dies would otherwise retry in lockstep forever.
	DefaultRestartJitter = 0.2

	// DefaultRestartResetAfter is how long a worker must run without failing
	// before its backoff is considered recovered and resets to the initial
	// delay. Without it, a worker that fails once an hour eventually waits the
	// maximum delay for a fault that is not actually escalating.
	DefaultRestartResetAfter = 5 * time.Minute
)

// WorkerRestartsMetricName is the counter [Run] records for every worker
// restart, as the name appears on /metrics after the OTel Prometheus
// exporter's UnderscoreEscapingWithSuffixes translation that obs.Setup pins.
// The instrument itself is [WorkerRestartsInstrumentName].
//
// It carries one attribute, "worker", which is [Worker.Name] and is therefore
// bounded by the service's own wiring. Alert on it: the whole point of the
// restart stance is that "one of my six pollers keeps dying" must be visible,
// since it is by construction neither a process crash nor a silence.
//
//	rate(lifecycle_worker_restarts_total[15m]) > 0
const (
	WorkerRestartsInstrumentName = "lifecycle.worker.restarts"
	WorkerRestartsMetricName     = "lifecycle_worker_restarts_total"
)

// ScopeName is the instrumentation scope for this package's own telemetry. It
// appears as otel_scope_name on every series above. It is declared here rather
// than imported from obs: the kit's packages have no edges between siblings.
const ScopeName = "github.com/leftathome/go-service-kit/lifecycle"

// Sentinel errors, so a caller can tell an orderly shutdown that hit a budget
// from one that failed outright.
var (
	// ErrDrainTimeout means in-flight requests did not finish within
	// DrainTimeout and their connections were closed.
	ErrDrainTimeout = errors.New("lifecycle: drain timed out with requests in flight")

	// ErrWorkerStopTimeout means a FinishCurrentCycle worker did not return
	// within WorkerStopTimeout.
	ErrWorkerStopTimeout = errors.New("lifecycle: worker did not stop within its budget")

	// ErrWorkerRestartsExhausted means a RestartWithBackoff worker exceeded
	// Backoff.MaxRestarts and its failure was escalated to a process shutdown.
	ErrWorkerRestartsExhausted = errors.New("lifecycle: worker exhausted its restart budget")
)

// RestartPolicy selects what happens when a [Worker.Run] returns a non-nil
// error or panics.
type RestartPolicy int

const (
	// CrashProcess is the DEFAULT (the zero value). The failure triggers the
	// whole shutdown sequence and [Run] returns the error. A worker whose loop
	// has died while the service keeps reporting ready is worse than a restart,
	// so for a service whose background work IS the service, crashing is right:
	// the pod restarts, the alert fires on the restart count, and the failure
	// is impossible to miss.
	CrashProcess RestartPolicy = iota

	// RestartWithBackoff restarts the worker after an exponentially increasing
	// delay instead of taking the process down.
	//
	// This is the stance for N INDEPENDENT background loops, which is what the
	// other two stances do not cover. nagus polls six connectors -- eBay,
	// several Shopify storefronts, a land source -- on independent intervals,
	// and its whole design is per-source failure isolation: one storefront
	// returning 429 for an hour must not stop the other five and must not take
	// down the read surface. With only CrashProcess available, such a service
	// is forced to swallow every error inside its own loop and never return
	// one, which turns the kit's "a dead loop is worse than a restart"
	// guarantee into a no-op and makes a permanently wedged poller invisible.
	//
	// Restarts are LOUD, which is the difference between this and swallowing:
	// each one logs at ERROR with the worker name, the error, the consecutive
	// failure count and the next delay, and increments
	// [WorkerRestartsMetricName]. Alert on the counter.
	//
	// Set [Backoff.MaxRestarts] to escalate to a process crash after N
	// consecutive failures, for a worker that the service genuinely cannot run
	// without.
	RestartWithBackoff
)

// String makes the policy readable in a log line.
func (p RestartPolicy) String() string {
	switch p {
	case CrashProcess:
		return "crash_process"
	case RestartWithBackoff:
		return "restart_with_backoff"
	default:
		return "unknown"
	}
}

// Backoff is the restart schedule for a [RestartWithBackoff] worker. The zero
// value is valid and means the documented defaults.
type Backoff struct {
	// Initial is the wait before the first restart. Zero means
	// [DefaultRestartInitialDelay]; negative is treated as zero, i.e. restart
	// immediately, which is almost never what you want for a failing
	// dependency.
	Initial time.Duration

	// Max caps the delay. Zero means [DefaultRestartMaxDelay].
	Max time.Duration

	// Factor multiplies the delay after each consecutive failure. Zero means
	// [DefaultRestartFactor]. A value below 1 is raised to 1, which yields a
	// constant delay rather than a shrinking one.
	Factor float64

	// Jitter spreads each delay by this fraction in each direction, so that
	// workers which failed together do not retry together. Zero means
	// [DefaultRestartJitter]; NEGATIVE means no jitter at all, which is the
	// spelling a test uses when it needs an exact schedule. Values above 1 are
	// clamped to 1.
	Jitter float64

	// ResetAfter is how long a restarted worker must run before the delay
	// sequence resets to Initial. Zero means [DefaultRestartResetAfter];
	// negative disables the reset, so the delay only ever grows.
	ResetAfter time.Duration

	// MaxRestarts escalates to a process crash after this many CONSECUTIVE
	// failures (consecutive in the ResetAfter sense above). Zero, the default,
	// means restart forever -- the right answer for one poller among many.
	// Set it for a worker the service cannot usefully run without.
	MaxRestarts int
}

// WithDefaults returns a copy with every zero-valued field filled in and every
// out-of-range field clamped.
func (b Backoff) WithDefaults() Backoff {
	if b.Initial == 0 {
		b.Initial = DefaultRestartInitialDelay
	}
	if b.Initial < 0 {
		b.Initial = 0
	}
	if b.Max == 0 {
		b.Max = DefaultRestartMaxDelay
	}
	if b.Max < b.Initial {
		b.Max = b.Initial
	}
	if b.Factor == 0 {
		b.Factor = DefaultRestartFactor
	}
	if b.Factor < 1 {
		b.Factor = 1
	}
	if b.Jitter == 0 {
		b.Jitter = DefaultRestartJitter
	}
	if b.Jitter < 0 {
		b.Jitter = 0
	}
	if b.Jitter > 1 {
		b.Jitter = 1
	}
	if b.ResetAfter == 0 {
		b.ResetAfter = DefaultRestartResetAfter
	}
	if b.ResetAfter < 0 {
		b.ResetAfter = 0
	}
	return b
}

// delay returns the wait before restart number n (1-based), with jitter
// applied. It is deterministic when Jitter resolves to 0.
func (b Backoff) delay(n int) time.Duration {
	d := float64(b.Initial)
	for range n - 1 {
		d *= b.Factor
		if d >= float64(b.Max) {
			d = float64(b.Max)
			break
		}
	}
	if d > float64(b.Max) {
		d = float64(b.Max)
	}
	if b.Jitter > 0 && d > 0 {
		// Uniform in [d*(1-j), d*(1+j)], then re-capped: a jittered delay must
		// not exceed Max, or the cap is not a cap.
		spread := d * b.Jitter
		d += spread * (2*rand.Float64() - 1) //nolint:gosec // scheduling jitter, not a secret
		if d > float64(b.Max) {
			d = float64(b.Max)
		}
		if d < 0 {
			d = 0
		}
	}
	return time.Duration(d)
}

// Worker is a background loop the process owns: an ingest poller, a
// reconciler, a cache warmer.
//
// # Two independent stances
//
// A Worker declares two things, and they answer different questions:
//
//   - [Worker.OnFailure] -- what happens when Run FAILS while the process is
//     healthy: take the process down ([CrashProcess], the default), or restart
//     the worker with backoff ([RestartWithBackoff]).
//   - [Worker.FinishCurrentCycle] -- what happens during SHUTDOWN: whether the
//     sequence waits for Run to return.
//
// # The finish-current-cycle vs abort stance
//
// lifecycle cannot force a worker to stop. It controls exactly two things, and
// both are documented here so a service does not have to guess.
//
// WHEN the worker's context is cancelled: at the START of the sequence, the
// same instant readiness flips, before the propagation delay. This is
// deliberate and not configurable. A worker that begins a fresh 30-second
// ingest cycle one second after SIGTERM will be killed halfway through it,
// having done partial work for no reason.
//
// WHETHER the sequence waits for Run to return, and for how long:
//
//   - FinishCurrentCycle false (the DEFAULT, "abort"): the context is
//     cancelled and the worker is not waited for. Correct when the work is
//     idempotent and safely resumable -- a poller that will re-read the same
//     queue on the next pod, a cache warmer. The goroutine dies with the
//     process.
//
//   - FinishCurrentCycle true ("finish"): the context is cancelled and then
//     the sequence WAITS, up to StopTimeout, for Run to return, before the
//     telemetry flush. Correct when an interrupted cycle leaves something
//     inconsistent -- a partially written batch, an un-acked message, a lease
//     that needs releasing. Such a worker must return promptly on ctx.Done()
//     after finishing the unit of work in hand; it must NOT start another.
//
// Either way the context is cancelled at the same moment, so a worker that
// ignores cancellation entirely is bounded only by StopTimeout.
type Worker struct {
	// Name identifies the worker in logs and in error messages.
	Name string

	// Run executes the worker in its own goroutine, started by [Run]. It must
	// return when ctx is done.
	//
	// Returning a non-nil error invokes [Worker.OnFailure]: by default that is
	// a CRASH, initiating the whole shutdown sequence and making [Run] return
	// the error, because a worker whose loop has died while the service keeps
	// reporting ready is worse than a restart. With [RestartWithBackoff] the
	// worker is restarted instead, loudly.
	//
	// Returning nil means the job is finished. The process carries on and the
	// worker is NOT restarted under any policy: nil is "done", not "failed".
	//
	// A panic is recovered and reported as an error, so a worker bug still
	// gets an orderly drain and flush -- or, under RestartWithBackoff, a
	// restart rather than a lost goroutine.
	Run func(ctx context.Context) error

	// OnFailure selects what a non-nil error from Run does. Default
	// [CrashProcess].
	OnFailure RestartPolicy

	// Backoff is the restart schedule. It is read only when OnFailure is
	// [RestartWithBackoff]; the zero value means the documented defaults.
	Backoff Backoff

	// FinishCurrentCycle selects the shutdown stance above. Default false
	// (abort).
	FinishCurrentCycle bool

	// StopTimeout overrides Spec.WorkerStopTimeout for this worker. Only
	// meaningful when FinishCurrentCycle is true.
	StopTimeout time.Duration
}

// Spec describes the process to run. The zero value is not useful; at minimum
// set Servers.
type Spec struct {
	// Servers are started by [Run] and shut down in parallel. Pass
	// httpapi.API.Server and httpapi.Admin.Server. Do not call ListenAndServe
	// on them yourself.
	Servers []*http.Server

	// Listeners, when non-empty, must have the same length as Servers and are
	// used instead of dialing Server.Addr. A nil entry falls back to Addr.
	//
	// This exists for tests (an ephemeral or in-memory listener whose address
	// the test needs to know) and for socket activation. Production wiring
	// leaves it nil.
	Listeners []net.Listener

	// Readiness is flipped to shutting-down FIRST, before anything else. Pass
	// *httpapi.Readiness. Nil is tolerated, but then nothing tells the
	// endpoint controller to stop routing here, which makes the propagation
	// delay pointless.
	Readiness interface{ SetShuttingDown() }

	// PropagationDelay is how long the listeners keep serving after the
	// readiness flip. Zero means [DefaultPropagationDelay].
	//
	// Set it to [NoPropagationDelay] to disable the wait. That is correct in
	// exactly two cases: a process that is not behind a Service, and a
	// Deployment with replicas: 1 and strategy: Recreate, where there is no
	// other pod to receive the traffic being withdrawn and the wait is pure
	// added downtime -- 4 seconds of outage per deploy, buying nothing. See
	// "The propagation delay is pure downtime under replicas: 1 + Recreate" in
	// the package doc, and state the reason in a comment wherever you set it,
	// because it stops being true the moment the service scales to two.
	//
	// Everywhere else, shortening it reintroduces the connection-refused burst
	// this package exists to prevent.
	PropagationDelay time.Duration

	// DrainTimeout bounds the in-flight request drain. Zero means
	// DefaultDrainTimeout.
	DrainTimeout time.Duration

	// FlushTimeout bounds Flush. Zero means DefaultFlushTimeout.
	FlushTimeout time.Duration

	// WorkerStopTimeout bounds the wait for each FinishCurrentCycle worker.
	// Zero means DefaultWorkerStopTimeout.
	WorkerStopTimeout time.Duration

	// Flush runs LAST, after the drain and after the workers, on a context
	// derived with context.WithoutCancel so that an already-cancelled parent
	// does not rob it of its budget. Wire obs.Providers.Shutdown here.
	Flush func(context.Context) error

	// Workers are background loops started by [Run]. See [Worker].
	Workers []Worker

	// Signals initiate shutdown. Nil means SIGTERM and SIGINT.
	//
	// An EMPTY, NON-NIL slice disables signal handling entirely. Tests running
	// inside a testing/synctest bubble must use that: signal delivery comes
	// from a runtime goroutine outside the bubble. Such tests drive shutdown
	// by cancelling the context, which Run treats identically.
	Signals []os.Signal

	// Logger records each phase of the sequence. Nil means slog.Default().
	// The log line at each transition is what turns "the deploy dropped
	// requests" into a five-minute diagnosis.
	Logger *slog.Logger

	// MeterProvider records [WorkerRestartsMetricName]. Pass
	// obs.Providers.MeterProvider. Nil means the OTel global, which obs.Setup
	// installs, so the counter is exported without wiring for any service that
	// calls obs.Setup -- and is a no-op for any service that does not.
	MeterProvider metric.MeterProvider
}

// WithDefaults returns a copy with every zero-valued budget filled in. Run
// calls it; it is exported so a chart or a test can compute the same numbers.
func (s Spec) WithDefaults() Spec {
	if s.PropagationDelay == 0 {
		s.PropagationDelay = DefaultPropagationDelay
	}
	if s.DrainTimeout <= 0 {
		s.DrainTimeout = DefaultDrainTimeout
	}
	if s.FlushTimeout <= 0 {
		s.FlushTimeout = DefaultFlushTimeout
	}
	if s.WorkerStopTimeout <= 0 {
		s.WorkerStopTimeout = DefaultWorkerStopTimeout
	}
	if s.Signals == nil {
		s.Signals = []os.Signal{syscall.SIGTERM, syscall.SIGINT}
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	return s
}

// GracePeriod is the wall-clock upper bound on the whole shutdown sequence,
// plus [GracePeriodMargin].
//
// The pod's terminationGracePeriodSeconds must EXCEED this, or SIGKILL lands
// mid-sequence. See [Spec.TerminationGracePeriodSeconds] for the value to put
// in the chart.
func (s Spec) GracePeriod() time.Duration {
	d := s.WithDefaults()
	total := GracePeriodMargin + d.DrainTimeout + d.FlushTimeout
	if d.PropagationDelay > 0 {
		total += d.PropagationDelay
	}

	// Only a FinishCurrentCycle worker can extend the sequence; an abort
	// worker is not waited for. With no workers configured, budget for the
	// default anyway: "we have no background work yet" is not a promise, and
	// adding the first worker should not silently require a chart change.
	worst := d.WorkerStopTimeout
	if len(d.Workers) > 0 {
		worst = 0
		for _, w := range d.Workers {
			if !w.FinishCurrentCycle {
				continue
			}
			budget := w.StopTimeout
			if budget <= 0 {
				budget = d.WorkerStopTimeout
			}
			if budget > worst {
				worst = budget
			}
		}
	}
	return total + worst
}

// TerminationGracePeriodSeconds is [Spec.GracePeriod] rounded up to whole
// seconds. This is the number the Helm chart must use.
func (s Spec) TerminationGracePeriodSeconds() int {
	d := s.GracePeriod()
	secs := int(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	return secs
}

// Run starts the servers and the workers, waits for a shutdown trigger, and
// executes the shutdown sequence described in the package documentation. It
// blocks until the sequence is complete.
//
// Shutdown is triggered by any of: a signal in Spec.Signals, cancellation of
// ctx, a worker returning a non-nil error or panicking, or a server's Serve
// failing for a reason other than being shut down.
//
// The returned error joins everything that went wrong: the trigger cause, any
// drain or worker-stop timeout, and any flush error. A clean shutdown returns
// nil. A failure to bind a listener is returned immediately, before anything
// is started.
func Run(ctx context.Context, spec Spec) error {
	spec = spec.WithDefaults()
	log := spec.Logger

	if n := len(spec.Listeners); n != 0 && n != len(spec.Servers) {
		return fmt.Errorf("lifecycle: %d listeners for %d servers; they are paired by index", n, len(spec.Servers))
	}

	// Signal registration happens FIRST, before any listener is opened, so
	// that there is no window in which the process is alive and SIGTERM still
	// has its default disposition of killing it outright.
	var sigCh chan os.Signal
	if len(spec.Signals) > 0 {
		sigCh = make(chan os.Signal, 1)
		signal.Notify(sigCh, spec.Signals...)
		defer signal.Stop(sigCh)
	}

	listeners, err := listen(ctx, spec)
	if err != nil {
		return err
	}

	// trigger carries the first thing that asked for a shutdown. Buffered and
	// written non-blockingly, so a second failure never blocks its reporter.
	trigger := make(chan error, 1)
	fire := func(err error) {
		select {
		case trigger <- err:
		default:
		}
	}

	// Worker contexts derive from ctx, so a cancelled parent cancels them with
	// no separate path, and step 1 cancels them on a signal.
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()

	workers := startWorkers(workerCtx, spec, log, fire)
	startServers(spec, listeners, log, fire)

	log.Info("lifecycle: serving",
		slog.Int("servers", len(spec.Servers)),
		slog.Int("workers", len(spec.Workers)),
		slog.Duration("propagation_delay", spec.PropagationDelay),
		slog.Duration("drain_timeout", spec.DrainTimeout),
		slog.Int("termination_grace_period_seconds", spec.TerminationGracePeriodSeconds()),
	)

	var cause error
	select {
	case <-ctx.Done():
		log.Info("lifecycle: shutdown requested", slog.String("trigger", "context"))
	case sig := <-sigCh:
		log.Info("lifecycle: shutdown requested",
			slog.String("trigger", "signal"), slog.String("signal", sig.String()))
	case cause = <-trigger:
		log.Error("lifecycle: shutdown requested",
			slog.String("trigger", "failure"), slog.String("error", cause.Error()))
	}

	return shutdown(ctx, spec, listeners, workers, sigCh, cause)
}

// shutdown runs steps 1 through 5. It is a separate function only so the phase
// order reads top to bottom on one screen.
func shutdown(
	parent context.Context,
	spec Spec,
	listeners []net.Listener,
	workers []workerHandle,
	sigCh <-chan os.Signal,
	cause error,
) error {
	log := spec.Logger
	errs := []error{cause}

	// Every context from here on is detached from the parent. The parent is
	// very often already cancelled -- that is one of the ways we got here --
	// and the shutdown budgets must not be zero because of it.
	base := context.WithoutCancel(parent)

	// STEP 1: readiness 503 and worker cancellation, at the same instant.
	if spec.Readiness != nil {
		spec.Readiness.SetShuttingDown()
	}
	for _, w := range workers {
		w.cancel()
	}
	log.Info("lifecycle: readiness set to shutting down, still serving")

	// STEP 2: keep serving while endpoint removal propagates.
	if spec.PropagationDelay > 0 {
		timer := time.NewTimer(spec.PropagationDelay)
		select {
		case <-timer.C:
		case sig := <-sigCh:
			// A second signal means an operator is in a hurry. Skip the wait
			// rather than making them reach for SIGKILL, which would abandon
			// the drain and the flush entirely.
			timer.Stop()
			log.Warn("lifecycle: second signal, skipping the propagation delay",
				slog.String("signal", sig.String()))
		}
	}

	// STEP 3: drain in-flight requests on every listener, in parallel.
	drainCtx, cancelDrain := context.WithTimeout(base, spec.DrainTimeout)
	defer cancelDrain()

	var (
		wg       sync.WaitGroup
		drainMu  sync.Mutex
		drainErr []error
	)
	for i, srv := range spec.Servers {
		if srv == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := srv.Shutdown(drainCtx)
			if err == nil {
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				// Requests still in flight at the budget. Close outright:
				// holding the pod until SIGKILL would skip the flush too.
				_ = srv.Close()
				err = fmt.Errorf("%w: server %d after %s", ErrDrainTimeout, i, spec.DrainTimeout)
			}
			drainMu.Lock()
			drainErr = append(drainErr, err)
			drainMu.Unlock()
		}()
	}
	wg.Wait()
	errs = append(errs, drainErr...)

	// Listeners no server adopted -- a nil server, or a Serve that failed
	// early -- would otherwise stay bound.
	for _, ln := range listeners {
		if ln != nil {
			_ = ln.Close()
		}
	}
	log.Info("lifecycle: drain complete", slog.Bool("timed_out", len(drainErr) > 0))

	// STEP 4: wait for finish-current-cycle workers.
	errs = append(errs, waitForWorkers(spec, workers)...)

	// STEP 5: flush telemetry, LAST, after everything that could produce any.
	if spec.Flush != nil {
		flushCtx, cancelFlush := context.WithTimeout(base, spec.FlushTimeout)
		if err := spec.Flush(flushCtx); err != nil {
			errs = append(errs, fmt.Errorf("lifecycle: flush: %w", err))
		}
		cancelFlush()
		log.Info("lifecycle: telemetry flushed")
	}

	log.Info("lifecycle: shutdown complete")
	return errors.Join(errs...)
}

// --- workers ------------------------------------------------------------

type workerHandle struct {
	worker Worker
	done   chan struct{}
	cancel context.CancelFunc
}

func startWorkers(ctx context.Context, spec Spec, log *slog.Logger, fire func(error)) []workerHandle {
	restarts := newRestartCounter(spec.MeterProvider)

	handles := make([]workerHandle, 0, len(spec.Workers))
	for _, w := range spec.Workers {
		if w.Run == nil {
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		h := workerHandle{worker: w, done: make(chan struct{}), cancel: cancel}
		handles = append(handles, h)

		go func() {
			defer close(h.done)
			superviseWorker(wctx, w, log, fire, restarts)
		}()
	}
	return handles
}

// superviseWorker runs one worker and applies its failure policy. Under
// [CrashProcess] it runs exactly once, which is the pre-existing behaviour.
func superviseWorker(
	ctx context.Context,
	w Worker,
	log *slog.Logger,
	fire func(error),
	restarts func(context.Context, string),
) {
	backoff := w.Backoff.WithDefaults()
	consecutive := 0

	for {
		started := time.Now()
		err := safeRun(ctx, w.Run)
		if err == nil {
			// Finished, not failed. Nothing restarts a completed job.
			return
		}
		// Cancellation is how shutdown ASKS a worker to stop. Reporting it as a
		// failure would make every clean shutdown return an error, and
		// restarting into an already-cancelled context would spin.
		if ctx.Err() != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			// A different error during shutdown is worth a line, but the
			// process is already going down: do not restart, do not fire.
			log.Warn("lifecycle: worker failed while shutting down",
				slog.String("worker", w.Name), slog.String("error", err.Error()))
			return
		}

		if w.OnFailure != RestartWithBackoff {
			log.Error("lifecycle: worker failed",
				slog.String("worker", w.Name),
				slog.String("policy", w.OnFailure.String()),
				slog.String("error", err.Error()))
			fire(fmt.Errorf("lifecycle: worker %s: %w", w.Name, err))
			return
		}

		// A worker that ran for a good while before failing is recovering, not
		// escalating: start its schedule over.
		if backoff.ResetAfter > 0 && time.Since(started) >= backoff.ResetAfter {
			consecutive = 0
		}
		consecutive++

		if backoff.MaxRestarts > 0 && consecutive > backoff.MaxRestarts {
			log.Error("lifecycle: worker exhausted its restart budget",
				slog.String("worker", w.Name),
				slog.Int("restarts", consecutive-1),
				slog.Int("max_restarts", backoff.MaxRestarts),
				slog.String("error", err.Error()))
			fire(fmt.Errorf("%w: %s after %d restarts: %w",
				ErrWorkerRestartsExhausted, w.Name, backoff.MaxRestarts, err))
			return
		}

		// Counted only once the restart is actually going to happen, so the
		// counter is restarts and not failures; the escalating failure above is
		// a crash, which the pod restart count already records.
		restarts(ctx, w.Name)

		delay := backoff.delay(consecutive)
		// ERROR, not WARN: a worker restarting is not routine. This line and
		// WorkerRestartsMetricName are the entire difference between this
		// stance and a service swallowing the error in its own loop.
		log.Error("lifecycle: worker failed, restarting",
			slog.String("worker", w.Name),
			slog.Int("consecutive_failures", consecutive),
			slog.Duration("delay", delay),
			slog.String("error", err.Error()))

		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
		} else if ctx.Err() != nil {
			return
		}
	}
}

// newRestartCounter returns the increment function for
// [WorkerRestartsMetricName]. Instrument creation failing must not stop the
// process from running its workers, so the fallback is a no-op.
func newRestartCounter(mp metric.MeterProvider) func(context.Context, string) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	counter, err := mp.Meter(ScopeName).Int64Counter(
		WorkerRestartsInstrumentName,
		metric.WithDescription("Background worker restarts, by worker name."),
		metric.WithUnit("{restart}"),
	)
	if err != nil {
		return func(context.Context, string) {}
	}
	return func(ctx context.Context, name string) {
		counter.Add(ctx, 1, metric.WithAttributes(attribute.String("worker", name)))
	}
}

// safeRun turns a worker panic into an error. An unrecovered panic here would
// take the process down without the drain or the flush, which is the one
// outcome worse than the bug that caused it.
func safeRun(ctx context.Context, run func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return run(ctx)
}

func waitForWorkers(spec Spec, handles []workerHandle) []error {
	var errs []error
	for _, h := range handles {
		if !h.worker.FinishCurrentCycle {
			// Abort stance: already cancelled, deliberately not waited for.
			continue
		}
		budget := h.worker.StopTimeout
		if budget <= 0 {
			budget = spec.WorkerStopTimeout
		}
		timer := time.NewTimer(budget)
		select {
		case <-h.done:
			timer.Stop()
		case <-timer.C:
			spec.Logger.Warn("lifecycle: worker did not stop in time",
				slog.String("worker", h.worker.Name), slog.Duration("budget", budget))
			errs = append(errs, fmt.Errorf("%w: %s after %s", ErrWorkerStopTimeout, h.worker.Name, budget))
		}
	}
	return errs
}

// --- servers ------------------------------------------------------------

func listen(ctx context.Context, spec Spec) ([]net.Listener, error) {
	// ListenConfig rather than net.Listen so a context cancelled during
	// startup aborts the bind instead of racing it.
	var lc net.ListenConfig
	listeners := make([]net.Listener, len(spec.Servers))
	for i, srv := range spec.Servers {
		if i < len(spec.Listeners) && spec.Listeners[i] != nil {
			listeners[i] = spec.Listeners[i]
			continue
		}
		if srv == nil {
			continue
		}
		addr := srv.Addr
		if addr == "" {
			addr = ":http"
		}
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			// Close whatever we already opened. A process that reports a
			// startup error but leaves ports held is worse than either
			// outcome on its own.
			for _, opened := range listeners[:i] {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return nil, fmt.Errorf("lifecycle: listen on %s: %w", addr, err)
		}
		listeners[i] = ln
	}
	return listeners, nil
}

func startServers(spec Spec, listeners []net.Listener, log *slog.Logger, fire func(error)) {
	for i, srv := range spec.Servers {
		if srv == nil || listeners[i] == nil {
			continue
		}
		go func() {
			err := srv.Serve(listeners[i])
			if err == nil || errors.Is(err, http.ErrServerClosed) {
				return
			}
			log.Error("lifecycle: server stopped",
				slog.String("addr", srv.Addr), slog.String("error", err.Error()))
			fire(fmt.Errorf("lifecycle: server %s: %w", srv.Addr, err))
		}()
	}
}
