// Package obs wires the three telemetry signals a service needs and hands back
// the handful of objects the rest of the process uses.
//
// # Shape
//
//	p, err := obs.Setup(ctx, obs.Config{ServiceName: "rom", ServiceVersion: v})
//	defer p.Shutdown(ctx)   // in practice: lifecycle.Spec.Flush, run LAST
//
//	p.Tracer         start spans
//	p.Meter          record application metrics
//	p.PromRegistry   serve at /metrics on the ADMIN listener
//	p.Logger         slog JSON on stdout, promtail scrapes it into Loki
//
// # Traces
//
// OTLP over gRPC, configured entirely by OpenTelemetry's own environment
// variables (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_TRACES_ENDPOINT,
// OTEL_EXPORTER_OTLP_HEADERS, OTEL_TRACES_SAMPLER, ...). None of those are
// re-invented as config fields. When no endpoint variable is set the exporter
// is not created at all: spans are started and dropped, nothing dials, nothing
// retries, nothing logs. That is the default in a cluster with no trace
// backend, and it must stay cheap and silent.
//
// # Metrics
//
// An OTel meter feeds a Prometheus exporter that registers into a
// *prometheus.Registry the caller serves. The Prometheus bridge does NOT give
// you the classic go_* collectors, so contrib/instrumentation/runtime is
// registered explicitly -- goroutine-leak-to-OOMKill is the most common Go
// failure mode and it is unobservable without those series. A build-info gauge
// is registered too, so "which version is actually running" is answerable from
// Prometheus across a rollback.
//
// # Logging hygiene (policy, not a suggestion)
//
// Downstream services hold household inventory: what a family owns, where it
// is, and what it cost. That is sensitive, and Loki is not the place for it.
// The rule is:
//
//   - Request bodies, query strings, and user-supplied field values are NEVER
//     logged at info level. Not truncated, not "just the first 200 bytes".
//   - Log identifiers and shapes instead: the route TEMPLATE (/widgets/{id},
//     never the filled-in path), a record id, a count, a duration, an error
//     class.
//   - When a sensitive value must appear for correlation, pass it through
//     [Redact], which emits a stable non-reversible token.
//   - The handler enforces a deny list ([DefaultRedactKeys]) as a backstop, so
//     one careless call site does not become a data leak. The backstop is not
//     an excuse to rely on it.
//   - Debug level may carry more, and debug level is off in production.
//
// # Semconv churn
//
// The semantic-conventions import is pinned in THIS FILE AND NOWHERE ELSE in
// the kit. An OTel bump can rename attributes and metrics; keeping exactly one
// import site makes that a one-line change plus a dashboard review, instead of
// a scavenger hunt. Renovate groups all go.opentelemetry.io/* modules so they
// move in lockstep.
package obs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/otlptranslator"
	runtimeinst "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	// SEMCONV PIN. This is the ONE semconv import site in the kit. Bumping the
	// version here is the whole migration; do not import semconv elsewhere.
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// ScopeName is the instrumentation scope for the kit's own telemetry.
const ScopeName = "github.com/leftathome/go-service-kit/obs"

// BuildInfoMetricName is the build-info gauge exported on /metrics. It is
// always 1; the information is in the labels (service, version, environment,
// go_version, revision). Query it to answer "what is actually running right
// now", which is the question a rollback makes urgent.
const BuildInfoMetricName = "service_build_info"

// RuntimeMetricNames are the EXACT Prometheus metric family names that
// contrib/instrumentation/runtime v0.69.0 exports through the OTel Prometheus
// exporter v0.66.0, as observed by TestRuntimeMetricNames on this exact
// dependency set.
//
// These are OTel-namespaced names produced by the UnderscoreEscapingWithSuffixes
// translation strategy. They are NOT the classic client_golang collector names:
// there is no go_goroutines, no go_memstats_alloc_bytes, no go_gc_duration_seconds.
// Grafana panels must be written against the list below or they render empty.
//
//	go_config_gogc_percent          gauge      GOGC value (100 unless tuned)
//	go_goroutine_count              gauge      live goroutines -- the leak canary
//	go_memory_allocated_bytes_total counter    cumulative heap bytes allocated
//	go_memory_allocations_total     counter    cumulative heap objects allocated
//	go_memory_gc_goal_bytes         gauge      heap size target for the next GC
//	go_memory_used_bytes            gauge      memory in use, split by go_memory_type
//	go_processor_limit              gauge      GOMAXPROCS (no unit suffix: the
//	                                           OTel unit is "{thread}", which
//	                                           the translator does not suffix)
//	go_schedule_duration_seconds    histogram  goroutine runnable-to-running delay
//
// The rest of what /metrics carries out of the box:
//
//	go_memory_limit_bytes    gauge  ONLY when GOMEMLIMIT is set; not asserted
//	go_build_info            gauge  main module path and version
//	service_build_info       gauge  see BuildInfoMetricName
//	target_info              gauge  resource attributes: service_name,
//	                                service_version, deployment_environment_name,
//	                                telemetry_sdk_*
//
// Label notes for whoever writes the dashboard:
//   - go_memory_used_bytes is split by a go_memory_type label ("stack",
//     "other"); sum without it for a total, or select go_memory_type="stack".
//   - Every runtime series also carries otel_scope_name, otel_scope_version,
//     and otel_scope_schema_url labels from the exporter's scope-info
//     behaviour. Ignore them in queries or they will fragment aggregations.
//   - There is no GC-pause metric in the new runtime instrumentation. Use
//     go_schedule_duration_seconds for scheduling latency; pause time only
//     exists in the deprecated set (OTEL_GO_X_DEPRECATED_RUNTIME_METRICS=true),
//     which is deliberately not enabled here.
//   - service.name is NOT a label on the runtime series. Template the dashboard
//     on the target's job/service label from the ServiceMonitor, or join
//     against target_info.
var RuntimeMetricNames = []string{
	"go_config_gogc_percent",
	"go_goroutine_count",
	"go_memory_allocated_bytes_total",
	"go_memory_allocations_total",
	"go_memory_gc_goal_bytes",
	"go_memory_used_bytes",
	"go_processor_limit",
	"go_schedule_duration_seconds",
}

// defaultShutdownTimeout bounds the final flush. It must fit inside the pod's
// terminationGracePeriodSeconds alongside the propagation delay and the drain.
const defaultShutdownTimeout = 5 * time.Second

// Config is the observability configuration. Every field has a working
// default; the zero value is a valid, silent, no-network setup.
//
// Anything OpenTelemetry already defines an environment variable for is
// deliberately absent here (endpoint, headers, timeout, sampler, resource
// attributes). Read those from the environment; the SDK does.
type Config struct {
	// ServiceName defaults to $OTEL_SERVICE_NAME, then "unknown_service".
	ServiceName string
	// ServiceVersion should be the build version stamped by the linker.
	ServiceVersion string
	// ServiceInstanceID is usually the pod name. Empty means unset.
	ServiceInstanceID string
	// Environment maps to deployment.environment.name.
	Environment string

	// LogLevel is one of debug, info, warn, error. Empty means info.
	LogLevel string
	// LogOutput defaults to os.Stdout (promtail -> Loki).
	LogOutput io.Writer
	// LogAddSource adds file:line to every record.
	LogAddSource bool
	// RedactKeys overrides DefaultRedactKeys. Prefer appending to it.
	RedactKeys []string

	// ShutdownTimeout bounds the final flush. Default 5s.
	ShutdownTimeout time.Duration

	// SpanExporter overrides the OTLP exporter. This exists for tests and for
	// a service that must ship spans somewhere unusual; leave it nil in
	// production and let OTEL_EXPORTER_OTLP_ENDPOINT decide.
	SpanExporter sdktrace.SpanExporter
}

// Providers is what the rest of the process consumes.
type Providers struct {
	Tracer       trace.Tracer
	Meter        metric.Meter
	PromRegistry *prometheus.Registry
	Logger       *slog.Logger

	// Shutdown flushes traces and metrics under a bounded context. It is safe
	// to call more than once; later calls repeat the first result. Run it LAST
	// in the shutdown sequence, after the request drain, or every deploy drops
	// the final requests' telemetry.
	Shutdown func(context.Context) error

	// TracerProvider and MeterProvider are exposed for instrumentation
	// libraries that take a provider rather than a tracer or meter (otelhttp,
	// for one). The globals are also set, so libraries that reach for the
	// global get the same thing.
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider

	// TraceExportEnabled reports whether spans actually leave the process. It
	// is false in the default, no-collector-configured deployment.
	TraceExportEnabled bool
}

// Setup builds the providers. It performs no network I/O: the OTLP gRPC client
// connects lazily, so an unreachable collector degrades to dropped spans rather
// than a failed start.
func Setup(ctx context.Context, cfg Config) (*Providers, error) {
	name := strings.TrimSpace(cfg.ServiceName)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	}
	if name == "" {
		name = "unknown_service"
	}

	level, err := ParseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	logger := NewLogger(LoggerOptions{
		Level:          level,
		Output:         cfg.LogOutput,
		ServiceName:    name,
		ServiceVersion: cfg.ServiceVersion,
		RedactKeys:     cfg.RedactKeys,
		AddSource:      cfg.LogAddSource,
	})

	res := buildResource(ctx, logger, name, cfg)

	tp, exportEnabled, err := newTracerProvider(ctx, res, cfg)
	if err != nil {
		return nil, err
	}

	reg := prometheus.NewRegistry()
	mp, err := newMeterProvider(res, reg)
	if err != nil {
		// Undo the tracer provider so a partial Setup leaves nothing running.
		_ = tp.Shutdown(ctx)
		return nil, err
	}
	if err := registerBuildInfo(reg, name, cfg); err != nil {
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		return nil, err
	}

	// Globals, so instrumentation libraries that reach for them agree with the
	// providers handed back here. W3C tracecontext plus baggage is the wire
	// format everything in the cluster speaks.
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	// Exporter failures are warnings, not crashes, and they must not go to
	// stderr unstructured.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("otel error", slog.String("error", err.Error()))
	}))

	timeout := cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}

	var (
		once     sync.Once
		shutErr  error
		shutdown = func(parent context.Context) error {
			once.Do(func() {
				// Bounded, and independent of whether the parent is already
				// cancelled: SIGTERM handling routinely hands us a dying
				// context, and the flush still has a grace budget to use.
				sctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
				defer cancel()
				shutErr = errors.Join(
					tp.ForceFlush(sctx),
					tp.Shutdown(sctx),
					mp.ForceFlush(sctx),
					mp.Shutdown(sctx),
				)
			})
			return shutErr
		}
	)

	return &Providers{
		Tracer:             tp.Tracer(name),
		Meter:              mp.Meter(name),
		PromRegistry:       reg,
		Logger:             logger,
		Shutdown:           shutdown,
		TracerProvider:     tp,
		MeterProvider:      mp,
		TraceExportEnabled: exportEnabled,
	}, nil
}

// buildResource merges the SDK defaults (which already read OTEL_SERVICE_NAME
// and OTEL_RESOURCE_ATTRIBUTES) with the explicitly configured identity, which
// wins.
func buildResource(_ context.Context, logger *slog.Logger, name string, cfg Config) *resource.Resource {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(name),
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.ServiceInstanceID != "" {
		attrs = append(attrs, semconv.ServiceInstanceIDKey.String(cfg.ServiceInstanceID))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentNameKey.String(cfg.Environment))
	}
	own := resource.NewWithAttributes(semconv.SchemaURL, attrs...)

	merged, err := resource.Merge(resource.Default(), own)
	if err != nil {
		// A schema-URL mismatch after an OTel bump lands here. Keep the
		// service identity rather than the SDK defaults, and say so loudly.
		logger.Warn("obs: resource merge failed, using explicit attributes only",
			slog.String("error", err.Error()))
		return own
	}
	return merged
}

// hasOTLPEndpoint reports whether OpenTelemetry has been told where to send
// traces. Only OTel's own variables are consulted.
func hasOTLPEndpoint() bool {
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
	} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

func newTracerProvider(ctx context.Context, res *resource.Resource, cfg Config) (*sdktrace.TracerProvider, bool, error) {
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}

	exp := cfg.SpanExporter
	if exp == nil && hasOTLPEndpoint() {
		// No options: everything (endpoint, TLS, headers, compression,
		// timeout, retry) comes from OTEL_EXPORTER_OTLP_* by design. The gRPC
		// client is lazy, so this does not dial here.
		otlpExp, err := otlptracegrpc.New(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("obs: otlp trace exporter: %w", err)
		}
		exp = otlpExp
	}

	if exp != nil {
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	// With no exporter the provider still creates real span contexts (so log
	// correlation works) and simply has nowhere to send them.
	return sdktrace.NewTracerProvider(opts...), exp != nil, nil
}

func newMeterProvider(res *resource.Resource, reg *prometheus.Registry) (*sdkmetric.MeterProvider, error) {
	exp, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		// Pin the naming strategy. The exporter's default is documented as
		// liable to change, and the metric names in RuntimeMetricNames (and
		// every dashboard written against them) depend on this exact choice.
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
		// go.schedule.duration is a precomputed runtime histogram and only
		// arrives through this producer.
		otelprom.WithProducer(runtimeinst.NewProducer()),
	)
	if err != nil {
		return nil, fmt.Errorf("obs: prometheus exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(exp),
	)
	if err := runtimeinst.Start(runtimeinst.WithMeterProvider(mp)); err != nil {
		_ = mp.Shutdown(context.Background())
		return nil, fmt.Errorf("obs: runtime instrumentation: %w", err)
	}
	return mp, nil
}

// registerBuildInfo adds go_build_info (module graph) and service_build_info
// (our own identity). One series each; no per-request labels ever go here.
func registerBuildInfo(reg *prometheus.Registry, name string, cfg Config) error {
	if err := reg.Register(collectors.NewBuildInfoCollector()); err != nil {
		return fmt.Errorf("obs: build info collector: %w", err)
	}
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: BuildInfoMetricName,
		Help: "Always 1. Labels carry the identity of the running build.",
	}, []string{"service", "version", "environment", "go_version", "revision"})
	if err := reg.Register(g); err != nil {
		return fmt.Errorf("obs: %s: %w", BuildInfoMetricName, err)
	}
	goVersion, revision := buildStamp()
	g.WithLabelValues(name, cfg.ServiceVersion, cfg.Environment, goVersion, revision).Set(1)
	return nil
}

func buildStamp() (goVersion, revision string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", "unknown"
	}
	goVersion, revision = bi.GoVersion, "unknown"
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			revision = s.Value
		}
	}
	return goVersion, revision
}
