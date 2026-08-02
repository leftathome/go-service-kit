package lifecycle_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/leftathome/go-service-kit/lifecycle"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The third stance, added because neither of the first two fits N independent
// pollers. nagus polls six connectors and its whole design is per-source
// failure isolation: with only "abort silently" and "crash the process" on
// offer, such a service is forced to swallow every error inside its own loop,
// which makes a permanently wedged poller invisible.

// runRecorder is a worker body that fails every time and records, on the fake
// clock, when each attempt began.
type runRecorder struct {
	mu     sync.Mutex
	starts []time.Time
	err    error

	// runFor is how long each attempt lasts before failing.
	runFor time.Duration
}

func (r *runRecorder) run(ctx context.Context) error {
	r.mu.Lock()
	r.starts = append(r.starts, time.Now())
	r.mu.Unlock()
	if r.runFor > 0 {
		select {
		case <-time.After(r.runFor):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.err
}

func (r *runRecorder) attempts() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.starts...)
}

// The headline behaviour: a failing worker comes back instead of taking the
// process with it, and the listeners keep serving throughout.
func TestRestartWithBackoffKeepsTheProcessUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		rec := &runRecorder{err: errors.New("storefront returned 429")}
		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.Workers = []lifecycle.Worker{{
			Name:      "poll-serverpartdeals",
			OnFailure: lifecycle.RestartWithBackoff,
			Backoff: lifecycle.Backoff{
				Initial: time.Second,
				Max:     4 * time.Second,
				Jitter:  -1, // exact schedule
			},
			Run: rec.run,
		}}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()

		time.Sleep(30 * time.Second)
		synctest.Wait()

		// Still serving, still ready: the failure was contained.
		if code, body := h.get(t, "/fast"); code != http.StatusOK {
			t.Errorf("the listener stopped serving after a worker failed: %d %s", code, body)
		}
		if h.ready.ShuttingDown() {
			t.Error("a restarting worker flipped readiness; only a crash should")
		}
		if n := len(rec.attempts()); n < 5 {
			t.Errorf("worker attempted %d times in 30s, want it to keep restarting", n)
		}

		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v, want nil: restarts are not a process failure", err)
		}
	})
}

// Exponential, capped, and measured on the fake clock. Without a cap a source
// that is down overnight is never polled again; without growth a source that
// is rate-limiting gets hammered.
func TestRestartDelaysAreExponentialAndCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		rec := &runRecorder{err: errors.New("boom")}
		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.Workers = []lifecycle.Worker{{
			Name:      "ingest",
			OnFailure: lifecycle.RestartWithBackoff,
			Backoff: lifecycle.Backoff{
				Initial:    time.Second,
				Max:        8 * time.Second,
				Factor:     2,
				Jitter:     -1,
				ResetAfter: -1, // never reset; assert the raw schedule
			},
			Run: rec.run,
		}}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()

		time.Sleep(60 * time.Second)
		synctest.Wait()
		cancel()
		<-done

		starts := rec.attempts()
		if len(starts) < 7 {
			t.Fatalf("only %d attempts in 60s, want at least 7", len(starts))
		}
		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
		for i, w := range want {
			got := starts[i+1].Sub(starts[i])
			if got != w {
				t.Errorf("delay before restart %d = %v, want %v", i+1, got, w)
			}
		}
	})
}

// Per-source failure isolation, stated as a test: one poller in a permanent
// crash loop must not touch another poller or the read surface.
func TestOneFailingWorkerDoesNotDisturbTheOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		broken := &runRecorder{err: errors.New("ebay 503")}
		var healthyTicks int
		var mu sync.Mutex

		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.Workers = []lifecycle.Worker{
			{
				Name:      "poll-ebay",
				OnFailure: lifecycle.RestartWithBackoff,
				Backoff:   lifecycle.Backoff{Initial: time.Second, Max: time.Second, Jitter: -1},
				Run:       broken.run,
			},
			{
				Name: "poll-shopify",
				Run: func(ctx context.Context) error {
					for {
						select {
						case <-ctx.Done():
							return nil
						case <-time.After(2 * time.Second):
							mu.Lock()
							healthyTicks++
							mu.Unlock()
						}
					}
				},
			},
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()

		time.Sleep(20 * time.Second)
		synctest.Wait()

		mu.Lock()
		ticks := healthyTicks
		mu.Unlock()
		if ticks < 9 {
			t.Errorf("the healthy worker ticked %d times in 20s, want ~10: it was disturbed by its neighbour", ticks)
		}
		if code, _ := h.get(t, "/fast"); code != http.StatusOK {
			t.Errorf("the read surface stopped serving: %d", code)
		}

		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	})
}

// A worker the service genuinely cannot run without can still bring the
// process down -- after N restarts, not on the first error.
func TestMaxRestartsEscalatesToAProcessCrash(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		boom := errors.New("schema migration failed")
		rec := &runRecorder{err: boom}
		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.Workers = []lifecycle.Worker{{
			Name:      "reconciler",
			OnFailure: lifecycle.RestartWithBackoff,
			Backoff: lifecycle.Backoff{
				Initial:     time.Second,
				Max:         time.Second,
				Jitter:      -1,
				MaxRestarts: 3,
			},
			Run: rec.run,
		}}

		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(context.Background(), spec) }()

		err := <-done
		if !errors.Is(err, lifecycle.ErrWorkerRestartsExhausted) {
			t.Errorf("Run() = %v, want ErrWorkerRestartsExhausted", err)
		}
		if !errors.Is(err, boom) {
			t.Errorf("Run() = %v, want it to wrap the worker's own error too", err)
		}
		if !h.ready.ShuttingDown() {
			t.Error("escalation did not flip readiness; it must be a full, orderly shutdown")
		}
		if n := len(rec.attempts()); n != 4 {
			t.Errorf("worker ran %d times, want 4 (the original plus MaxRestarts=3)", n)
		}
	})
}

// A worker that ran for a long time before failing is recovering, not
// escalating. Without this, a poller that fails once an hour eventually waits
// the maximum delay for a fault that is not getting worse.
func TestBackoffResetsAfterAStableRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		rec := &runRecorder{err: errors.New("transient"), runFor: 30 * time.Second}
		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.Workers = []lifecycle.Worker{{
			Name:      "ingest",
			OnFailure: lifecycle.RestartWithBackoff,
			Backoff: lifecycle.Backoff{
				Initial:    time.Second,
				Max:        time.Minute,
				Jitter:     -1,
				ResetAfter: 10 * time.Second,
			},
			Run: rec.run,
		}}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()

		time.Sleep(3 * (30*time.Second + time.Second))
		synctest.Wait()
		cancel()
		<-done

		starts := rec.attempts()
		if len(starts) < 3 {
			t.Fatalf("only %d attempts, want at least 3", len(starts))
		}
		// Each run lasts 30s, comfortably past ResetAfter, so every restart
		// waits the INITIAL delay rather than an escalating one.
		for i := 1; i < len(starts); i++ {
			gap := starts[i].Sub(starts[i-1]) - 30*time.Second
			if gap != time.Second {
				t.Errorf("delay before restart %d = %v, want 1s (the schedule must reset after a stable run)", i, gap)
			}
		}
	})
}

// Jitter defaults ON, so six pollers that all fail the instant a shared
// database dies do not retry in lockstep. It stays inside the cap.
func TestJitterSpreadsRestartsWithoutExceedingMax(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		rec := &runRecorder{err: errors.New("boom")}
		spec := h.spec()
		spec.PropagationDelay = time.Second
		spec.DrainTimeout = time.Second
		spec.Workers = []lifecycle.Worker{{
			Name:      "ingest",
			OnFailure: lifecycle.RestartWithBackoff,
			Backoff: lifecycle.Backoff{
				Initial:    10 * time.Second,
				Max:        10 * time.Second,
				ResetAfter: -1,
			}, // Jitter zero -> DefaultRestartJitter
			Run: rec.run,
		}}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()

		time.Sleep(2 * time.Minute)
		synctest.Wait()
		cancel()
		<-done

		starts := rec.attempts()
		if len(starts) < 5 {
			t.Fatalf("only %d attempts in 2m, want at least 5", len(starts))
		}
		lo := time.Duration(float64(10*time.Second) * (1 - lifecycle.DefaultRestartJitter))
		var varied bool
		for i := 1; i < len(starts); i++ {
			d := starts[i].Sub(starts[i-1])
			if d < lo || d > 10*time.Second {
				t.Errorf("jittered delay %v outside [%v, %v]; a jittered delay must not exceed Max", d, lo, 10*time.Second)
			}
			if d != 10*time.Second {
				varied = true
			}
		}
		if !varied {
			t.Error("every delay was exactly Max; jitter is not being applied")
		}
	})
}

// Countable, not just loggable: "one of my six pollers keeps dying" has to be
// alertable, and it is by construction neither a crash nor a silence.
func TestWorkerRestartsAreCounted(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	rec := &runRecorder{err: errors.New("boom")}
	spec := lifecycle.Spec{
		Signals:       []os.Signal{},
		Logger:        slog.New(slog.DiscardHandler),
		MeterProvider: mp,
		Workers: []lifecycle.Worker{{
			Name:      "poll-ebay",
			OnFailure: lifecycle.RestartWithBackoff,
			Backoff: lifecycle.Backoff{
				Initial:     time.Millisecond,
				Max:         time.Millisecond,
				Jitter:      -1,
				MaxRestarts: 3,
			},
			Run: rec.run,
		}},
		PropagationDelay: lifecycle.NoPropagationDelay,
		DrainTimeout:     time.Second,
	}

	if err := lifecycle.Run(context.Background(), spec); !errors.Is(err, lifecycle.ErrWorkerRestartsExhausted) {
		t.Fatalf("Run() = %v, want ErrWorkerRestartsExhausted", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	var got int64 = -1
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != lifecycle.ScopeName {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != lifecycle.WorkerRestartsInstrumentName {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want a monotonic Sum[int64]", m.Name, m.Data)
			}
			if !sum.IsMonotonic {
				t.Errorf("%s must be monotonic to be usable with rate()", m.Name)
			}
			for _, dp := range sum.DataPoints {
				v, ok := dp.Attributes.Value(attribute.Key("worker"))
				if !ok || v.AsString() != "poll-ebay" {
					t.Errorf("data point attributes = %v, want worker=poll-ebay", dp.Attributes.Encoded(attribute.DefaultEncoder()))
				}
				got = dp.Value
			}
		}
	}
	if got != 3 {
		t.Errorf("%s = %d, want 3 (the restarts, not the final escalating failure)", lifecycle.WorkerRestartsInstrumentName, got)
	}
}

// The default is unchanged: a bare Worker that fails still crashes the
// process, because for a service whose background work IS the service that is
// the right answer.
func TestCrashProcessRemainsTheZeroValuePolicy(t *testing.T) {
	t.Parallel()

	if lifecycle.RestartPolicy(0) != lifecycle.CrashProcess {
		t.Error("the zero RestartPolicy must be CrashProcess, or adding the field changed existing behaviour")
	}
	if got := lifecycle.CrashProcess.String(); got != "crash_process" {
		t.Errorf("CrashProcess.String() = %q", got)
	}
	if got := lifecycle.RestartWithBackoff.String(); got != "restart_with_backoff" {
		t.Errorf("RestartWithBackoff.String() = %q", got)
	}
}

func TestBackoffWithDefaults(t *testing.T) {
	t.Parallel()

	got := lifecycle.Backoff{}.WithDefaults()
	want := lifecycle.Backoff{
		Initial:    lifecycle.DefaultRestartInitialDelay,
		Max:        lifecycle.DefaultRestartMaxDelay,
		Factor:     lifecycle.DefaultRestartFactor,
		Jitter:     lifecycle.DefaultRestartJitter,
		ResetAfter: lifecycle.DefaultRestartResetAfter,
	}
	if got != want {
		t.Errorf("Backoff{}.WithDefaults() = %+v, want %+v", got, want)
	}

	// Negative jitter is the documented spelling of "none", and must not be
	// mistaken for "unset".
	if j := (lifecycle.Backoff{Jitter: -1}).WithDefaults().Jitter; j != 0 {
		t.Errorf("Backoff{Jitter: -1}.WithDefaults().Jitter = %v, want 0", j)
	}
	// Out-of-range values are clamped rather than producing a shrinking or
	// wildly overshooting schedule.
	clamped := lifecycle.Backoff{Factor: 0.5, Jitter: 5, Initial: 30 * time.Second, Max: time.Second}.WithDefaults()
	if clamped.Factor != 1 {
		t.Errorf("Factor = %v, want it clamped to 1", clamped.Factor)
	}
	if clamped.Jitter != 1 {
		t.Errorf("Jitter = %v, want it clamped to 1", clamped.Jitter)
	}
	if clamped.Max != 30*time.Second {
		t.Errorf("Max = %v, want it raised to Initial", clamped.Max)
	}
}

// --- propagation delay under replicas: 1 + Recreate --------------------

func TestNoPropagationDelaySkipsTheWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 0)

		spec := h.spec()
		// replicas: 1 + strategy: Recreate -- there is no other pod to route
		// to, so the wait is pure added downtime.
		spec.PropagationDelay = lifecycle.NoPropagationDelay
		spec.DrainTimeout = 10 * time.Second

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- lifecycle.Run(ctx, spec) }()
		synctest.Wait()

		start := time.Now()
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("shutdown took %v of fake time, want 0: the propagation wait was not skipped", elapsed)
		}
	})
}

func TestNoPropagationDelayIsExcludedFromTheGracePeriod(t *testing.T) {
	t.Parallel()

	base := lifecycle.Spec{DrainTimeout: 10 * time.Second, FlushTimeout: 5 * time.Second, WorkerStopTimeout: 10 * time.Second}

	withDelay := base
	withDelay.PropagationDelay = 4 * time.Second
	without := base
	without.PropagationDelay = lifecycle.NoPropagationDelay

	if got, want := without.GracePeriod(), withDelay.GracePeriod()-4*time.Second; got != want {
		t.Errorf("GracePeriod with the delay disabled = %v, want %v", got, want)
	}
	if lifecycle.NoPropagationDelay >= 0 {
		t.Error("NoPropagationDelay must be negative, or Spec.WithDefaults will read it as unset")
	}
	// The default is unchanged. It is correct for the multi-replica case and
	// this work must not have moved it.
	if got := (lifecycle.Spec{}).WithDefaults().PropagationDelay; got != lifecycle.DefaultPropagationDelay {
		t.Errorf("default PropagationDelay = %v, want %v", got, lifecycle.DefaultPropagationDelay)
	}
}
