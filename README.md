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

v0.1.0 -- all six packages implemented and tested. See the design spec in
[`go-service-template`](https://gitlab.orac.local/homelab/go-service-template)
at `docs/superpowers/specs/2026-07-28-go-service-template-design.md`.

## License

Apache 2.0.

## Install

```bash
go get github.com/leftathome/go-service-kit@v0.1.0
```

See [CHANGELOG.md](CHANGELOG.md) for what is in this release and the verified
behavior notes (huma returns 422 for validation failures; otelhttp emits no
request-count metric; Go runtime metrics are not the classic `go_*` names).
