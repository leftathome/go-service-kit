# ADR 0001: Whether and how the kit gains a SQLite adapter

- **Status:** Proposed -- the operator decides
- **Date:** 2026-09-24
- **Supersedes:** nothing
- **Superseded by:** nothing
- **Origin:** quark `.agent/tasks/QUARK-06-cross-repo-obligations.md`,
  item "go-service-kit: SQLite adapter (`modernc.org/sqlite`). Needs an ADR
  there -- it changes what moves into the kit."

This is the kit's first ADR. The numbering is `docs/adr/NNNN-slug.md`.

## Context

Every SQLite-backed store in the house uses `modernc.org/sqlite` (pure Go, no
cgo), for the same non-negotiable reason: release images are distroless static,
built with `CGO_ENABLED=0`, so a cgo driver either fails to build or fails to
start. There are three such adapters today, in two services, and they already
disagree about how to OPEN the database:

| Adapter | Pool | busy_timeout | journal_mode | BEGIN | Schema evolution |
| --- | --- | --- | --- | --- | --- |
| nagus `internal/store/sqlitestore` | 1 conn | post-open `Exec` | default (rollback) | DEFERRED | one idempotent `CREATE ... IF NOT EXISTS` |
| nagus `internal/offer/sqliteoffer` | 1 conn | post-open `Exec` | WAL, post-open `Exec` | DEFERRED | idempotent DDL + probe-then-`ALTER TABLE ADD COLUMN` |
| quark `internal/product/sqlite.go` | 1 conn | **DSN `_pragma`** | **WAL, DSN `_pragma`** | **`_txlock=immediate`** | idempotent DDL + a `schema_meta` key checked on open + backfill |

quark's column is the one that is right, and it got there by being bitten:

- **`busy_timeout` is per connection.** A post-open `Exec` applies it to
  whichever pooled connection it lands on; a `driver.ErrBadConn` replacement
  silently loses it. With a pool of one that is "usually fine", which is the
  worst kind of fine. The DSN form (`_pragma=busy_timeout(5000)`) applies it to
  every connection the driver ever opens.
- **DEFERRED transactions that read before they write fail under contention
  regardless of `busy_timeout`.** The read lock's upgrade to a write lock is
  refused with `SQLITE_BUSY` IMMEDIATELY -- SQLite will not wait, because
  waiting could deadlock. `_txlock=immediate` takes the write lock up front,
  which is what makes `busy_timeout` mean what it says. nagus's two adapters do
  not set it; they are protected only by the one-connection pool serialising
  everything inside one process.
- **Contention should be a typed error**, so a handler answers 503 rather than
  a 500 carrying a driver string. quark maps `SQLITE_BUSY`/`SQLITE_LOCKED` to a
  sentinel; nagus does not.
- **Schema evolution is ad hoc everywhere.** Three adapters, three approaches,
  none with a recorded schema version that a newer binary can refuse or an
  older one can detect.

That is the same pattern that justified `kit/mcp`: a second copy of hand-rolled
infrastructure that is already diverging from the first in the places that
matter. The question is what, if anything, should move into the kit.

What should NOT move is not in dispute. `storekit`'s package doc already
records why the kit has no universal `Store`: real stores are domain-shaped
(nagus's is Put/Get/Search/DeleteStale over FTS5; quark's is 1,400 lines of
identity resolution with merge redirects and skeleton columns). A generic
SQLite-backed `Store[T, K]` would be used by neither.

## Options

### A. Status quo: no SQLite code in the kit

Services keep their own adapters. The kit contributes only the `storekit`
harness. The quark lessons are written up (in this ADR, or a doc) for others to
copy.

- **For:** zero new dependency in the kit; nothing to version.
- **Against:** the divergence above is not hypothetical, it exists now, and
  the next service (gonk, glovebox, recognizer, anything the template
  scaffolds) will copy whichever adapter it finds first -- quite possibly
  nagus's, which has neither the DSN pragmas nor immediate transactions. A
  write-up is the "hand-port forever" failure the house keeps filing items
  against.

### B. A small `sqlitekit` package in the kit's root module

A package that owns HOW a SQLite database is opened and evolved, and nothing
about WHAT is stored in it:

- `Open(ctx, path, Options) (*sql.DB, error)`: builds the DSN with
  `_pragma=busy_timeout(N)`, `_pragma=journal_mode(WAL)` and
  `_txlock=immediate`; caps the pool at one
  connection (which also keeps `:memory:` coherent); refuses a DSN that already
  sets any of those, rather than silently merging two opinions; pings before
  returning so a bad path fails at startup, not at the first request.
- `Migrate(ctx, db, []Migration) error`: ordered, numbered migrations, each in
  its own transaction, tracked in `PRAGMA user_version`. A database whose
  version is NEWER than the binary knows is refused -- the rollback case none of
  the current adapters can detect. Services keep their DDL; the kit keeps the
  bookkeeping.
- `IsBusy(err) bool` (`SQLITE_BUSY`/`SQLITE_LOCKED`) and
  `IsConstraint(err) bool`, so contention and uniqueness become typed errors
  without every service importing the driver's error type and hard-coding 5
  and 6.
- A `storekit.Pinger` over the `*sql.DB`, for readiness.

The kit's go.mod gains `modernc.org/sqlite` (and transitively
`modernc.org/libc`, `mathutil`, `memory`).

- **For:** one open sequence, reviewed once; nagus's two adapters get immediate
  transactions and DSN pragmas by changing one call. Migration versioning exists
  for the first time. It stays within the storekit philosophy (mechanism in the
  kit, domain in the service). Go's module graph pruning means a service that
  does not import `sqlitekit` neither compiles nor links the driver, and
  `govulncheck` (which is symbol-reachability based) does not report its
  vulnerabilities to that service.
- **Against:** the kit's own `go.mod`/`go.sum` carry the driver and its large
  transpiled libc, so every kit consumer sees those modules in `go mod graph`
  and Renovate PRs for them arrive at the kit. MVS selects the newer of the kit's and the
  service's driver requirement, so a kit bump can silently drag a SQLite-using
  service onto a driver version it never tested -- manageable with a Renovate
  group that moves the kit and those services in step, but a real coordination
  cost. The kit's CI runs the driver's tests-by-use, which is
  slower than today's suite.

### C. The same package as a nested module (`go-service-kit/sqlitekit` with its own go.mod)

Identical API to B, released as its own module with `sqlitekit/vX.Y.Z` tags.

- **For:** the root module's graph stays driver-free, so consumers that never
  touch SQLite see nothing at all; driver bumps release independently of the
  rest of the kit.
- **Against:** multi-module repos are genuinely more work: prefixed tags, a
  `replace` or pseudo-version dance while developing both halves, CI that knows
  about two modules, `go work` for local development, and a second release line
  to keep coherent with the first. The kit is one module today and its release
  notes assume it. The benefit over B is mostly cosmetic given graph pruning.

### D. A generic SQLite `Store[T, K]` (JSON blob per key) in the kit

- **For:** a new service with a key-value shape gets persistence in one line.
- **Against:** it contradicts the `storekit` package's recorded reasoning, and
  neither existing consumer could use it: both need real columns, indexes, FTS5
  or domain-specific queries. It would be a third store shape to maintain that
  nobody runs in production. Rejected on the kit's own prior decision; listed so
  that the rejection is recorded.

### E. A driver-free helper (DSN builder + migrations over `*sql.DB`), no import of `modernc.org/sqlite`

The kit builds the modernc-specific DSN string and runs migrations over any
`*sql.DB`; the service blank-imports the driver itself.

- **For:** no driver in the kit's go.mod... at first.
- **Against:** the kit's tests must still open a real SQLite database to prove
  the pragmas take effect, which puts the driver in go.mod anyway (Go has no
  test-only requirements). `IsBusy` cannot be written without the driver's
  error type. And a service that forgets the blank import fails at runtime with
  `sql: unknown driver "sqlite"`. It is B with worse ergonomics and the same
  go.mod.

## Recommendation

**B: a `sqlitekit` package in the root module, scoped to open, migrate, error
classification and readiness -- no Store.**

The deciding facts are that the adapters have already diverged on the exact
settings that make SQLite correct under concurrency, that two of the three are
on the weaker side, and that graph pruning makes the cost of B over C small for
consumers who never import it. C remains the right move if the driver's
dependency weight ever becomes a practical problem for a non-SQLite consumer
(for example, a vulnerability report that cannot be triaged away, or Renovate
churn that the kit's maintainers find disruptive); moving a package into a
nested module later is mechanical and does not change its import path's last
element.

Conditions, if accepted:

1. The package defaults are quark's, verbatim, with the reasons in its doc
   (this ADR's Context table is the source).
2. A Renovate group keeps `modernc.org/sqlite` in step across the kit, nagus
   and quark.
3. The first adopters are nagus's two adapters (they gain the most) and quark's
   `OpenSQLite`; each adoption is a follow-up MR in that service, not part of
   the kit change. quark's Unicode-version check becomes a service-level
   migration step on top of `Migrate`, not a kit feature.
4. `storekit`'s harness runs against a `sqlitekit`-opened reference adapter in
   the kit's own tests, so the harness and the open sequence are proven
   together.

## Consequences

- If B is accepted: a new `sqlitekit` package and a go.mod dependency, one
  CHANGELOG entry flagging the new requirement, and follow-up items in nagus
  and quark. The QUARK-06 item closes when the package ships, not when every
  adopter has migrated.
- If A is chosen instead: record the three open-sequence lessons in
  `go-service-template`'s docs so the next service copies quark rather than
  nagus, and file nagus items to adopt `_txlock=immediate` and DSN pragmas by
  hand. The QUARK-06 item closes as "decided: not in the kit".
- Either way, D is not revisited without a consumer that actually has a
  key-value shape.
