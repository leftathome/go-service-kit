package obs

import (
	"context"
	"errors"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// clearOTLPEnv removes every OTLP endpoint variable for the duration of a test.
// t.Setenv cannot unset, and "set to empty" is not the same code path in the
// OTel SDK as "absent", so this restores absence explicitly.
func clearOTLPEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	} {
		if old, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		_ = os.Unsetenv(k)
	}
}

func testConfig() Config {
	return Config{
		ServiceName:    "kit-test",
		ServiceVersion: "0.0.1-test",
		Environment:    "test",
		LogLevel:       "info",
		LogOutput:      newSyncBuffer(),
	}
}

func TestSetupWithoutEndpointIsSilentNoop(t *testing.T) {
	clearOTLPEnv(t)

	p, err := Setup(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if p.Tracer == nil || p.Meter == nil || p.PromRegistry == nil || p.Logger == nil || p.Shutdown == nil {
		t.Fatalf("Providers has nil field: %+v", p)
	}
	if p.TraceExportEnabled {
		t.Error("TraceExportEnabled = true with no OTLP endpoint set; want false")
	}

	// Spans still work locally, they are simply dropped.
	_, span := p.Tracer.Start(context.Background(), "noop")
	span.End()

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Safe to call again: lifecycle may invoke Flush on more than one path.
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestSetupRecordsSpansWithInMemoryExporter(t *testing.T) {
	clearOTLPEnv(t)

	exp := tracetest.NewInMemoryExporter()
	cfg := testConfig()
	cfg.SpanExporter = exp

	p, err := Setup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !p.TraceExportEnabled {
		t.Error("TraceExportEnabled = false with an explicit SpanExporter; want true")
	}

	_, span := p.Tracer.Start(context.Background(), "unit-of-work")
	span.End()

	// Flush rather than Shutdown to read the buffer: tracetest's in-memory
	// exporter resets itself on Shutdown.
	sdkTP, ok := p.TracerProvider.(*sdktrace.TracerProvider)
	if !ok {
		t.Fatalf("TracerProvider is %T, want *sdktrace.TracerProvider", p.TracerProvider)
	}
	if err := sdkTP.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	got := exp.GetSpans()
	if len(got) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(got))
	}
	if got[0].Name != "unit-of-work" {
		t.Errorf("span name = %q, want %q", got[0].Name, "unit-of-work")
	}
	var haveService bool
	for _, kv := range got[0].Resource.Attributes() {
		if string(kv.Key) == "service.name" && kv.Value.AsString() == "kit-test" {
			haveService = true
		}
	}
	if !haveService {
		t.Errorf("span resource missing service.name=kit-test: %v", got[0].Resource.Attributes())
	}

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

var _ sdktrace.SpanExporter = (*tracetest.InMemoryExporter)(nil)

// stubCollector is a minimal OTLP/gRPC trace collector.
type stubCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	mu    sync.Mutex
	names []string
	got   chan struct{}
	once  sync.Once
}

func (s *stubCollector) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	s.mu.Lock()
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				s.names = append(s.names, sp.GetName())
			}
		}
	}
	s.mu.Unlock()
	s.once.Do(func() { close(s.got) })
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (s *stubCollector) spanNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

// TestOTLPGRPCExportReachesCollector is the test that keeps the OTLP path from
// rotting before Tempo exists in the cluster. It runs a real gRPC listener on
// 127.0.0.1 with an ephemeral port, so it is hermetic (no outbound network).
func TestOTLPGRPCExportReachesCollector(t *testing.T) {
	clearOTLPEnv(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stub := &stubCollector{got: make(chan struct{})}
	srv := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(srv, stub)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		<-serveDone
	})

	// Read OTel's own env var. The http:// scheme selects an insecure channel.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+lis.Addr().String())

	p, err := Setup(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !p.TraceExportEnabled {
		t.Fatal("TraceExportEnabled = false with OTEL_EXPORTER_OTLP_ENDPOINT set; want true")
	}

	_, span := p.Tracer.Start(context.Background(), "over-the-wire")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-stub.got:
	case <-ctx.Done():
		t.Fatal("collector received no spans before timeout")
	}
	names := stub.spanNames()
	if len(names) != 1 || names[0] != "over-the-wire" {
		t.Fatalf("collector span names = %v, want [over-the-wire]", names)
	}
}

func TestShutdownIsBoundedAndIdempotent(t *testing.T) {
	clearOTLPEnv(t)

	cfg := testConfig()
	cfg.ShutdownTimeout = 100 * time.Millisecond
	p, err := Setup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// An already-cancelled parent must not make Shutdown hang; it returns.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- p.Shutdown(ctx) }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Shutdown with cancelled ctx: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return with a cancelled context")
	}

	// The Once means the second call repeats the first result, not a panic.
	if err := p.Shutdown(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// gatherNames scrapes the registry the caller would serve at /metrics.
func gatherNames(t *testing.T, p *Providers) ([]string, map[string]*dto.MetricFamily) {
	t.Helper()
	mfs, err := p.PromRegistry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := make([]string, 0, len(mfs))
	byName := make(map[string]*dto.MetricFamily, len(mfs))
	for _, mf := range mfs {
		names = append(names, mf.GetName())
		byName[mf.GetName()] = mf
	}
	sort.Strings(names)
	return names, byName
}

// TestRuntimeMetricNames pins the EXACT Prometheus names exported by
// contrib/instrumentation/runtime. They are OTel-namespaced and are NOT the
// classic go_goroutines / go_memstats_* collector names. The Grafana dashboard
// is written against these, so a rename in an OTel bump must break this test
// rather than silently blank the dashboard.
func TestRuntimeMetricNames(t *testing.T) {
	clearOTLPEnv(t)

	p, err := Setup(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	names, _ := gatherNames(t, p)
	t.Logf("exported metric families:\n  %s", strings.Join(names, "\n  "))

	for _, want := range RuntimeMetricNames {
		found := false
		for _, got := range names {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("runtime metric %q missing from /metrics; exported: %v", want, names)
		}
	}

	// The classic collectors are deliberately NOT what we get here. If these
	// ever appear, something registered a duplicate source of truth.
	for _, unwanted := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		for _, got := range names {
			if got == unwanted {
				t.Errorf("unexpected classic collector metric %q; the runtime bridge uses OTel names", unwanted)
			}
		}
	}
}

func TestBuildInfoMetricIsQueryable(t *testing.T) {
	clearOTLPEnv(t)

	p, err := Setup(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	_, byName := gatherNames(t, p)
	mf, ok := byName[BuildInfoMetricName]
	if !ok {
		t.Fatalf("%s missing from /metrics", BuildInfoMetricName)
	}
	if len(mf.GetMetric()) != 1 {
		t.Fatalf("%s has %d series, want 1", BuildInfoMetricName, len(mf.GetMetric()))
	}
	labels := map[string]string{}
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	if labels["version"] != "0.0.1-test" {
		t.Errorf("version label = %q, want %q", labels["version"], "0.0.1-test")
	}
	if labels["service"] != "kit-test" {
		t.Errorf("service label = %q, want %q", labels["service"], "kit-test")
	}
	if got := mf.GetMetric()[0].GetGauge().GetValue(); got != 1 {
		t.Errorf("%s value = %v, want 1", BuildInfoMetricName, got)
	}
	if _, ok := byName["go_build_info"]; !ok {
		t.Error("go_build_info missing; the module build-info collector is not registered")
	}
}

func TestSetupDefaultsServiceNameFromOTelEnv(t *testing.T) {
	clearOTLPEnv(t)
	t.Setenv("OTEL_SERVICE_NAME", "from-env")

	cfg := testConfig()
	cfg.ServiceName = ""
	p, err := Setup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	_, byName := gatherNames(t, p)
	mf := byName[BuildInfoMetricName]
	if mf == nil {
		t.Fatalf("%s missing", BuildInfoMetricName)
	}
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		if lp.GetName() == "service" && lp.GetValue() != "from-env" {
			t.Errorf("service label = %q, want %q (OTEL_SERVICE_NAME)", lp.GetValue(), "from-env")
		}
	}
}

func TestSetupRejectsBadLogLevel(t *testing.T) {
	clearOTLPEnv(t)
	cfg := testConfig()
	cfg.LogLevel = "loud"
	if _, err := Setup(context.Background(), cfg); err == nil {
		t.Fatal("Setup accepted an invalid log level; want error")
	}
}
