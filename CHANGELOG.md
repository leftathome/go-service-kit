# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Ergonomic gaps found by nagus, the kit's first real consumer, while planning
its migration. Everything below is additive at the API level: v0.1.2 code keeps
compiling. There is exactly one behaviour change -- `obs.DefaultRedactKeys` --
and it has its own section.

### Added

- **`httpapi`: a single-port TRANSITION MODE.** `AdminHandlers(AdminOptions)
  map[string]http.Handler`, `(*Admin).Handlers()` and `(*Admin).RegisterOn(mux)`
  mount the admin routes on an existing mux -- including the API listener's --
  so a live service can serve them on both ports for one release instead of
  moving the chart's probes and the binary in one unrevertible step. The keys
  are method-qualified ServeMux patterns, so mounting them elsewhere keeps the
  405-on-wrong-method strictness a hand-rolled `/healthz` loses. Both copies
  share one `*Readiness` and one registry.

  `Options.InstrumentationFilter`, with the supplied `ExceptAdminPaths`, keeps
  kubelet probes and Prometheus scrapes out of the API listener's RED metrics
  while they are there.

  **Two listeners remain the destination.** Single-port is a migration
  affordance with a deadline, documented as such on every new symbol: `/metrics`
  is a live inventory of the process, pprof hands out heap dumps, and the
  OpenAPI document maps every route and type -- none of them belong on the one
  port that could end up behind an Ingress.

- **`httpapi`: configurable server timeouts, with the invariant intact.**
  `Options.Timeouts` / `AdminOptions.Timeouts` (type `Timeouts`). A fixed 30s
  `WriteTimeout` truncates the body of a slow fan-out READ mid-JSON rather than
  returning an error. **Zero means the documented DEFAULT, never net/http's "no
  timeout", and a negative value is rejected by `Timeouts.Validate` and panics
  at construction**, so a timeout-less server is still unconstructable.
  `DefaultReadHeaderTimeout`, `DefaultReadTimeout`, `DefaultWriteTimeout`,
  `DefaultIdleTimeout` and `DefaultMaxHeaderBytes` are now exported.

- **`httpapi`: raw routes, for MCP and other non-REST surfaces.**
  `(*API).RawRoute`, `RawRouteFunc` and `RawRoutes` make the "handler that
  cannot be a huma operation" pattern first-class: method-qualified,
  instrumented, `http_route`-labelled, and absent from the OpenAPI document.
  The package doc states the honest caveat -- a raw route is an EXCEPTION to
  one-error-format-per-listener, because a JSON-RPC error is not problem+json.
  `RawRoutes()` is the opt-out list, so a test can assert it has not grown.

- **`lifecycle`: a third worker stance, restart-with-backoff.**
  `Worker.OnFailure = RestartWithBackoff` plus `Worker.Backoff` (`Initial`,
  `Max`, `Factor`, `Jitter`, `ResetAfter`, `MaxRestarts`). The previous two
  stances -- abort silently, or crash the process -- do not fit N independent
  pollers needing per-source failure isolation, which forced such a service to
  swallow every error and made a wedged loop invisible. Restarts are loud: an
  ERROR log with the worker, the error, the consecutive count and the next
  delay, plus a `lifecycle_worker_restarts_total{worker}` counter
  (`Spec.MeterProvider`, defaulting to the OTel global that `obs.Setup`
  installs). `CrashProcess` is the zero value, so existing workers are
  unaffected. New: `RestartPolicy`, `Backoff`, `ErrWorkerRestartsExhausted`,
  `ScopeName`, `WorkerRestartsInstrumentName`, `WorkerRestartsMetricName` and
  the `DefaultRestart*` constants.

- **`storekit`: `Pinger`, and clause-level adoption.** `Pinger` is the readiness
  clause on its own, and `PingHarness` gates it with no type parameters and no
  fixtures. `Store` is unchanged as a method set but is now composed from
  `Putter`, `Getter`, `Lister`, `Deleter` and `Pinger`. `Harness.NewAdapter`
  (type `Adapter`, plus `AdapterFor`) declares operations one at a time, and
  `Harness.RunClauses(t, names...)` with the `Clause*` constants and `Clauses()`
  runs a subset. `Run` now skips a clause the adapter cannot support, naming the
  missing operation; `RunClauses` fails on one, because a clause you asked for
  by name and silently did not run is worse than one you never asked for.

- **`config`: `float64` and friends in `Load[T]`.** A HARD BLOCKER, not an
  ergonomic one: nagus could not adopt the package at all, because
  `NAGUS_MIN_CAPACITY` and `NAGUS_LAND_{MIN,MAX}_ACREAGE` are fractional. Any
  service with a threshold, a ratio or a unit measurement hits it. Now
  supported: `float32`/`float64`, the remaining signed widths, all unsigned
  widths, `time.Time` as RFC 3339, and slices of any supported scalar
  (`[]int`, `[]float64`, `[]time.Duration`).

  **NaN and the infinities are refused.** They parse cleanly in `strconv` and
  then make every comparison against them false: a NaN minimum capacity matches
  nothing while the service reports healthy.

  `[]byte` and `[][]T` stay unsupported deliberately -- `[]byte` is `[]uint8`,
  which the numeric path would read as a list of small integers -- as do maps,
  for which no obvious textual form exists. Slice element errors report the
  INDEX, never the element text, so a credential-named variable's redaction
  still holds. Fixed in passing: a named string-slice type (`type Hosts
  []string`) panicked in `reflect.Value.Set`.

- **`outbound`: three shapes over one policy.** `*outbound.Client` was not an
  `*http.Client`, did not implement `http.RoundTripper`, and the package
  exported no interface to type a field as -- so adopting it meant editing all
  six of nagus's integrations, and any integration that skipped the edit kept
  `http.DefaultClient` with no timeout at all.

  `Doer` is the one-method interface both `*http.Client` and `*outbound.Client`
  satisfy. `NewTransport` returns the whole policy as an `http.RoundTripper`
  (`hc.Transport = tr`). `NewHTTPClient` returns a configured `*http.Client`.
  `(*Client).Transport()` and `(*Client).HTTPClient()` hand out the other two
  shapes backed by the SAME limiter and budget, so call sites can be converted
  one at a time without running two quotas against one remote.

  **The security properties hold on every path**, which took work, because a
  RoundTripper sits below the layer that normally provides two of them. The
  per-attempt timeout is a context deadline whose cancel is deferred to the
  response body's `Close`, giving the same headers-and-body coverage
  `http.Client.Timeout` does. Redirect hygiene is applied by the transport
  itself, detecting a redirect follow through `Request.Response`, so the
  cross-origin auth-header strip and the `MaxRedirects` cap survive being
  attached to a bare `&http.Client{}` -- whose own policy forwards `X-Api-Key`
  across hosts and caps hops at 10. Tested that way, deliberately.

- **`outbound`: windowed, resettable, server-reportable call budgets.**
  `CallBudget` was a process-lifetime `atomic.Int64`. Real quotas are windowed
  (eBay Browse: ~5,000 calls per UTC DAY, not per 24h of pod uptime) and are
  often reported by the server, so nagus could not retire its own budget code
  in favour of the kit's -- the very code this package's doc cites as its
  precedent.

  New: the `Budget` interface (`Reserve`/`Observe`/`Reset`/`Stats`),
  `WindowedBudget`, `BudgetConfig`, `BudgetStats`, `Config.Budget`,
  `(*Client).Budget()` and `(*Client).BudgetRemaining()` -- the last filling a
  gap where the remaining count was observable only as a side effect on
  `CallEvent`. A rolling window TUMBLES from its original start, so a poller
  waking every 61 minutes on an hourly budget does not drift. `Calendar: true`
  aligns to UTC boundaries, which at 24h is exactly "per UTC day".
  `Observe(remaining)` is authoritative DOWNWARD only: a generous server report
  never widens a ceiling the service chose, and a negative one means none left.
  `Config.CallBudget` is unchanged and is now defined as
  `BudgetConfig{Limit: N}` with no window.

- **`obs`: the `outbound.Metrics` bridge.** `outbound.Metrics` was documented as
  "the seam the observability package wires into" and `obs` shipped no
  implementation, so every service was going to hand-roll one and pick
  different names. `OutboundMetrics(meter)` and `(*Providers).OutboundMetrics()`
  build it; `OutboundMetricNames` records the family names the way
  `RuntimeMetricNames` already does:
  `outbound_calls_total{host,method,status}`,
  `outbound_call_duration_seconds`, `outbound_limiter_delay_seconds{host}`,
  `outbound_budget_remaining{host}`. `status="0"` is a transport failure rather
  than an HTTP status. The import direction is `obs -> outbound`, never the
  reverse.

- **`obs`: subtractive redaction.** `CredentialRedactKeys`, `PIIRedactKeys` and
  `RequestMaterialRedactKeys` are the three parts `DefaultRedactKeys` is now
  composed from, and `RedactKeysExcept(...)` returns a copy with named keys
  removed. It PANICS if asked to uncover a credential key: that is a
  programming error detectable only at the call, and a process that started
  anyway would log credentials for as long as it ran. A service with a
  genuinely narrower need composes one from the three parts instead.

### Changed

- **`obs.DefaultRedactKeys` no longer redacts `query`, `search`, `lat` or
  `lon`.** This is a BEHAVIOUR CHANGE. Those are domain vocabulary, not
  credentials: nagus is an acquisition service where `query` is the operator's
  own eBay source configuration and lat/lon are the entire subject of its land
  category, so the fleet default turned every useful debug line into a hash
  while adding nothing to safety. `query_string`, `url.query`, `raw_url` and the
  new `raw_query` stay in, because credentials routinely ride in a raw query
  string -- it is the bare domain words that left. `client_secret`,
  `signature`, `ssn` and `date_of_birth` were added while revisiting the list.

  **A service relying on the default to mask a field named exactly `query`,
  `search`, `lat` or `lon` must now name it**, e.g.
  `append(slices.Clone(obs.DefaultRedactKeys), "query")`. Nothing stops
  compiling.

### Documented

- **`lifecycle`: the propagation delay is pure downtime under `replicas: 1` +
  `strategy: Recreate`.** The 4s wait assumes another replica takes the traffic.
  With one replica and Recreate there is none, so it adds 4s to every deploy's
  outage and spares no client anything. New package-doc section, a paragraph on
  `Spec.PropagationDelay`, and `NoPropagationDelay` as a greppable name for the
  existing negative-disables behaviour. **The default is unchanged**: it is
  correct for the multi-replica case.

- **`httpapi`: how a non-REST surface coexists with huma on the API listener**,
  including why it cannot honour the one-error-format invariant and how to keep
  the exception bounded.

- **`storekit`: how to adopt a SUBSET of the contract**, and why a store with a
  richer read path than `List` should not bend a live query to produce the total
  order the pagination clauses require.

- **`obs`: how to preserve an existing metric family name across a migration.**
  `Providers.PromRegistry` was the right escape hatch and nothing said WHEN to
  use it. A service porting an existing `/metrics` cannot re-express its
  metrics as OTel instruments: the pinned `UnderscoreEscapingWithSuffixes`
  strategy escapes dots and appends `_total` and unit suffixes, so the old
  series goes stale, the renamed one starts at zero, and every alert written
  against the old name evaluates to nothing without erroring -- nobody notices
  until the alert that should have fired does not. The package doc now carries
  the exact translation rules, the native `prometheus.Collector` recipe, the
  advice to collect at SCRAPE TIME for a derived value, and the
  `testutil.CollectAndCompare` gate. It is verified rather than asserted: a
  test registers a native collector and an OTel counter of the same name and
  proves the first keeps its name while the second is renamed. nagus must keep
  `nagus_ebay_api_calls_*` across its migration or lose the only operational
  signal it has, and working this out required reading `newMeterProvider`.

- **`obs`: the deny list is a fleet DEFAULT, not a fleet law.** The logging
  hygiene section now says so, and points at `RedactKeysExcept` and the three
  component lists, with the standing instruction to write the reason for any
  removal at the call site.

- **`outbound`: the terms-of-service example now uses a calendar budget** and
  `MaxAttempts: 1`, because a metered API is the wrong place for a silent retry
  -- the quota is charged per attempt, not per logical call -- and shows
  feeding `X-RateLimit-Remaining` back through `Observe`.

## [0.1.2] - 2026-08-02

### Security

- **Bumped `google.golang.org/grpc` v1.81.1 -> v1.82.1** for GO-2026-6061.
  The dependency arrives via the OTLP trace exporter, so every service built on
  the kit inherited it. `govulncheck` now reports no vulnerabilities.

### Fixed

- **`make lint` assumed `golangci-lint` was on PATH.** It is not on a stock CI
  runner, so this repository's GitHub Actions workflow was red from its first
  commit -- `make: golangci-lint: No such file or directory` -- while every
  local run looked green, because this machine has the binary installed. The
  target now prefers a binary on PATH and falls back to a pinned
  `go run` invocation, matching go-service-template. Both CIs call `make lint`,
  so the resolution belongs in one place.
- `govulncheck` was invoked as `@latest`, which makes the gate
  non-reproducible: a build could start failing because a tool moved rather
  than because the code changed. Pinned to v1.6.0.

## [0.1.1] - 2026-08-02

### Fixed

- **`go.mod` declared `go 1.26.5`, which the standard `golang:1.26` CI image
  cannot satisfy.** That image ships Go 1.26.4, and with `GOTOOLCHAIN=local` the
  build fails outright rather than upgrading:
  `go: go.mod requires go >= 1.26.5 (running go 1.26.4)`. v0.1.0 was therefore
  unbuildable in the homelab pipeline. The directive is now `go 1.26.0` -- the
  language version, not whichever patch release happened to be on the machine
  that ran `go mod init`.

  Raising `GOTOOLCHAIN` instead would have been the wrong fix: it makes every
  build download a Go toolchain, and this build path is behind a residential
  uplink where that is exactly the kind of pull that hangs a pipeline.

  Found by the first real pipeline run of a service generated from
  go-service-template. No API change; v0.1.0 and v0.1.1 are source-identical
  apart from the directive.

## [0.1.0] - 2026-08-01

First release. Six packages, all tested; `go vet`, `go test -race`, and
`golangci-lint` are clean across the module.

### Added

- **`config`** -- env-driven configuration via `Load[T]()`. Validation errors are
  aggregated with `errors.Join`, so N bad fields produce N reported errors rather
  than only the first. Walks nested structs, so a service config can embed
  `obs.Config` and load everything in one call. Env vars whose names contain
  SECRET/PASSWORD/TOKEN/KEY/CREDENTIAL never echo their value into error text.
  Supported field types: `string`, `int`, `int64`, `bool`, `time.Duration`,
  `[]string`.

- **`obs`** -- OpenTelemetry wiring for all three signals behind one `Setup` and
  one `Shutdown`. Traces export over OTLP/gRPC and are a no-op unless
  `OTEL_EXPORTER_OTLP_ENDPOINT` is set. Metrics go through the Prometheus
  exporter and include Go runtime instrumentation plus a build-info series.
  Logs are `slog` JSON on stdout with `trace_id`/`span_id` injected from context.
  `Redact` and `DefaultRedactKeys` support a logging-hygiene policy. The
  `semconv` import is pinned in exactly one file so an OTel bump is a one-line
  change.

- **`httpapi`** -- huma v2 over stdlib `net/http`, with OpenAPI 3.1 derived from
  Go types so the spec cannot drift from the code. Two listeners: a public API
  port, and an admin port carrying health, metrics, and pprof. Server timeouts
  are mandatory -- a timeout-less server is unconstructable. Includes
  `Readiness` (registered dependency checks, shared with `lifecycle`), RFC 9457
  problem+json helpers, and a cursor-pagination envelope.

- **`lifecycle`** -- graceful shutdown in the order that actually works on
  Kubernetes: readiness flips to 503 immediately, the servers keep serving
  through a propagation delay while endpoints are withdrawn, then in-flight
  requests drain, and only then is telemetry flushed. Solved in-process because
  distroless images have no shell and `preStop: exec` is unavailable.
  `TerminationGracePeriodSeconds()` derives the value a chart must set.

- **`outbound`** -- the egress trust boundary. Rate limited by default,
  identifying User-Agent, honors `Retry-After`, exponential backoff with jitter,
  and mandatory timeouts. An SSRF guard resolves the host, refuses if any answer
  is private/loopback/link-local/CGNAT/ULA, and dials the checked IP literal so
  there is no check-to-connect window. Auth-bearing headers are stripped when a
  redirect changes origin, which Go's stdlib does not do for custom headers.

- **`storekit`** -- a generic contract-test harness that any store adapter must
  satisfy, parameterized over the service's own types. Pins the semantics
  adapters would otherwise diverge on: a missing key is not an error, `Put` is an
  upsert, `Delete` is idempotent, and `List` order must be deterministic. `Ping`
  is part of the contract so `/readyz` can be a real dependency check.

### Notes

- The docs UI is a dependency-free embedded renderer, not Stoplight Elements.
  Every huma built-in renderer hardcodes a `unpkg.com` script tag, and vendoring
  megabytes of third-party minified JS into a library -- and therefore into every
  binary built on it -- was the worse trade. The page works air-gapped under
  `default-src 'none'`.
- Verified behavior of huma v2.39.0 that differs from common assumption:
  validation failures are **422**, not 400; the problem+json `type` member is
  **absent** by default despite the OpenAPI schema advertising `about:blank`; and
  an unrouted 404 is stdlib plain text rather than problem+json.
- `otelhttp` emits **no request-count metric**. Rate and error rate come from
  `http_server_request_duration_seconds_count`.
- Go runtime metrics are OTel-namespaced and are **not** the classic collector
  names: `go_goroutine_count`, not `go_goroutines`; no `go_memstats_*`; and no
  GC-pause metric at all in the current instrumentation.

### Not included

Database adapters (per-service; a SQLite adapter must use a pure-Go driver such
as `modernc.org/sqlite` to stay CGO-free), authentication middleware beyond a
reserved slot, and an MCP surface.

[0.1.2]: https://github.com/leftathome/go-service-kit/releases/tag/v0.1.2
[0.1.1]: https://github.com/leftathome/go-service-kit/releases/tag/v0.1.1
[0.1.0]: https://github.com/leftathome/go-service-kit/releases/tag/v0.1.0
