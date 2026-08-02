// Package storekit provides a reusable contract-test harness for store
// adapters.
//
// # Why a harness and not an interface
//
// The kit deliberately does NOT define a universal Store that every service
// implements. Real services have service-specific stores: nagus has its own,
// and the template's worked example defines another. Forcing them through one
// kit-owned interface would either bloat that interface or bend the services.
//
// What generalizes is not the interface, it is the CONTRACT: whatever your
// adapter's real shape, put-then-get must round-trip, a missing key must not
// be an error, a second put on the same key must overwrite rather than
// duplicate, listing must be ordered and paginate without gaps or repeats, and
// a cancelled context must be honored. Harness is that contract, parameterized
// over the service's own entity and key types. Adapters whose real interface is
// wider embed [Store]; the harness only ever calls these five methods.
//
// # Usage
//
//	func TestMemStore(t *testing.T) {
//	    storekit.Harness[widget, string]{
//	        NewStore: func(t *testing.T) storekit.Store[widget, string] { return newMemStore(50) },
//	        NewItem:  func(seed int) widget { return widget{ID: fmt.Sprintf("wid-%04d", seed)} },
//	        KeyOf:    func(w widget) string { return w.ID },
//	        PageSize: 50,
//	    }.Run(t)
//	}
//
// The package's own harness_test.go carries a complete reference in-memory
// adapter; copy it as the starting point for a new adapter.
//
// # Adopting a SUBSET of the contract
//
// [Store] is a shape, and a real service's store often is not that shape.
// nagus's is Put/Get/Search(Query)/DeleteStale: no List, no single-key Delete,
// and a natural ordering of SeenAt DESC, which is not the TOTAL order the List
// clauses require. Satisfying [Store] would mean adding three methods to three
// adapters purely for a test and changing a live query's ORDER BY to make it
// total. That is a bad trade, and "all or nothing" would push such a service
// out of the harness entirely -- including out of the one clause it most wants.
//
// Two ways in, in increasing order of coverage:
//
//   - [PingHarness] takes a [Pinger] and nothing else. Readiness is the clause
//     with the highest value-to-cost ratio, and it should not require the whole
//     shape.
//
//   - [Harness.NewAdapter] declares the operations method by method instead of
//     through [Store], and [Harness.RunClauses] runs a named subset. Clauses
//     whose operations are absent are skipped by [Harness.Run] with a message
//     naming what is missing, and are a hard failure in RunClauses, because
//     asking for a clause by name and silently not running it is the one
//     outcome worse than not running it.
//
// [Harness.NewStore] and [Harness.Run] are unchanged: an adapter that does
// implement [Store] runs all ten clauses exactly as before.
//
// # Why Ping is in the contract
//
// Ping exists so that readiness is a real dependency check rather than a
// hardcoded 200. The kit's httpapi readiness handle is designed to be fed
// dependency probes (ready.Register("store", store.Ping)); if the store cannot
// answer, /readyz must report 503 and the pod must leave the Service
// endpoints. An adapter that cannot be probed forces every service to invent
// its own fake health signal, which is how a fleet ends up reporting healthy
// while every request fails.
//
// # Contract clauses the harness pins down
//
//   - Get on a missing key returns (zero, false, nil). Absence is NOT an error;
//     errors are reserved for the store failing to answer.
//   - Put on an existing key OVERWRITES it. Put is an upsert; it never
//     duplicates and never errors on conflict.
//   - Delete on a missing key returns nil. Delete is IDEMPOTENT. Callers
//     retrying after a timeout must not have to distinguish "deleted" from
//     "already gone".
//   - List returns items in a total, stable order that does not change between
//     calls when the data does not change. The harness does not mandate WHICH
//     order (key-ascending is the obvious choice, and what "ORDER BY id" gives
//     a SQL adapter); it requires only that the order is deterministic, since
//     cursor pagination is otherwise meaningless.
//   - List's Cursor is OPAQUE. The zero Cursor ("") means "start at the
//     beginning" on input and "no more results" on output. A page whose
//     returned cursor is empty is the last page. Clients must not parse a
//     cursor; adapters should encode rather than expose raw keys.
//   - Every method honors context cancellation: called with an already-
//     cancelled context they must return an error satisfying
//     errors.Is(err, context.Canceled) and must not mutate state.
//
// # Adapters must be CGO-free
//
// Any real adapter must be usable from a CGO-free static binary. The
// deployment image is distroless static built with CGO_ENABLED=0, so a
// CGO-based driver produces a binary that either fails to build or fails to
// start in the image. A SQLite adapter must therefore use a pure-Go driver
// such as modernc.org/sqlite (as nagus does), never github.com/mattn/go-sqlite3.
// The same rule applies to any other driver reached for later.
package storekit

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// Cursor is an opaque pagination token.
//
// The zero value means "start at the beginning" when passed to
// [Store.List] and "there are no further pages" when returned from it.
// Its contents are the adapter's business: callers, including this harness,
// must treat a Cursor as an opaque string and only ever hand it back.
type Cursor string

// Pinger is the readiness clause on its own.
//
// It is separate from [Store] because it is the clause a service is most likely
// to want and least likely to have to bend its design for: readiness must be a
// real dependency check, and requiring the whole five-method shape to get one
// is how a service ends up hardcoding /readyz to 200. Type your store's
// readiness seam as this, register it with
// httpapi.Readiness.Register("store", store.Ping), and gate it with
// [PingHarness].
//
// Contract: Ping returns nil when the store can answer, an error when it cannot,
// and honors context cancellation. It must be cheap enough to run on every
// probe -- a connection-pool ping, not a query.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Putter, Getter, Lister and Deleter are the remaining clauses, each on its
// own, so that an adapter can be typed as exactly what it implements and the
// harness can report precisely which clauses that covers.
type (
	// Putter inserts item, overwriting any existing item with the same key.
	Putter[T any] interface {
		Put(ctx context.Context, item T) error
	}
	// Getter returns the item for key. A missing key returns (zero, false, nil).
	Getter[T any, K comparable] interface {
		Get(ctx context.Context, key K) (T, bool, error)
	}
	// Lister returns a page of items in a stable order plus the cursor for the
	// next page, or the zero Cursor when the page returned is the last.
	Lister[T any] interface {
		List(ctx context.Context, cur Cursor) ([]T, Cursor, error)
	}
	// Deleter removes key. Deleting a missing key is not an error.
	Deleter[K comparable] interface {
		Delete(ctx context.Context, key K) error
	}
)

// Store is the minimal shape the harness exercises. A service whose real store
// is wider embeds this interface; the harness never calls anything else. A
// service whose real store is NARROWER cannot embed it and should use
// [Harness.NewAdapter] or [PingHarness] instead -- see "Adopting a SUBSET of the
// contract" in the package doc.
//
// The method set is the same five it has always been; it is now composed from
// the single-clause interfaces above so that a partially conforming adapter can
// name what it does implement.
type Store[T any, K comparable] interface {
	Putter[T]
	Getter[T, K]
	Lister[T]
	Deleter[K]
	Pinger
}

// Clause names. Each is one subtest of the contract suite and one argument
// [Harness.RunClauses] accepts. They are exported so a partially conforming
// adapter can name the subset it promises in code rather than in a comment.
const (
	ClausePing                   = "Ping"
	ClausePutGetRoundTrip        = "PutGetRoundTrip"
	ClauseGetMissingKey          = "GetMissingKey"
	ClausePutOverwrites          = "PutOverwrites"
	ClauseDelete                 = "Delete"
	ClauseDeleteMissingKeyIsNoOp = "DeleteMissingKeyIsNoOp"
	ClauseListEmpty              = "ListEmpty"
	ClauseListStableOrder        = "ListStableOrder"
	ClauseListPaginates          = "ListPaginates"
	ClauseContextCanceled        = "ContextCanceled"
)

// Clauses returns every clause name, in the order [Harness.Run] executes them.
func Clauses() []string {
	return []string{
		ClausePing,
		ClausePutGetRoundTrip,
		ClauseGetMissingKey,
		ClausePutOverwrites,
		ClauseDelete,
		ClauseDeleteMissingKeyIsNoOp,
		ClauseListEmpty,
		ClauseListStableOrder,
		ClauseListPaginates,
		ClauseContextCanceled,
	}
}

// Operation names, as they appear in a skip message and in [Adapter].
const (
	opPut    = "Put"
	opGet    = "Get"
	opList   = "List"
	opDelete = "Delete"
	opPing   = "Ping"
)

// clauseNeeds records which operations each clause calls. A clause is run only
// when the adapter supplies all of them.
//
// PutOverwrites and Delete need List because their assertion is about what the
// store CONTAINS afterwards -- that an upsert did not duplicate, that a delete
// removed one row and not two. Dropping that half would be silently reduced
// coverage, which this package treats as worse than a skip.
var clauseNeeds = map[string][]string{
	ClausePing:                   {opPing},
	ClausePutGetRoundTrip:        {opPut, opGet},
	ClauseGetMissingKey:          {opGet},
	ClausePutOverwrites:          {opPut, opGet, opList},
	ClauseDelete:                 {opPut, opGet, opDelete, opList},
	ClauseDeleteMissingKeyIsNoOp: {opPut, opDelete},
	ClauseListEmpty:              {opList},
	ClauseListStableOrder:        {opPut, opList},
	ClauseListPaginates:          {opPut, opList},
	// ContextCanceled runs a subtest per operation the adapter supplies, so it
	// needs none of them up front; it skips if the adapter supplies nothing.
	ClauseContextCanceled: nil,
}

// clausesNeedingPageSize are the ones that seed 2*PageSize+1 items.
var clausesNeedingPageSize = map[string]bool{
	ClauseListStableOrder: true,
	ClauseListPaginates:   true,
}

// Adapter declares a store's operations one at a time, for an adapter whose
// real shape is NARROWER than [Store].
//
// A nil field means "this store does not offer that operation", and the clauses
// that need it are skipped by [Harness.Run] rather than failing to compile.
// This is the seam for a store like nagus's, which has Put, Get and Ping but
// Search(Query) and DeleteStale(source, before) in place of List and Delete:
// it can be gated on the clauses it does promise instead of being pushed out of
// the harness entirely.
//
// Use [AdapterFor] when the store does implement [Store].
type Adapter[T any, K comparable] struct {
	Put    func(ctx context.Context, item T) error
	Get    func(ctx context.Context, key K) (T, bool, error)
	List   func(ctx context.Context, cur Cursor) ([]T, Cursor, error)
	Delete func(ctx context.Context, key K) error
	Ping   func(ctx context.Context) error
}

// AdapterFor lifts a full [Store] into an [Adapter]. [Harness.NewStore] uses it
// internally, so the two entry points run identical code.
func AdapterFor[T any, K comparable](s Store[T, K]) Adapter[T, K] {
	return Adapter[T, K]{
		Put:    s.Put,
		Get:    s.Get,
		List:   s.List,
		Delete: s.Delete,
		Ping:   s.Ping,
	}
}

// has reports the operations this adapter is missing from want.
func (a Adapter[T, K]) missing(want []string) []string {
	var out []string
	for _, op := range want {
		switch op {
		case opPut:
			if a.Put == nil {
				out = append(out, op)
			}
		case opGet:
			if a.Get == nil {
				out = append(out, op)
			}
		case opList:
			if a.List == nil {
				out = append(out, op)
			}
		case opDelete:
			if a.Delete == nil {
				out = append(out, op)
			}
		case opPing:
			if a.Ping == nil {
				out = append(out, op)
			}
		}
	}
	return out
}

// PingHarness gates the readiness clause on its own, for a store that does not
// and should not implement the whole [Store] shape.
//
// It is the smallest useful contract in this package and the one with the best
// value-to-cost ratio: a store that cannot be probed forces a service to
// hardcode /readyz, which is how a fleet ends up reporting healthy while every
// request fails.
//
//	func TestPostgresStoreReadiness(t *testing.T) {
//	    storekit.PingHarness{
//	        NewStore: func(t *testing.T) storekit.Pinger { return newTestStore(t) },
//	    }.Run(t)
//	}
type PingHarness struct {
	// NewStore returns a fresh, HEALTHY store. Called once per subtest.
	NewStore func(t *testing.T) Pinger
}

// Run executes the two readiness clauses: a healthy store answers nil, and a
// cancelled context is honored rather than ignored.
func (h PingHarness) Run(t *testing.T) {
	t.Helper()
	if h.NewStore == nil {
		t.Fatal("storekit: PingHarness.NewStore is required")
	}
	Harness[struct{}, string]{
		NewAdapter: func(t *testing.T) Adapter[struct{}, string] {
			return Adapter[struct{}, string]{Ping: h.NewStore(t).Ping}
		},
	}.RunClauses(t, ClausePing, ClauseContextCanceled)
}

// Harness is the contract every store adapter must satisfy. A service
// instantiates it with its own entity type and a factory that produces a
// fresh, empty adapter, then calls [Harness.Run] from a single Test function.
type Harness[T any, K comparable] struct {
	// NewStore returns a fresh, EMPTY adapter. It is called once per subtest,
	// so subtests never share state. Register any cleanup with t.Cleanup.
	//
	// Exactly one of NewStore and [Harness.NewAdapter] must be set.
	NewStore func(t *testing.T) Store[T, K]

	// NewAdapter is NewStore for a store that does not implement the whole
	// [Store] shape: it declares the operations one at a time and leaves the
	// absent ones nil. See "Adopting a SUBSET of the contract" in the package
	// doc.
	NewAdapter func(t *testing.T) Adapter[T, K]

	// NewItem returns a deterministic fixture for seed. Distinct seeds must
	// produce distinct keys, and the same seed must always produce the same
	// item. The harness verifies both before running any case.
	NewItem func(seed int) T

	// KeyOf extracts the key an item is stored under.
	KeyOf func(T) K

	// PageSize is the maximum number of items the adapter returns from one
	// List call. It must be greater than zero: the harness seeds
	// 2*PageSize+1 items so pagination is exercised across at least three
	// pages. Keep it small (single digits) in tests.
	PageSize int

	// Mutate is optional. It returns a copy of item with the SAME key and at
	// least one different field. When set, the overwrite case asserts the
	// stored value was actually replaced rather than merely not duplicated.
	Mutate func(item T) T

	// Equal is optional. It reports whether two items are equivalent.
	// Defaults to reflect.DeepEqual.
	Equal func(a, b T) bool
}

// Run executes the whole contract suite. Each clause is a named subtest so a
// failure names the clause it violated.
//
// A clause whose operations the adapter does not supply is SKIPPED, with a
// message naming what is missing. For an adapter built from [Harness.NewStore]
// that never happens -- [Store] has all five -- so this is exactly what it
// always was.
func (h Harness[T, K]) Run(t *testing.T) {
	t.Helper()
	h.run(t, Clauses(), false)
}

// RunClauses executes only the named clauses, for an adapter that conforms to
// part of the contract. Names come from the Clause* constants; [Clauses]
// returns all of them.
//
//	// nagus's store has Put, Get and Ping; Search(Query) and
//	// DeleteStale(source, before) stand in for List and Delete.
//	storekit.Harness[item, string]{
//	    NewAdapter: func(t *testing.T) storekit.Adapter[item, string] {
//	        s := newStore(t)
//	        return storekit.Adapter[item, string]{Put: s.Put, Get: s.Get, Ping: s.Ping}
//	    },
//	    NewItem: newItem,
//	    KeyOf:   func(i item) string { return i.ID },
//	}.RunClauses(t, storekit.ClausePing, storekit.ClausePutGetRoundTrip,
//	    storekit.ClauseGetMissingKey, storekit.ClauseContextCanceled)
//
// Unlike [Harness.Run], a named clause whose operations are missing is a
// FAILURE, not a skip: asking for a clause by name and silently not running it
// is the one outcome worse than not running it. An unknown name is a failure
// too.
func (h Harness[T, K]) RunClauses(t *testing.T, names ...string) {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("storekit: RunClauses needs at least one clause name; use Run for the whole suite")
	}
	h.run(t, names, true)
}

func (h Harness[T, K]) run(t *testing.T, names []string, strict bool) {
	t.Helper()

	clauses := h.clauseFuncs()
	for _, name := range names {
		if _, ok := clauses[name]; !ok {
			t.Fatalf("storekit: unknown clause %q; the valid names are %v", name, Clauses())
		}
	}

	// One throwaway adapter, used only to discover which operations exist, so
	// that validation can demand exactly the fixtures the runnable clauses
	// need and no more.
	probe := h.newAdapter(t)
	h.validate(t, names, probe)

	for _, name := range names {
		if miss := probe.missing(clauseNeeds[name]); len(miss) > 0 {
			if strict {
				t.Errorf("storekit: clause %s was requested but the adapter supplies no %v", name, miss)
				continue
			}
			t.Run(name, func(t *testing.T) {
				t.Skipf("storekit: adapter supplies no %v", miss)
			})
			continue
		}
		t.Run(name, clauses[name])
	}
}

func (h Harness[T, K]) clauseFuncs() map[string]func(*testing.T) {
	return map[string]func(*testing.T){
		ClausePing:                   h.testPing,
		ClausePutGetRoundTrip:        h.testPutGetRoundTrip,
		ClauseGetMissingKey:          h.testGetMissingKey,
		ClausePutOverwrites:          h.testPutOverwrites,
		ClauseDelete:                 h.testDelete,
		ClauseDeleteMissingKeyIsNoOp: h.testDeleteMissingKeyIsNoOp,
		ClauseListEmpty:              h.testListEmpty,
		ClauseListStableOrder:        h.testListStableOrder,
		ClauseListPaginates:          h.testListPaginates,
		ClauseContextCanceled:        h.testContextCanceled,
	}
}

// newAdapter produces a fresh, empty adapter from whichever factory the harness
// was configured with.
func (h Harness[T, K]) newAdapter(t *testing.T) Adapter[T, K] {
	t.Helper()
	switch {
	case h.NewStore == nil && h.NewAdapter == nil:
		t.Fatal("storekit: one of Harness.NewStore and Harness.NewAdapter is required")
	case h.NewStore != nil && h.NewAdapter != nil:
		t.Fatal("storekit: set exactly one of Harness.NewStore and Harness.NewAdapter, not both")
	case h.NewAdapter != nil:
		return h.NewAdapter(t)
	}
	return AdapterFor(h.NewStore(t))
}

// validate fails fast on a misconfigured harness, so a wiring mistake never
// looks like silently reduced coverage. It demands only what the clauses that
// will actually run require: a Ping-only adapter needs no fixtures at all.
func (h Harness[T, K]) validate(t *testing.T, names []string, probe Adapter[T, K]) {
	t.Helper()

	hasData := probe.Put != nil || probe.Get != nil || probe.List != nil || probe.Delete != nil

	var needFixtures, needPageSize bool
	for _, name := range names {
		if len(probe.missing(clauseNeeds[name])) > 0 {
			continue
		}
		switch name {
		case ClausePing:
			// Readiness needs no fixture at all.
		case ClauseContextCanceled:
			// Runs one subtest per available operation; only the data ones
			// need an item to work on.
			needFixtures = needFixtures || hasData
		default:
			needFixtures = true
		}
		if clausesNeedingPageSize[name] {
			needPageSize = true
		}
	}

	if !needFixtures {
		return
	}
	if h.NewItem == nil {
		t.Fatal("storekit: Harness.NewItem is required")
	}
	if h.KeyOf == nil {
		t.Fatal("storekit: Harness.KeyOf is required")
	}
	if needPageSize && h.PageSize <= 0 {
		t.Fatalf("storekit: Harness.PageSize must be > 0 (the adapter's maximum List page size), got %d", h.PageSize)
	}
	if a, b := h.KeyOf(h.NewItem(0)), h.KeyOf(h.NewItem(0)); a != b {
		t.Fatalf("storekit: Harness.NewItem is not deterministic: seed 0 produced keys %v and %v", a, b)
	}
	if a, b := h.KeyOf(h.NewItem(0)), h.KeyOf(h.NewItem(1)); a == b {
		t.Fatalf("storekit: Harness.NewItem must give distinct seeds distinct keys, both produced %v", a)
	}
	if h.Mutate != nil {
		orig := h.NewItem(0)
		mutated := h.Mutate(orig)
		if k1, k2 := h.KeyOf(orig), h.KeyOf(mutated); k1 != k2 {
			t.Fatalf("storekit: Harness.Mutate must preserve the key, %v became %v", k1, k2)
		}
		if h.equal(orig, mutated) {
			t.Fatal("storekit: Harness.Mutate must change at least one non-key field")
		}
	}
}

func (h Harness[T, K]) equal(a, b T) bool {
	if h.Equal != nil {
		return h.Equal(a, b)
	}
	return reflect.DeepEqual(a, b)
}

func (h Harness[T, K]) testPing(t *testing.T) {
	a := h.newAdapter(t)
	if err := a.Ping(t.Context()); err != nil {
		t.Fatalf("Ping on a healthy store: got error %v, want nil", err)
	}
}

func (h Harness[T, K]) testPutGetRoundTrip(t *testing.T) {
	ctx := t.Context()
	a := h.newAdapter(t)
	want := h.NewItem(1)

	if err := a.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, found, err := a.Get(ctx, h.KeyOf(want))
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	if !found {
		t.Fatalf("Get after Put: found=false, want true for key %v", h.KeyOf(want))
	}
	if !h.equal(got, want) {
		t.Fatalf("Get after Put returned %#v, want %#v", got, want)
	}
}

func (h Harness[T, K]) testGetMissingKey(t *testing.T) {
	ctx := t.Context()
	a := h.newAdapter(t)

	var zero T
	got, found, err := a.Get(ctx, h.KeyOf(h.NewItem(99)))
	if err != nil {
		t.Fatalf("Get on a missing key must not error, got %v", err)
	}
	if found {
		t.Fatal("Get on a missing key: found=true, want false")
	}
	if !h.equal(got, zero) {
		t.Fatalf("Get on a missing key returned %#v, want the zero value", got)
	}
}

func (h Harness[T, K]) testPutOverwrites(t *testing.T) {
	ctx := t.Context()
	a := h.newAdapter(t)

	first := h.NewItem(1)
	second := first
	if h.Mutate != nil {
		second = h.Mutate(first)
	}
	if err := a.Put(ctx, first); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := a.Put(ctx, second); err != nil {
		t.Fatalf("second Put on the same key must overwrite, not error: %v", err)
	}

	got, found, err := a.Get(ctx, h.KeyOf(first))
	if err != nil || !found {
		t.Fatalf("Get after overwrite: found=%v err=%v, want true/nil", found, err)
	}
	if !h.equal(got, second) {
		t.Fatalf("Get after overwrite returned %#v, want the second value %#v", got, second)
	}

	all := h.sweep(ctx, t, a)
	if len(all) != 1 {
		t.Fatalf("overwrite must not duplicate: List returned %d items, want 1", len(all))
	}
}

func (h Harness[T, K]) testDelete(t *testing.T) {
	ctx := t.Context()
	a := h.newAdapter(t)

	item := h.NewItem(1)
	other := h.NewItem(2)
	for _, it := range []T{item, other} {
		if err := a.Put(ctx, it); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := a.Delete(ctx, h.KeyOf(item)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, err := a.Get(ctx, h.KeyOf(item)); err != nil || found {
		t.Fatalf("Get after Delete: found=%v err=%v, want false/nil", found, err)
	}
	if _, found, err := a.Get(ctx, h.KeyOf(other)); err != nil || !found {
		t.Fatalf("Delete removed an unrelated key: found=%v err=%v, want true/nil", found, err)
	}
	if all := h.sweep(ctx, t, a); len(all) != 1 {
		t.Fatalf("after deleting 1 of 2, List returned %d items, want 1", len(all))
	}
}

func (h Harness[T, K]) testDeleteMissingKeyIsNoOp(t *testing.T) {
	ctx := t.Context()
	a := h.newAdapter(t)

	// Contract: Delete is idempotent. Deleting an absent key returns nil.
	key := h.KeyOf(h.NewItem(99))
	if err := a.Delete(ctx, key); err != nil {
		t.Fatalf("Delete on a missing key must return nil (Delete is idempotent), got %v", err)
	}
	if err := a.Delete(ctx, key); err != nil {
		t.Fatalf("second Delete on a missing key must return nil, got %v", err)
	}

	item := h.NewItem(1)
	if err := a.Put(ctx, item); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := a.Delete(ctx, h.KeyOf(item)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := a.Delete(ctx, h.KeyOf(item)); err != nil {
		t.Fatalf("re-deleting an already deleted key must return nil, got %v", err)
	}
}

func (h Harness[T, K]) testListEmpty(t *testing.T) {
	ctx := t.Context()
	a := h.newAdapter(t)

	page, next, err := a.List(ctx, "")
	if err != nil {
		t.Fatalf("List on an empty store: %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("List on an empty store returned %d items, want 0", len(page))
	}
	if next != "" {
		t.Fatalf("List on an empty store returned cursor %q, want the zero Cursor", next)
	}
}

func (h Harness[T, K]) testListStableOrder(t *testing.T) {
	ctx := t.Context()
	a := h.newAdapter(t)
	seeded := h.seed(ctx, t, a)

	first := h.keys(h.sweep(ctx, t, a))
	second := h.keys(h.sweep(ctx, t, a))

	if len(first) != len(seeded) {
		t.Fatalf("List returned %d items, want the %d seeded", len(first), len(seeded))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("List order is not stable: position %d was %v then %v", i, first[i], second[i])
		}
	}

	want := make(map[K]bool, len(seeded))
	for _, it := range seeded {
		want[h.KeyOf(it)] = true
	}
	for _, k := range first {
		if !want[k] {
			t.Fatalf("List returned key %v that was never Put", k)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("List omitted %d seeded keys, e.g. %v", len(want), anyKey(want))
	}
}

func (h Harness[T, K]) testListPaginates(t *testing.T) {
	ctx := t.Context()
	a := h.newAdapter(t)
	seeded := h.seed(ctx, t, a)

	seen := make(map[K]int, len(seeded))
	pages := 0
	var cur Cursor
	for {
		page, next, err := a.List(ctx, cur)
		if err != nil {
			t.Fatalf("List(page %d): %v", pages, err)
		}
		if len(page) > h.PageSize {
			t.Fatalf("List returned %d items, more than the declared PageSize %d", len(page), h.PageSize)
		}
		if len(page) > 0 {
			pages++
		}
		for _, it := range page {
			k := h.KeyOf(it)
			seen[k]++
			if seen[k] > 1 {
				t.Fatalf("pagination returned key %v %d times: pages must not overlap", k, seen[k])
			}
		}
		if next == "" {
			break
		}
		if next == cur {
			t.Fatalf("List returned the same cursor %q twice: pagination cannot terminate", next)
		}
		cur = next
		if pages > len(seeded)+2 {
			t.Fatalf("pagination did not terminate after %d pages for %d items", pages, len(seeded))
		}
	}

	if pages < 2 {
		t.Fatalf("pagination produced %d page(s) for %d items with PageSize %d, want at least 2; does the adapter honor the cursor?", pages, len(seeded), h.PageSize)
	}
	if len(seen) != len(seeded) {
		t.Fatalf("pagination yielded %d distinct keys, want %d: a page was dropped", len(seen), len(seeded))
	}
	for _, it := range seeded {
		if seen[h.KeyOf(it)] == 0 {
			t.Fatalf("pagination never returned seeded key %v", h.KeyOf(it))
		}
	}
}

// testContextCanceled runs one subtest per operation the adapter supplies. It
// is the one clause that adapts to a partial adapter rather than being skipped
// wholesale: cancellation is a per-method promise, so a store with three
// methods can honor it for three methods.
func (h Harness[T, K]) testContextCanceled(t *testing.T) {
	// setup returns a fresh adapter and the item seeded into it. When the
	// adapter has no Put there is nothing to seed, and the subtests that need a
	// seeded item are not registered.
	setup := func(t *testing.T) (Adapter[T, K], T) {
		t.Helper()
		a := h.newAdapter(t)
		var item T
		if a.Put != nil {
			item = h.NewItem(1)
			if err := a.Put(t.Context(), item); err != nil {
				t.Fatalf("Put during setup: %v", err)
			}
		}
		return a, item
	}
	cancelled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	check := func(t *testing.T, method string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s with an already-cancelled context returned nil, want context.Canceled", method)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s with an already-cancelled context returned %v, want an error satisfying errors.Is(err, context.Canceled)", method, err)
		}
	}

	probe := h.newAdapter(t)
	ran := 0

	if probe.Put != nil {
		ran++
		t.Run(opPut, func(t *testing.T) {
			a, _ := setup(t)
			check(t, opPut, a.Put(cancelled(), h.NewItem(2)))
			if a.Get == nil {
				return
			}
			if _, found, err := a.Get(t.Context(), h.KeyOf(h.NewItem(2))); err != nil || found {
				t.Fatalf("Put on a cancelled context must not mutate the store: found=%v err=%v", found, err)
			}
		})
	}
	if probe.Get != nil {
		ran++
		t.Run(opGet, func(t *testing.T) {
			a, item := setup(t)
			if a.Put == nil {
				item = h.NewItem(1)
			}
			_, _, err := a.Get(cancelled(), h.KeyOf(item))
			check(t, opGet, err)
		})
	}
	if probe.List != nil {
		ran++
		t.Run(opList, func(t *testing.T) {
			a, _ := setup(t)
			_, _, err := a.List(cancelled(), "")
			check(t, opList, err)
		})
	}
	if probe.Delete != nil {
		ran++
		t.Run(opDelete, func(t *testing.T) {
			a, item := setup(t)
			if a.Put == nil {
				item = h.NewItem(1)
			}
			check(t, opDelete, a.Delete(cancelled(), h.KeyOf(item)))
			if a.Put == nil || a.Get == nil {
				return
			}
			if _, found, err := a.Get(t.Context(), h.KeyOf(item)); err != nil || !found {
				t.Fatalf("Delete on a cancelled context must not mutate the store: found=%v err=%v", found, err)
			}
		})
	}
	if probe.Ping != nil {
		ran++
		t.Run(opPing, func(t *testing.T) {
			a, _ := setup(t)
			check(t, opPing, a.Ping(cancelled()))
		})
	}

	if ran == 0 {
		t.Skip("storekit: adapter supplies no operations")
	}
}

// seed fills the store with 2*PageSize+1 items so pagination spans at least
// three pages, and returns them.
func (h Harness[T, K]) seed(ctx context.Context, t *testing.T, a Adapter[T, K]) []T {
	t.Helper()
	n := 2*h.PageSize + 1
	items := make([]T, 0, n)
	for i := range n {
		item := h.NewItem(i)
		if err := a.Put(ctx, item); err != nil {
			t.Fatalf("Put(seed %d): %v", i, err)
		}
		items = append(items, item)
	}
	return items
}

// sweep walks every page and returns the items in the order List yielded them.
func (h Harness[T, K]) sweep(ctx context.Context, t *testing.T, a Adapter[T, K]) []T {
	t.Helper()
	var (
		out   []T
		cur   Cursor
		calls int
	)
	for {
		page, next, err := a.List(ctx, cur)
		if err != nil {
			t.Fatalf("List(cursor %q): %v", cur, err)
		}
		out = append(out, page...)
		calls++
		if next == "" {
			return out
		}
		if next == cur {
			t.Fatalf("List returned the same cursor %q twice: pagination cannot terminate", next)
		}
		cur = next
		if calls > len(out)+2 {
			t.Fatalf("List did not terminate after %d calls yielding %d items", calls, len(out))
		}
	}
}

func (h Harness[T, K]) keys(items []T) []K {
	out := make([]K, 0, len(items))
	for _, it := range items {
		out = append(out, h.KeyOf(it))
	}
	return out
}

func anyKey[K comparable](m map[K]bool) K {
	for k := range m {
		return k
	}
	var zero K
	return zero
}
