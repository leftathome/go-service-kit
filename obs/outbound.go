package obs

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/leftathome/go-service-kit/outbound"
)

// OutboundMetricNames are the EXACT Prometheus metric family names the bridge
// built by [OutboundMetrics] exports, as asserted by TestOutboundMetricNames on
// this dependency set. Write dashboards and alerts against this list.
//
//	outbound_calls_total            counter    one per ATTEMPT, so a retried
//	                                           call counts more than once --
//	                                           that is what the remote saw
//	outbound_call_duration_seconds  histogram  round-trip time per attempt
//	outbound_limiter_delay_seconds  histogram  time parked on the rate limiter;
//	                                           a rising p99 here means the
//	                                           configured RPS is the bottleneck,
//	                                           not the remote
//	outbound_budget_remaining       gauge      what is left of the call budget;
//	                                           absent entirely for an unmetered
//	                                           client
//
// Labels, and why there are so few of them:
//
//   - host, method and status are on the call counter and the duration
//     histogram. All three are bounded by configuration or by protocol.
//   - status is the HTTP status code, with 0 meaning the attempt failed before
//     any response -- DNS, dial, TLS, timeout. Alert on
//     status="0" separately from 5xx: they fail for different reasons and get
//     fixed by different people.
//   - limiter delay and budget carry host only. The limiter is per client, and
//     splitting its delay by status would invite reading pacing as an error
//     rate.
//   - The ATTEMPT NUMBER is deliberately not a label. It is unbounded in
//     principle and the retry rate is already derivable from the counter.
//
// Every series also carries the otel_scope_* labels the Prometheus exporter
// attaches. Ignore them in queries or they fragment aggregations.
//
// This bridge exists because outbound.Metrics was documented as "the seam the
// observability package wires into" while obs shipped no implementation of it.
// Every service was going to hand-roll an adapter and every service was going
// to pick different names, which is precisely the divergence the kit exists to
// prevent. nagus was the first to hit it.
var OutboundMetricNames = []string{
	"outbound_calls_total",
	"outbound_call_duration_seconds",
	"outbound_limiter_delay_seconds",
	"outbound_budget_remaining",
}

// Instrument names and units. The Prometheus family names in
// OutboundMetricNames are DERIVED from these by the exporter's
// UnderscoreEscapingWithSuffixes translation: dots become underscores, a
// counter gains _total, and the unit "s" becomes the _seconds suffix. The
// "{call}" unit is an OTel annotation, which the translator does not suffix --
// that is how outbound_calls_total avoids becoming outbound_calls_calls_total.
//
// Do not rename these to match the Prometheus names. Naming a counter
// "outbound_calls_total" would export outbound_calls_total_total.
const (
	outboundCallsInstrument         = "outbound.calls"
	outboundDurationInstrument      = "outbound.call.duration"
	outboundLimiterInstrument       = "outbound.limiter.delay"
	outboundBudgetInstrument        = "outbound.budget.remaining"
	outboundCallUnit                = "{call}"
	outboundSecondsUnit             = "s"
	outboundAttrHost                = "host"
	outboundAttrMethod              = "method"
	outboundAttrStatus              = "status"
	outboundUnmeteredBudgetSentinel = -1
)

// OutboundMetrics builds the bridge from [outbound.CallEvent] to OTel
// instruments with the stable names documented on [OutboundMetricNames].
//
// Attach it at construction time and the egress client reports itself:
//
//	m, err := obsp.OutboundMetrics()
//	c, err := outbound.New(outbound.Config{..., Metrics: m})
//
// One client per integration gives one host per client, so the series stay
// separable per remote without any per-request labelling.
func OutboundMetrics(meter metric.Meter) (outbound.Metrics, error) {
	if meter == nil {
		return nil, errors.New("obs: OutboundMetrics requires a meter")
	}
	calls, err := meter.Int64Counter(outboundCallsInstrument,
		metric.WithUnit(outboundCallUnit),
		metric.WithDescription("Outbound HTTP attempts, one per attempt rather than per logical call."))
	if err != nil {
		return nil, fmt.Errorf("obs: %s: %w", outboundCallsInstrument, err)
	}
	duration, err := meter.Float64Histogram(outboundDurationInstrument,
		metric.WithUnit(outboundSecondsUnit),
		metric.WithDescription("Round-trip duration of one outbound HTTP attempt."))
	if err != nil {
		return nil, fmt.Errorf("obs: %s: %w", outboundDurationInstrument, err)
	}
	limiter, err := meter.Float64Histogram(outboundLimiterInstrument,
		metric.WithUnit(outboundSecondsUnit),
		metric.WithDescription("Time an outbound attempt spent waiting on the client's rate limiter."))
	if err != nil {
		return nil, fmt.Errorf("obs: %s: %w", outboundLimiterInstrument, err)
	}
	budget, err := meter.Int64Gauge(outboundBudgetInstrument,
		metric.WithUnit(outboundCallUnit),
		metric.WithDescription("Calls remaining in the outbound client's budget window."))
	if err != nil {
		return nil, fmt.Errorf("obs: %s: %w", outboundBudgetInstrument, err)
	}
	return &outboundMetrics{calls: calls, duration: duration, limiter: limiter, budget: budget}, nil
}

// OutboundMetrics builds the bridge on this Providers' meter. See the package
// function of the same name.
func (p *Providers) OutboundMetrics() (outbound.Metrics, error) {
	return OutboundMetrics(p.Meter)
}

type outboundMetrics struct {
	calls    metric.Int64Counter
	duration metric.Float64Histogram
	limiter  metric.Float64Histogram
	budget   metric.Int64Gauge
}

// RecordCall implements outbound.Metrics.
//
// The context is context.Background rather than the caller's:
// outbound.CallEvent carries no context, so there is no span to attach an
// exemplar to. Widening the outbound seam to pass one would make that package
// depend on a tracing concept it deliberately does not have.
func (m *outboundMetrics) RecordCall(ev outbound.CallEvent) {
	ctx := context.Background()
	host := attribute.String(outboundAttrHost, ev.Host)
	call := metric.WithAttributes(
		host,
		attribute.String(outboundAttrMethod, ev.Method),
		attribute.Int(outboundAttrStatus, ev.StatusCode),
	)
	byHost := metric.WithAttributes(host)

	m.calls.Add(ctx, 1, call)
	m.duration.Record(ctx, ev.Duration.Seconds(), call)
	m.limiter.Record(ctx, ev.LimiterDelay.Seconds(), byHost)
	if ev.BudgetRemaining > outboundUnmeteredBudgetSentinel {
		// An unmetered client reports -1. Recording that would put a fake
		// "minus one calls left" on a dashboard next to real ones.
		m.budget.Record(ctx, ev.BudgetRemaining, byHost)
	}
}
