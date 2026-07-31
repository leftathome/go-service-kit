# go-service-kit

Importable Go library for services in this homelab. Sibling to
[`go-service-template`](https://gitlab.orac.local/homelab/go-service-template),
which scaffolds a service that imports this kit.

**Canonical module path: `github.com/leftathome/go-service-kit`** (public).
This GitLab repo is the dev remote; the module resolves through
`proxy.golang.org` so that contributors can build without homelab access.

## Packages

| Package | Responsibility |
| --- | --- |
| `obs` | OTel wiring: traces (OTLP), metrics (Prometheus exporter + runtime instrumentation), `slog` JSON to stdout with `trace_id`/`span_id` injection. One `Setup`, one `Shutdown`. |
| `httpapi` | huma v2 over stdlib `net/http`. OpenAPI 3.1 derived from Go types, served with a docs UI. Two listeners: public API, and an admin port for health/metrics/pprof. Mandatory server timeouts. |
| `lifecycle` | Signal handling and graceful shutdown ordering: readiness off, propagation delay, drain, then flush telemetry. |
| `config` | Env-driven config loading with aggregated validation errors. |
| `outbound` | The egress trust boundary: rate limiting, identifying User-Agent, `Retry-After`, backoff with jitter, mandatory timeouts, SSRF guard, redirect header hygiene. |
| `storekit` | Generic contract-test harness that any store adapter must satisfy. |

## Status

Design approved, implementation not started. See the design spec in
[`go-service-template`](https://gitlab.orac.local/homelab/go-service-template)
at `docs/superpowers/specs/2026-07-28-go-service-template-design.md`.

## License

Apache 2.0.
