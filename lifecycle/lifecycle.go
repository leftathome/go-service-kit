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
// Run blocks until the sequence is complete. It starts the servers; do not
// call ListenAndServe yourself.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
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
)

// Sentinel errors, so a caller can tell an orderly shutdown that hit a budget
// from one that failed outright.
var (
	// ErrDrainTimeout means in-flight requests did not finish within
	// DrainTimeout and their connections were closed.
	ErrDrainTimeout = errors.New("lifecycle: drain timed out with requests in flight")

	// ErrWorkerStopTimeout means a FinishCurrentCycle worker did not return
	// within WorkerStopTimeout.
	ErrWorkerStopTimeout = errors.New("lifecycle: worker did not stop within its budget")
)

// Worker is a background loop the process owns: an ingest poller, a
// reconciler, a cache warmer.
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
	// Returning a non-nil error is a CRASH: it initiates the whole shutdown
	// sequence and [Run] returns that error. A worker whose loop has died
	// while the service keeps reporting ready is worse than a restart.
	//
	// Returning nil means the job is finished. The process carries on.
	//
	// A panic is recovered, reported as an error, and treated as a crash, so
	// that a worker bug still gets an orderly drain and flush.
	Run func(ctx context.Context) error

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
	// readiness flip. Zero means DefaultPropagationDelay; negative disables
	// the wait, which is only correct for a process that is not behind a
	// Service.
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
			err := safeRun(wctx, w.Run)
			if err == nil {
				return
			}
			// Cancellation is how shutdown ASKS a worker to stop. Reporting it
			// as a failure would make every clean shutdown return an error.
			if wctx.Err() != nil && errors.Is(err, context.Canceled) {
				return
			}
			log.Error("lifecycle: worker failed",
				slog.String("worker", w.Name), slog.String("error", err.Error()))
			fire(fmt.Errorf("lifecycle: worker %s: %w", w.Name, err))
		}()
	}
	return handles
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
