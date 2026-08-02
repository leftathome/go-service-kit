package obs

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/leftathome/go-service-kit/outbound"
)

func setupForMetrics(t *testing.T) *Providers {
	t.Helper()
	clearOTLPEnv(t)
	p, err := Setup(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

// The names are the contract. If a dependency bump changes what the OTel
// Prometheus translator produces, this test fails and OutboundMetricNames --
// and every dashboard written against it -- gets updated deliberately rather
// than discovered empty six weeks later.
func TestOutboundMetricNames(t *testing.T) {
	p := setupForMetrics(t)
	m, err := p.OutboundMetrics()
	if err != nil {
		t.Fatalf("OutboundMetrics: %v", err)
	}

	m.RecordCall(outbound.CallEvent{
		Host: "api.example.com", Method: "GET", Attempt: 1, StatusCode: 200,
		Duration: 120 * time.Millisecond, LimiterDelay: 30 * time.Millisecond,
		CallsUsed: 1, BudgetRemaining: 4999,
	})

	names, _ := gatherNames(t, p)
	for _, want := range OutboundMetricNames {
		if !slices.Contains(names, want) {
			t.Errorf("metric %q missing from /metrics; exported: %v", want, names)
		}
	}
	// Guard against the classic translator surprise: naming an instrument
	// "outbound_calls_total" would export outbound_calls_total_total.
	for _, unwanted := range []string{"outbound_calls_total_total", "outbound_calls_calls_total", "outbound_calls"} {
		if slices.Contains(names, unwanted) {
			t.Errorf("unexpected family %q; the instrument name and unit are wrong", unwanted)
		}
	}
}

func TestOutboundMetricLabels(t *testing.T) {
	p := setupForMetrics(t)
	m, err := p.OutboundMetrics()
	if err != nil {
		t.Fatalf("OutboundMetrics: %v", err)
	}
	m.RecordCall(outbound.CallEvent{
		Host: "api.example.com", Method: "POST", StatusCode: 503,
		Duration: time.Second, LimiterDelay: 0, BudgetRemaining: 12,
	})

	_, byName := gatherNames(t, p)
	mf := byName["outbound_calls_total"]
	if mf == nil || len(mf.GetMetric()) == 0 {
		t.Fatal("outbound_calls_total has no samples")
	}
	got := map[string]string{}
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, want := range map[string]string{"host": "api.example.com", "method": "POST", "status": "503"} {
		if got[k] != want {
			t.Errorf("label %s = %q, want %q (all labels: %v)", k, got[k], want, got)
		}
	}
}

// A transport failure has no status code. It must still be counted, and it must
// be distinguishable from a 5xx, because the two get fixed by different people.
func TestOutboundMetricsRecordTransportFailuresAsStatusZero(t *testing.T) {
	p := setupForMetrics(t)
	m, err := p.OutboundMetrics()
	if err != nil {
		t.Fatalf("OutboundMetrics: %v", err)
	}
	m.RecordCall(outbound.CallEvent{
		Host: "api.example.com", Method: "GET", StatusCode: 0,
		Err: errors.New("dial tcp: i/o timeout"), BudgetRemaining: -1,
	})

	_, byName := gatherNames(t, p)
	mf := byName["outbound_calls_total"]
	if mf == nil {
		t.Fatal("outbound_calls_total missing")
	}
	found := false
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		if lp.GetName() == "status" && lp.GetValue() == "0" {
			found = true
		}
	}
	if !found {
		t.Error("a transport failure did not produce status=\"0\"")
	}
	// An unmetered client reports -1; that is "no budget", not "minus one".
	if byName["outbound_budget_remaining"] != nil {
		t.Error("outbound_budget_remaining was exported for an unmetered client")
	}
}

func TestOutboundMetricsRejectsNilMeter(t *testing.T) {
	t.Parallel()
	if _, err := OutboundMetrics(nil); err == nil {
		t.Fatal("OutboundMetrics(nil) succeeded, want an error")
	}
}

// End to end: the bridge is wired the way the doc says to wire it, and a real
// outbound client feeds it.
func TestOutboundMetricsBridgeARealClient(t *testing.T) {
	p := setupForMetrics(t)
	m, err := p.OutboundMetrics()
	if err != nil {
		t.Fatalf("OutboundMetrics: %v", err)
	}
	budget, err := outbound.NewWindowedBudget(outbound.BudgetConfig{Limit: 5, Window: 24 * time.Hour, Calendar: true})
	if err != nil {
		t.Fatalf("NewWindowedBudget: %v", err)
	}
	c, err := outbound.New(outbound.Config{
		Product: "kit-test", ContactURL: "https://example.invalid/bots",
		Timeout: time.Second, Unlimited: true,
		Budget: budget, Metrics: m,
	})
	if err != nil {
		t.Fatalf("outbound.New: %v", err)
	}
	// The destination is refused by the SSRF guard before any dial; that is
	// still one attempt, and the point here is that the event reaches the
	// bridge at all.
	resp, err := c.Get(context.Background(), "http://127.0.0.1:1/blocked")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected the guard to refuse a loopback destination")
	}

	_, byName := gatherNames(t, p)
	for _, want := range []string{"outbound_calls_total", "outbound_call_duration_seconds", "outbound_budget_remaining"} {
		if byName[want] == nil {
			t.Errorf("%s missing after a real client call", want)
		}
	}
	if mf := byName["outbound_budget_remaining"]; mf != nil {
		if got := mf.GetMetric()[0].GetGauge().GetValue(); got != 4 {
			t.Errorf("outbound_budget_remaining = %v, want 4", got)
		}
	}
}

// --- preserving an existing metric family name across a migration ------------

// This is the mechanism documented in the package comment, verified rather than
// asserted. nagus must keep nagus_ebay_api_calls_* across its migration or lose
// the only operational signal it has, and working this out took reading
// newMeterProvider.
func TestNativeCollectorPreservesAnExistingFamilyName(t *testing.T) {
	p := setupForMetrics(t)

	// The escape hatch: a native Prometheus collector on Providers.PromRegistry
	// keeps the name it is given, verbatim.
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nagus_ebay_api_calls_budget",
		Help: "Configured eBay API call budget for the current UTC day.",
	}, []string{"source"})
	if err := p.PromRegistry.Register(g); err != nil {
		t.Fatalf("Register: %v", err)
	}
	g.WithLabelValues("ebay").Set(5000)

	// The same figure expressed as an OTel instrument does NOT keep its name:
	// the exporter's UnderscoreEscapingWithSuffixes strategy appends _total to
	// a counter.
	ctr, err := p.Meter.Int64Counter("nagus_ebay_api_calls")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	ctr.Add(context.Background(), 1)

	names, _ := gatherNames(t, p)
	if !slices.Contains(names, "nagus_ebay_api_calls_budget") {
		t.Errorf("the native collector's family name did not survive; got %v", names)
	}
	if slices.Contains(names, "nagus_ebay_api_calls") {
		t.Error("an OTel counter kept its exact name; the doc's warning is now wrong and must be rewritten")
	}
	if !slices.Contains(names, "nagus_ebay_api_calls_total") {
		t.Errorf("expected the translator to rename the OTel counter to *_total; got %v", names)
	}
}

// A dotted OTel instrument name is escaped to underscores, which is the other
// half of why an existing name cannot simply be re-expressed as an instrument.
func TestOTelInstrumentNamesAreEscapedToUnderscores(t *testing.T) {
	p := setupForMetrics(t)
	gauge, err := p.Meter.Int64Gauge("some.service.thing")
	if err != nil {
		t.Fatalf("Int64Gauge: %v", err)
	}
	gauge.Record(context.Background(), 1)

	names, _ := gatherNames(t, p)
	if !slices.Contains(names, "some_service_thing") {
		t.Errorf("dots were not escaped to underscores; got %v", names)
	}
}
