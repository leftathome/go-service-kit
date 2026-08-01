package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/go-service-kit/httpapi"
)

func getReadyz(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, httpapi.ReadinessReport) {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/readyz")

	var report httpapi.ReadinessReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode readiness body: %v (%s)", err, rec.Body.String())
	}
	return rec, report
}

func TestReadinessNoChecksIsReady(t *testing.T) {
	t.Parallel()

	r := httpapi.NewReadiness()
	rec, report := getReadyz(t, r.Handler())

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if report.Status != httpapi.StatusOK {
		t.Errorf("report.Status = %q, want %q", report.Status, httpapi.StatusOK)
	}
	if len(report.Checks) != 0 {
		t.Errorf("checks = %v, want none", report.Checks)
	}
}

func TestReadinessAllChecksPass(t *testing.T) {
	t.Parallel()

	r := httpapi.NewReadiness()
	r.Register("store", func(context.Context) error { return nil })
	r.Register("cache", func(context.Context) error { return nil })

	rec, report := getReadyz(t, r.Handler())
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if len(report.Checks) != 2 {
		t.Fatalf("checks = %v, want 2", report.Checks)
	}
	for _, c := range report.Checks {
		if c.Status != httpapi.StatusOK {
			t.Errorf("check %q status = %q, want ok", c.Name, c.Status)
		}
	}
}

// The body must name the failing check. An operator reading a 503 needs to
// know WHICH dependency is down without attaching a debugger.
func TestReadinessFailingCheckIsNamedInTheBody(t *testing.T) {
	t.Parallel()

	r := httpapi.NewReadiness()
	r.Register("store", func(context.Context) error { return nil })
	r.Register("postgres", func(context.Context) error { return errors.New("dial tcp: connection refused") })

	rec := do(t, r.Handler(), http.MethodGet, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "postgres") {
		t.Errorf("body does not name the failing check: %s", body)
	}
	if !strings.Contains(body, "connection refused") {
		t.Errorf("body does not carry the failure reason: %s", body)
	}

	var report httpapi.ReadinessReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Status != httpapi.StatusFailed {
		t.Errorf("report.Status = %q, want %q", report.Status, httpapi.StatusFailed)
	}
	var found bool
	for _, c := range report.Checks {
		if c.Name == "postgres" {
			found = true
			if c.Status != httpapi.StatusFailed {
				t.Errorf("postgres status = %q, want failed", c.Status)
			}
			if c.Error == "" {
				t.Error("postgres check has no error text")
			}
		}
	}
	if !found {
		t.Errorf("postgres check missing from report: %+v", report)
	}
}

func TestReadinessShuttingDownIs503EvenWhenChecksPass(t *testing.T) {
	t.Parallel()

	r := httpapi.NewReadiness()
	r.Register("store", func(context.Context) error { return nil })

	if pre, _ := getReadyz(t, r.Handler()); pre.Code != http.StatusOK {
		t.Fatalf("precondition: status = %d, want 200", pre.Code)
	}

	r.SetShuttingDown()

	rec, report := getReadyz(t, r.Handler())
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if report.Status != httpapi.StatusShuttingDown {
		t.Errorf("report.Status = %q, want %q", report.Status, httpapi.StatusShuttingDown)
	}
	if !r.ShuttingDown() {
		t.Error("ShuttingDown() = false after SetShuttingDown()")
	}
}

// A dependency that hangs must not hang /readyz forever: the check budget
// turns a hang into a reported failure.
func TestReadinessCheckTimeoutBecomesAFailure(t *testing.T) {
	t.Parallel()

	r := httpapi.NewReadiness(httpapi.WithCheckTimeout(20 * time.Millisecond))
	r.Register("slow", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	rec := do(t, r.Handler(), http.MethodGet, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "slow") {
		t.Errorf("body does not name the slow check: %s", rec.Body.String())
	}
}

// A panicking check is a bug in the check, not a reason to take the admin
// listener down with it.
func TestReadinessPanickingCheckIsAFailureNotACrash(t *testing.T) {
	t.Parallel()

	r := httpapi.NewReadiness()
	r.Register("boom", func(context.Context) error { panic("check bug") })

	rec := do(t, r.Handler(), http.MethodGet, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("body does not name the panicking check: %s", rec.Body.String())
	}
}

func TestReadinessReportIsStablyOrdered(t *testing.T) {
	t.Parallel()

	r := httpapi.NewReadiness()
	for _, n := range []string{"zebra", "alpha", "middle"} {
		r.Register(n, func(context.Context) error { return nil })
	}

	_, report := getReadyz(t, r.Handler())
	got := make([]string, 0, len(report.Checks))
	for _, c := range report.Checks {
		got = append(got, c.Name)
	}
	want := []string{"alpha", "middle", "zebra"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("check order = %v, want %v", got, want)
	}
}

// Registering the same name twice is a wiring bug that would otherwise
// silently drop a dependency check.
func TestReadinessDuplicateRegistrationPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("duplicate Register did not panic")
		}
	}()
	r := httpapi.NewReadiness()
	r.Register("store", func(context.Context) error { return nil })
	r.Register("store", func(context.Context) error { return nil })
}

// lifecycle.Spec.Readiness accepts anything with SetShuttingDown. This is the
// one cross-package coupling in the kit; assert the shape here so a rename
// cannot pass httpapi's tests while breaking lifecycle's.
func TestReadinessSatisfiesTheLifecycleContract(t *testing.T) {
	t.Parallel()

	var _ interface{ SetShuttingDown() } = httpapi.NewReadiness()
}
