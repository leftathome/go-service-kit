package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Readiness status values. They appear on the wire, so treat them as part of
// the contract with whatever scrapes /readyz.
const (
	// StatusOK means every registered check answered without error.
	StatusOK = "ok"
	// StatusFailed means at least one check reported an error.
	StatusFailed = "failed"
	// StatusShuttingDown means the process is draining. Checks are not run:
	// the answer is 503 regardless of what they would say.
	StatusShuttingDown = "shutting_down"
)

// DefaultCheckTimeout bounds each individual readiness check. A dependency
// that hangs must become a reported failure, not a hung /readyz that the
// kubelet eventually times out with no explanation in the body.
const DefaultCheckTimeout = 2 * time.Second

// CheckResult is one dependency's answer.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Error is the failure text, present only when Status is "failed". It is
	// the error a dependency returned, not a user-supplied value.
	Error string `json:"error,omitempty"`
}

// ReadinessReport is the /readyz body. Checks are ordered by name so a diff
// between two scrapes is meaningful.
type ReadinessReport struct {
	Status string        `json:"status"`
	Checks []CheckResult `json:"checks"`
}

// Ready reports whether the report as a whole means "send me traffic".
func (r ReadinessReport) Ready() bool { return r.Status == StatusOK }

// Readiness is the ONE cross-package coupling in the kit: it is declared here,
// served by the admin listener's /readyz, and flipped by lifecycle.Run the
// instant SIGTERM arrives. Making that seam explicit is what stops three
// services inventing three different shutdown signals.
//
// # /healthz and /readyz are different questions
//
// /healthz is LIVENESS: "is this process wedged?" It answers a static 200 for
// as long as the process runs, including while dependencies are down and
// including while draining. A liveness probe that fails gets the container
// KILLED and restarted, so wiring dependency checks into it turns a Postgres
// blip into a fleet-wide CrashLoopBackOff -- restarting a Go process does not
// fix someone else's database.
//
// /readyz is READINESS: "should traffic be routed here right now?" It answers
// 503 when a dependency check fails or when the process is shutting down. A
// failing readiness probe removes the pod from the Service endpoints and
// leaves it running, which is exactly the behaviour you want for both a
// transient dependency outage and a graceful shutdown.
//
// The zero value is not usable; call [NewReadiness].
type Readiness struct {
	timeout time.Duration

	mu     sync.RWMutex
	names  []string
	checks map[string]func(context.Context) error

	shuttingDown atomic.Bool
}

// ReadinessOption configures a [Readiness].
type ReadinessOption func(*Readiness)

// WithCheckTimeout overrides [DefaultCheckTimeout], the per-check budget.
// Checks run concurrently, so this is the bound on the whole probe, not the
// sum over dependencies.
func WithCheckTimeout(d time.Duration) ReadinessOption {
	return func(r *Readiness) {
		if d > 0 {
			r.timeout = d
		}
	}
}

// NewReadiness returns a Readiness with no checks registered, which reports
// ready. Register dependency probes before serving.
func NewReadiness(opts ...ReadinessOption) *Readiness {
	r := &Readiness{
		timeout: DefaultCheckTimeout,
		checks:  make(map[string]func(context.Context) error),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register adds a dependency check. The name appears in the /readyz body, so
// make it the thing an operator would grep for ("postgres", "store", "keepa").
//
// Registering the same name twice panics: it is a wiring bug that would
// otherwise silently drop one of the two checks, and the resulting "everything
// is ready" is worse than a crash at startup.
func (r *Readiness) Register(name string, check func(context.Context) error) {
	if name == "" {
		panic("httpapi: readiness check registered with an empty name")
	}
	if check == nil {
		panic("httpapi: readiness check " + name + " is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.checks[name]; dup {
		panic("httpapi: readiness check registered twice: " + name)
	}
	r.checks[name] = check
	r.names = append(r.names, name)
	sort.Strings(r.names)
}

// SetShuttingDown flips /readyz to 503 permanently. lifecycle.Run calls this
// FIRST, before anything else in the shutdown sequence, so the endpoint
// controller starts removing this pod while it is still serving.
//
// It is idempotent and safe to call from any goroutine.
func (r *Readiness) SetShuttingDown() { r.shuttingDown.Store(true) }

// ShuttingDown reports whether [Readiness.SetShuttingDown] has been called.
func (r *Readiness) ShuttingDown() bool { return r.shuttingDown.Load() }

// Check runs every registered check concurrently, each under its own budget,
// and returns the report. It does not consult the shutting-down flag; the
// handler does.
func (r *Readiness) Check(ctx context.Context) ReadinessReport {
	r.mu.RLock()
	names := make([]string, len(r.names))
	copy(names, r.names)
	checks := make([]func(context.Context) error, len(names))
	for i, n := range names {
		checks[i] = r.checks[n]
	}
	timeout := r.timeout
	r.mu.RUnlock()

	report := ReadinessReport{Status: StatusOK, Checks: make([]CheckResult, len(names))}

	var wg sync.WaitGroup
	for i := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			report.Checks[i] = CheckResult{Name: names[i], Status: StatusOK}
			if err := runCheck(cctx, checks[i]); err != nil {
				report.Checks[i].Status = StatusFailed
				report.Checks[i].Error = err.Error()
			}
		}()
	}
	wg.Wait()

	for _, c := range report.Checks {
		if c.Status != StatusOK {
			report.Status = StatusFailed
			break
		}
	}
	return report
}

// runCheck isolates a panicking check. A bug in one dependency probe must not
// take the admin listener down: the admin listener is how you find out what is
// wrong.
func runCheck(ctx context.Context, check func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("check panicked: %v", p)
		}
	}()
	return check(ctx)
}

// Handler serves the /readyz body. Mount it on the ADMIN listener only.
//
// It answers 200 with {"status":"ok",...} when every check passes, and 503
// otherwise. The body always names each check and carries the failure text for
// the ones that failed, because "503" on its own tells an operator nothing.
//
// The body is application/json rather than RFC 9457 problem+json: /readyz is
// not part of the service's API, it is a kubelet-facing signal, and the report
// is not an error object. The public API's error convention is in errors.go.
func (r *Readiness) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var report ReadinessReport
		if r.ShuttingDown() {
			// Do not run the checks. The answer cannot change, and probing
			// dependencies during a drain is pointless load on them.
			report = ReadinessReport{Status: StatusShuttingDown, Checks: []CheckResult{}}
		} else {
			report = r.Check(req.Context())
		}

		status := http.StatusOK
		if report.Status != StatusOK {
			status = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(report)
	})
}
