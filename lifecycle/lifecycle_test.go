package lifecycle_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/leftathome/go-service-kit/httpapi"
	"github.com/leftathome/go-service-kit/lifecycle"
)

// --- in-memory plumbing ------------------------------------------------
//
// testing/synctest needs every goroutine in the bubble to be DURABLY blocked
// before it will advance the fake clock. A goroutine parked in the netpoller
// waiting on a real socket is not durably blocked, so a real 127.0.0.1
// listener inside a bubble stops the clock dead and the test hangs.
//
// net.Pipe is implemented with channels, which ARE durably blocking, so a real
// *http.Server serving real requests over pipe connections runs inside a
// bubble with a fake clock and no sleeping. Everything below is a real server,
// a real client, and a real Shutdown drain -- only the transport and the clock
// are synthetic.

type pipeListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *pipeListener) client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return l.dial(ctx)
		},
		// One connection at a time keeps the drain deterministic.
		DisableKeepAlives: true,
	}}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// --- event log ---------------------------------------------------------

// journal records what happened and WHEN on the fake clock. Ordering
// assertions read from it; the whole point of this package is the order.
type journal struct {
	mu     sync.Mutex
	start  time.Time
	events []event
}

type event struct {
	name string
	at   time.Duration
}

func newJournal() *journal { return &journal{start: time.Now()} }

func (j *journal) record(name string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, event{name: name, at: time.Since(j.start)})
}

func (j *journal) startedAt() time.Time { return j.start }

func (j *journal) names() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, len(j.events))
	for i, e := range j.events {
		out[i] = e.name
	}
	return out
}

func (j *journal) at(name string) (time.Duration, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, e := range j.events {
		if e.name == name {
			return e.at, true
		}
	}
	return 0, false
}

func (j *journal) mustAt(t *testing.T, name string) time.Duration {
	t.Helper()
	d, ok := j.at(name)
	if !ok {
		t.Fatalf("event %q never happened; journal = %v", name, j.names())
	}
	return d
}

func (j *journal) index(name string) int {
	for i, n := range j.names() {
		if n == name {
			return i
		}
	}
	return -1
}

// assertBefore is the ordering primitive. It compares POSITIONS, not
// timestamps, so two events on the same fake-clock instant are still ordered.
func (j *journal) assertBefore(t *testing.T, first, second string) {
	t.Helper()
	i, k := j.index(first), j.index(second)
	if i < 0 {
		t.Fatalf("event %q never happened; journal = %v", first, j.names())
	}
	if k < 0 {
		t.Fatalf("event %q never happened; journal = %v", second, j.names())
	}
	if i >= k {
		t.Errorf("%q must happen before %q; journal = %v", first, second, j.names())
	}
}

// --- harness -----------------------------------------------------------

type harness struct {
	journal *journal
	ready   *httpapi.Readiness
	ln      *pipeListener
	server  *http.Server
	client  *http.Client

	// stuck releases the /stuck handler. synctest panics if a bubbled
	// goroutine is still blocked when the test function returns, so a handler
	// that models "never finishes" has to be releasable at the end.
	stuck chan struct{}
}

// newHarness builds a real http.Server over a pipe listener whose handler
// takes handlerDelay of fake time to answer, plus a Readiness wired the way
// the template wires it.
func newHarness(t *testing.T, handlerDelay time.Duration) *harness {
	t.Helper()

	j := newJournal()
	ln := newPipeListener()
	h := &harness{journal: j, ready: httpapi.NewReadiness(), ln: ln, stuck: make(chan struct{})}

	admin := httpapi.NewAdmin(httpapi.AdminOptions{Readiness: h.ready})

	mux := http.NewServeMux()
	mux.Handle("GET /readyz", admin.Handler())
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		j.record("handler:start")
		time.Sleep(handlerDelay)
		j.record("handler:done")
		_, _ = io.WriteString(w, "served")
	})
	mux.HandleFunc("GET /fast", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "served")
	})
	mux.HandleFunc("GET /stuck", func(w http.ResponseWriter, _ *http.Request) {
		j.record("stuck:start")
		<-h.stuck
		j.record("stuck:done")
		_, _ = io.WriteString(w, "served")
	})

	// Releasing at cleanup keeps a wedged handler from tripping synctest's
	// end-of-bubble deadlock check.
	t.Cleanup(func() { close(h.stuck) })

	h.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	h.client = ln.client()
	return h
}

func (h *harness) spec() lifecycle.Spec {
	return lifecycle.Spec{
		Servers:   []*http.Server{h.server},
		Listeners: []net.Listener{h.ln},
		Readiness: h.ready,
		Logger:    slog.New(slog.DiscardHandler),
		// Signal handling off: signal delivery crosses the synctest bubble
		// boundary. Shutdown is driven by cancelling the context instead,
		// which Run treats identically.
		Signals: []os.Signal{},
	}
}

func (h *harness) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://pipe"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// --- the ordering tests ------------------------------------------------

// The whole sequence, asserted end to end on a fake clock.
func TestShutdownSequenceOrderAndTiming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 2*time.Second)
		j := h.journal

		spec := h.spec()
		spec.PropagationDelay = 4 * time.Second
		spec.DrainTimeout = 10 * time.Second
		spec.Flush = func(context.Context) error {
			j.record("flush")
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()

		synctest.Wait()

		// Serving normally.
		if code, _ := h.get(t, "/readyz"); code != http.StatusOK {
			t.Fatalf("pre-shutdown /readyz = %d, want 200", code)
		}

		start := time.Now()
		j.record("sigterm")
		cancel()
		synctest.Wait()

		// 1. Readiness flips IMMEDIATELY, with no propagation delay first.
		if !h.ready.ShuttingDown() {
			t.Error("readiness was not flipped immediately on shutdown")
		}
		code, body := h.get(t, "/readyz")
		if code != http.StatusServiceUnavailable {
			t.Errorf("/readyz right after signal = %d, want 503", code)
		}
		if !strings.Contains(body, httpapi.StatusShuttingDown) {
			t.Errorf("/readyz body = %q, want it to say shutting down", body)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("readiness flipped %v after the signal, want immediately", elapsed)
		}

		// 2. The server KEEPS SERVING through the propagation window. This is
		//    the whole reason the delay exists: the endpoint controller has
		//    not finished removing this pod yet, so traffic is still arriving.
		time.Sleep(time.Second)
		if code, body := h.get(t, "/fast"); code != http.StatusOK || body != "served" {
			t.Errorf("request 1s after signal: %d %q, want 200 served -- the propagation window is not serving", code, body)
		}
		time.Sleep(2 * time.Second) // t+3s, still inside the 4s window
		if code, _ := h.get(t, "/fast"); code != http.StatusOK {
			t.Errorf("request 3s after signal = %d, want 200", code)
		}

		select {
		case err := <-done:
			t.Fatalf("Run returned during the propagation window: %v", err)
		default:
		}

		// 3. Shutdown drains, then 4. Flush runs LAST.
		var runErr error
		select {
		case runErr = <-done:
		case <-time.After(time.Minute):
			t.Fatal("Run did not return")
		}
		if runErr != nil {
			t.Errorf("Run() = %v, want nil", runErr)
		}

		j.assertBefore(t, "sigterm", "flush")
		if got := j.mustAt(t, "flush"); got < 4*time.Second {
			t.Errorf("flush ran %v after the signal, before the propagation delay elapsed", got)
		}
	})
}

// The propagation delay is measured from the signal, and the drain starts only
// after it. Assert the boundary rather than trusting the sequence above.
func TestDrainStartsAfterThePropagationDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)
		j := h.journal

		spec := h.spec()
		spec.PropagationDelay = 6 * time.Second
		spec.DrainTimeout = 10 * time.Second

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()
		synctest.Wait()

		start := time.Now()
		cancel()
		synctest.Wait()

		// One tick before the window closes the listener still answers.
		time.Sleep(6*time.Second - time.Millisecond)
		if code, _ := h.get(t, "/fast"); code != http.StatusOK {
			t.Errorf("request just before the window closes = %d, want 200", code)
		}

		<-done
		closedAt := time.Since(start)
		if closedAt < 6*time.Second {
			t.Errorf("Run returned %v after the signal, want at least the 6s propagation delay", closedAt)
		}
		_ = j

		// After the drain the listener is gone.
		if code, _ := h.get(t, "/fast"); code == http.StatusOK {
			t.Error("server still answering after Run returned")
		}
	})
}

// THE ordering assertion this package exists for. Flush must run strictly
// after the drain completes: telemetry flushed before the drain drops the
// final requests' spans on every single deploy, which is exactly when you most
// want them.
func TestFlushRunsStrictlyAfterTheDrainCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// A handler slower than the propagation delay, so it is unambiguously
		// still in flight when Shutdown is called.
		h := newHarness(t, 5*time.Second)
		j := h.journal

		spec := h.spec()
		spec.PropagationDelay = 2 * time.Second
		spec.DrainTimeout = 30 * time.Second
		spec.Flush = func(context.Context) error {
			j.record("flush")
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()
		synctest.Wait()

		// Start a long request, then signal while it is running.
		var code int
		var body string
		reqDone := make(chan struct{})
		go func() {
			defer close(reqDone)
			code, body = h.get(t, "/slow")
		}()

		time.Sleep(time.Second) // let the handler start
		synctest.Wait()
		if _, ok := j.at("handler:start"); !ok {
			t.Fatal("handler never started")
		}

		j.record("sigterm")
		cancel()

		<-reqDone
		if err := <-done; err != nil {
			t.Errorf("Run() = %v", err)
		}

		// The in-flight request completed rather than being cut off.
		if code != http.StatusOK || body != "served" {
			t.Errorf("in-flight request: %d %q, want 200 served", code, body)
		}

		j.assertBefore(t, "sigterm", "handler:done")
		j.assertBefore(t, "handler:done", "flush")
	})
}

// DrainTimeout bounds the wait. A handler that ignores the drain must not hold
// the pod past terminationGracePeriodSeconds, where SIGKILL is waiting.
func TestDrainTimeoutBoundsAStuckHandler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)
		j := h.journal

		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = 3 * time.Second
		spec.Flush = func(context.Context) error {
			j.record("flush")
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()
		synctest.Wait()

		reqDone := make(chan struct{})
		go func() {
			defer close(reqDone)
			_, _ = h.get(t, "/stuck") // never returns within the budget
		}()
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if _, ok := j.at("stuck:start"); !ok {
			t.Fatal("the stuck handler never started")
		}

		start := time.Now()
		cancel()

		err := <-done
		elapsed := time.Since(start)

		if !errors.Is(err, lifecycle.ErrDrainTimeout) {
			t.Errorf("Run() = %v, want it to wrap ErrDrainTimeout", err)
		}
		if want := 4 * time.Second; elapsed != want {
			t.Errorf("Run took %v, want exactly %v (1s propagation + 3s drain)", elapsed, want)
		}
		// Telemetry is still flushed. A drain that timed out is precisely when
		// the spans matter.
		if _, ok := j.at("flush"); !ok {
			t.Errorf("flush was skipped after a drain timeout; journal = %v", j.names())
		}

		// The wedged connection was closed rather than left holding the pod.
		close(h.stuck)
		h.stuck = make(chan struct{}) // so the cleanup close is not a double close
		<-reqDone
	})
}

// Flush is bounded too, and its failure is reported rather than swallowed.
func TestFlushErrorsAreReportedAndBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		flushErr := errors.New("collector unreachable")
		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.Flush = func(context.Context) error { return flushErr }

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()
		synctest.Wait()
		cancel()

		if err := <-done; !errors.Is(err, flushErr) {
			t.Errorf("Run() = %v, want it to wrap the flush error", err)
		}
	})
}

func TestFlushIsGivenABudgetEvenWhenTheParentIsAlreadyCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		var flushDeadline time.Duration
		var flushCtxErr error
		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.FlushTimeout = 7 * time.Second
		spec.Flush = func(ctx context.Context) error {
			flushCtxErr = ctx.Err()
			if dl, ok := ctx.Deadline(); ok {
				flushDeadline = time.Until(dl)
			}
			return nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()
		synctest.Wait()
		cancel()
		<-done

		// SIGTERM handling routinely hands the flush a dying context. It must
		// still get its full budget, or the last telemetry never leaves.
		if flushCtxErr != nil {
			t.Errorf("flush context was already cancelled: %v", flushCtxErr)
		}
		if flushDeadline != 7*time.Second {
			t.Errorf("flush budget = %v, want 7s", flushDeadline)
		}
	})
}

// --- workers -----------------------------------------------------------

// The documented stance, part one: a worker's context is cancelled at the
// START of the sequence, together with the readiness flip. A worker that picks
// up a new 30s ingest cycle after SIGTERM is the failure mode this prevents.
func TestWorkerContextIsCancelledImmediatelyOnSignal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)
		j := h.journal

		spec := h.spec()
		spec.PropagationDelay = 5 * time.Second
		spec.DrainTimeout = 5 * time.Second
		spec.Workers = []lifecycle.Worker{{
			Name: "ingest",
			Run: func(ctx context.Context) error {
				j.record("worker:start")
				<-ctx.Done()
				j.record("worker:cancelled")
				return nil
			},
		}}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()
		synctest.Wait()

		if _, ok := j.at("worker:start"); !ok {
			t.Fatal("Run did not start the worker")
		}

		signalAt := time.Since(j.startedAt())
		j.record("sigterm")
		cancel()
		synctest.Wait()

		cancelledAt := j.mustAt(t, "worker:cancelled")
		if cancelledAt != signalAt {
			t.Errorf("worker cancelled %v after the signal, want immediately (not after the %v propagation delay)",
				cancelledAt-signalAt, spec.PropagationDelay)
		}
		j.assertBefore(t, "sigterm", "worker:cancelled")
		<-done
	})
}

// The documented stance, part two: FinishCurrentCycle decides whether the
// sequence WAITS for the worker to return.
func TestFinishCurrentCycleWorkerIsWaitedForBeforeFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)
		j := h.journal

		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = 2 * time.Second
		spec.WorkerStopTimeout = 30 * time.Second
		spec.Flush = func(context.Context) error {
			j.record("flush")
			return nil
		}
		spec.Workers = []lifecycle.Worker{{
			Name:               "ingest",
			FinishCurrentCycle: true,
			Run: func(ctx context.Context) error {
				<-ctx.Done()
				// Finish the cycle in hand, on a context that is already done.
				time.Sleep(8 * time.Second)
				j.record("worker:finished-cycle")
				return nil
			},
		}}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()
		synctest.Wait()

		start := time.Now()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v", err)
		}

		// The worker's 8s cycle outlasts the 1s + 2s server sequence, so Run
		// cannot have returned before it: waiting is the point.
		if elapsed := time.Since(start); elapsed < 8*time.Second {
			t.Errorf("Run returned after %v, before the worker finished its cycle", elapsed)
		}
		j.assertBefore(t, "worker:finished-cycle", "flush")
	})
}

// The other stance: an abort worker is cancelled and NOT waited for.
func TestAbortWorkerDoesNotDelayShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = 2 * time.Second
		spec.WorkerStopTimeout = 60 * time.Second
		release := make(chan struct{})
		spec.Workers = []lifecycle.Worker{{
			Name: "slow-to-notice",
			Run: func(ctx context.Context) error {
				<-ctx.Done()
				<-release // would hang forever if Run waited
				return nil
			},
		}}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()
		synctest.Wait()

		start := time.Now()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v", err)
		}
		// Propagation delay only. The drain has nothing in flight, so it
		// returns at once, and the 60s WorkerStopTimeout is never consumed --
		// which is the whole claim of the abort stance.
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Errorf("Run took %v, want 1s; an abort worker must not be waited for", elapsed)
		}
		close(release)
		synctest.Wait()
	})
}

// A finish-current-cycle worker that never returns is bounded too. Nothing in
// the sequence may outlast its own budget, or SIGKILL arrives mid-flush.
func TestWorkerStopTimeoutIsEnforced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.WorkerStopTimeout = 4 * time.Second
		release := make(chan struct{})
		spec.Workers = []lifecycle.Worker{{
			Name:               "wedged",
			FinishCurrentCycle: true,
			Run: func(context.Context) error {
				<-release
				return nil
			},
		}}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()
		synctest.Wait()

		start := time.Now()
		cancel()
		err := <-done

		if !errors.Is(err, lifecycle.ErrWorkerStopTimeout) {
			t.Errorf("Run() = %v, want it to wrap ErrWorkerStopTimeout", err)
		}
		// 1s propagation, an instant drain (nothing in flight), then the
		// full 4s worker budget.
		if want := 5 * time.Second; time.Since(start) != want {
			t.Errorf("Run took %v, want %v (1s propagation + 4s worker budget)", time.Since(start), want)
		}
		close(release)
		synctest.Wait()
	})
}

// A worker that dies takes the process down with it, gracefully. A service
// whose ingest loop has crashed but whose /readyz still says 200 is worse than
// one that restarts.
func TestWorkerFailureInitiatesShutdownAndIsReported(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		boom := errors.New("ingest loop died")
		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.Workers = []lifecycle.Worker{{
			Name: "ingest",
			Run: func(context.Context) error {
				time.Sleep(2 * time.Second)
				return boom
			},
		}}

		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(context.Background(), spec) }()

		err := <-done
		if !errors.Is(err, boom) {
			t.Errorf("Run() = %v, want it to wrap the worker error", err)
		}
		if !h.ready.ShuttingDown() {
			t.Error("a failed worker did not flip readiness")
		}
	})
}

// A worker returning nil is a completed job, not a failure, and must not tear
// the process down.
func TestWorkerReturningNilDoesNotShutDownTheProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.Workers = []lifecycle.Worker{{
			Name: "one-shot",
			Run: func(context.Context) error {
				time.Sleep(time.Second)
				return nil
			},
		}}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()

		time.Sleep(5 * time.Second)
		synctest.Wait()

		select {
		case err := <-done:
			t.Fatalf("Run returned after a worker finished cleanly: %v", err)
		default:
		}
		if h.ready.ShuttingDown() {
			t.Error("readiness flipped after a worker finished cleanly")
		}

		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v", err)
		}
	})
}

// A panicking worker must not take the process down uncontrolled. It is a
// crash, so shutdown is right; an unrecovered panic skipping the drain and the
// flush is not.
func TestPanickingWorkerBecomesAGracefulShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)
		j := h.journal

		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.Flush = func(context.Context) error {
			j.record("flush")
			return nil
		}
		spec.Workers = []lifecycle.Worker{{
			Name: "boom",
			Run: func(context.Context) error {
				time.Sleep(time.Second)
				panic("worker bug")
			},
		}}

		err := lifecycle.Run(context.Background(), spec)
		if err == nil || !strings.Contains(err.Error(), "worker bug") {
			t.Errorf("Run() = %v, want it to report the panic", err)
		}
		if _, ok := j.at("flush"); !ok {
			t.Error("a panicking worker skipped the telemetry flush")
		}
	})
}

// --- misc contract -----------------------------------------------------

// A listen failure must be reported at startup, not swallowed into a process
// that looks alive and serves nothing.
func TestListenFailureIsReturned(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	// Two servers on the same address: the second cannot bind.
	err = lifecycle.Run(context.Background(), lifecycle.Spec{
		Servers: []*http.Server{{Addr: ln.Addr().String(), ReadHeaderTimeout: time.Second}},
		Signals: []os.Signal{},
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("Run() = nil, want a listen error")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("Run() = %v, want a listen error", err)
	}
}

// Both listeners come down. Leaving the admin port serving after the API port
// has drained means the pod keeps answering probes it can no longer honour.
func TestAllServersAreShutDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lnA, lnB := newPipeListener(), newPipeListener()
		srvA := &http.Server{Handler: okHandler(), ReadHeaderTimeout: time.Second}
		srvB := &http.Server{Handler: okHandler(), ReadHeaderTimeout: time.Second}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- lifecycle.Run(ctx, lifecycle.Spec{
				Servers:          []*http.Server{srvA, srvB},
				Listeners:        []net.Listener{lnA, lnB},
				PropagationDelay: time.Second,
				DrainTimeout:     time.Second,
				Signals:          []os.Signal{},
				Logger:           slog.New(slog.DiscardHandler),
			})
		}()
		synctest.Wait()

		for name, ln := range map[string]*pipeListener{"a": lnA, "b": lnB} {
			if code := getStatus(t, ln, "/"); code != http.StatusOK {
				t.Fatalf("server %s pre-shutdown = %d, want 200", name, code)
			}
		}

		cancel()
		<-done

		for name, ln := range map[string]*pipeListener{"a": lnA, "b": lnB} {
			if code := getStatus(t, ln, "/"); code == http.StatusOK {
				t.Errorf("server %s still answering after shutdown", name)
			}
		}
	})
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
}

func getStatus(t *testing.T, ln *pipeListener, path string) int {
	t.Helper()
	c := ln.client()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://pipe"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// A nil Readiness must not panic. Not every service has one wired on day one.
func TestNilReadinessIsTolerated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ln := newPipeListener()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- lifecycle.Run(ctx, lifecycle.Spec{
				Servers:          []*http.Server{{Handler: okHandler(), ReadHeaderTimeout: time.Second}},
				Listeners:        []net.Listener{ln},
				PropagationDelay: time.Second,
				DrainTimeout:     time.Second,
				Signals:          []os.Signal{},
				Logger:           slog.New(slog.DiscardHandler),
			})
		}()
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v", err)
		}
	})
}

func TestDefaultsAreApplied(t *testing.T) {
	t.Parallel()

	s := lifecycle.Spec{}.WithDefaults()
	if s.PropagationDelay != lifecycle.DefaultPropagationDelay {
		t.Errorf("PropagationDelay = %v, want %v", s.PropagationDelay, lifecycle.DefaultPropagationDelay)
	}
	if s.DrainTimeout != lifecycle.DefaultDrainTimeout {
		t.Errorf("DrainTimeout = %v, want %v", s.DrainTimeout, lifecycle.DefaultDrainTimeout)
	}
	if s.FlushTimeout != lifecycle.DefaultFlushTimeout {
		t.Errorf("FlushTimeout = %v, want %v", s.FlushTimeout, lifecycle.DefaultFlushTimeout)
	}
	if s.WorkerStopTimeout != lifecycle.DefaultWorkerStopTimeout {
		t.Errorf("WorkerStopTimeout = %v, want %v", s.WorkerStopTimeout, lifecycle.DefaultWorkerStopTimeout)
	}
	if lifecycle.DefaultPropagationDelay != 4*time.Second {
		t.Errorf("DefaultPropagationDelay = %v, want 4s", lifecycle.DefaultPropagationDelay)
	}
}

// The chart derives terminationGracePeriodSeconds from this. If the pod's
// grace period is shorter than the sequence, SIGKILL lands mid-drain and the
// guarantees above are decoration.
func TestGracePeriodCoversTheWholeSequence(t *testing.T) {
	t.Parallel()

	s := lifecycle.Spec{
		PropagationDelay:  4 * time.Second,
		DrainTimeout:      20 * time.Second,
		WorkerStopTimeout: 10 * time.Second,
		FlushTimeout:      5 * time.Second,
	}
	sum := s.PropagationDelay + s.DrainTimeout + s.WorkerStopTimeout + s.FlushTimeout
	if got := s.GracePeriod(); got <= sum {
		t.Errorf("GracePeriod() = %v, want more than the bare sum %v (it must include margin)", got, sum)
	}
	if got, want := s.TerminationGracePeriodSeconds(), int(s.GracePeriod().Seconds()); got < want {
		t.Errorf("TerminationGracePeriodSeconds() = %d, want at least %d", got, want)
	}
	// Defaults must produce a usable number without any configuration.
	if got := (lifecycle.Spec{}).TerminationGracePeriodSeconds(); got <= 0 {
		t.Errorf("default TerminationGracePeriodSeconds() = %d, want positive", got)
	}
}

func TestMismatchedListenersIsAProgrammingError(t *testing.T) {
	t.Parallel()

	err := lifecycle.Run(context.Background(), lifecycle.Spec{
		Servers:   []*http.Server{{}, {}},
		Listeners: []net.Listener{newPipeListener()},
		Signals:   []os.Signal{},
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("Run() = nil, want an error for a Listeners/Servers length mismatch")
	}
}
