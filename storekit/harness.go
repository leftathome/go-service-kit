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

// Store is the minimal shape the harness exercises. A service whose real store
// is wider embeds this interface; the harness never calls anything else.
type Store[T any, K comparable] interface {
	// Put inserts item, overwriting any existing item with the same key.
	Put(ctx context.Context, item T) error
	// Get returns the item for key. A missing key returns (zero, false, nil).
	Get(ctx context.Context, key K) (T, bool, error)
	// List returns a page of items in a stable order plus the cursor for the
	// next page, or the zero Cursor when the page returned is the last.
	List(ctx context.Context, cur Cursor) ([]T, Cursor, error)
	// Delete removes key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key K) error
	// Ping reports whether the store is reachable, so readiness can be a real
	// dependency check.
	Ping(ctx context.Context) error
}

// Harness is the contract every store adapter must satisfy. A service
// instantiates it with its own entity type and a factory that produces a
// fresh, empty adapter, then calls [Harness.Run] from a single Test function.
type Harness[T any, K comparable] struct {
	// NewStore returns a fresh, EMPTY adapter. It is called once per subtest,
	// so subtests never share state. Register any cleanup with t.Cleanup.
	NewStore func(t *testing.T) Store[T, K]

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

// Run executes the contract suite. Each clause is a named subtest so a failure
// names the clause it violated.
func (h Harness[T, K]) Run(t *testing.T) {
	t.Helper()
	h.validate(t)

	t.Run("Ping", h.testPing)
	t.Run("PutGetRoundTrip", h.testPutGetRoundTrip)
	t.Run("GetMissingKey", h.testGetMissingKey)
	t.Run("PutOverwrites", h.testPutOverwrites)
	t.Run("Delete", h.testDelete)
	t.Run("DeleteMissingKeyIsNoOp", h.testDeleteMissingKeyIsNoOp)
	t.Run("ListEmpty", h.testListEmpty)
	t.Run("ListStableOrder", h.testListStableOrder)
	t.Run("ListPaginates", h.testListPaginates)
	t.Run("ContextCanceled", h.testContextCanceled)
}

// validate fails fast on a misconfigured harness, so a wiring mistake never
// looks like silently reduced coverage.
func (h Harness[T, K]) validate(t *testing.T) {
	t.Helper()
	if h.NewStore == nil {
		t.Fatal("storekit: Harness.NewStore is required")
	}
	if h.NewItem == nil {
		t.Fatal("storekit: Harness.NewItem is required")
	}
	if h.KeyOf == nil {
		t.Fatal("storekit: Harness.KeyOf is required")
	}
	if h.PageSize <= 0 {
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
	s := h.NewStore(t)
	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("Ping on a healthy store: got error %v, want nil", err)
	}
}

func (h Harness[T, K]) testPutGetRoundTrip(t *testing.T) {
	ctx := t.Context()
	s := h.NewStore(t)
	want := h.NewItem(1)

	if err := s.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, found, err := s.Get(ctx, h.KeyOf(want))
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
	s := h.NewStore(t)

	var zero T
	got, found, err := s.Get(ctx, h.KeyOf(h.NewItem(99)))
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
	s := h.NewStore(t)

	first := h.NewItem(1)
	second := first
	if h.Mutate != nil {
		second = h.Mutate(first)
	}
	if err := s.Put(ctx, first); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := s.Put(ctx, second); err != nil {
		t.Fatalf("second Put on the same key must overwrite, not error: %v", err)
	}

	got, found, err := s.Get(ctx, h.KeyOf(first))
	if err != nil || !found {
		t.Fatalf("Get after overwrite: found=%v err=%v, want true/nil", found, err)
	}
	if !h.equal(got, second) {
		t.Fatalf("Get after overwrite returned %#v, want the second value %#v", got, second)
	}

	all := h.sweep(t, ctx, s)
	if len(all) != 1 {
		t.Fatalf("overwrite must not duplicate: List returned %d items, want 1", len(all))
	}
}

func (h Harness[T, K]) testDelete(t *testing.T) {
	ctx := t.Context()
	s := h.NewStore(t)

	item := h.NewItem(1)
	other := h.NewItem(2)
	for _, it := range []T{item, other} {
		if err := s.Put(ctx, it); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := s.Delete(ctx, h.KeyOf(item)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, err := s.Get(ctx, h.KeyOf(item)); err != nil || found {
		t.Fatalf("Get after Delete: found=%v err=%v, want false/nil", found, err)
	}
	if _, found, err := s.Get(ctx, h.KeyOf(other)); err != nil || !found {
		t.Fatalf("Delete removed an unrelated key: found=%v err=%v, want true/nil", found, err)
	}
	if all := h.sweep(t, ctx, s); len(all) != 1 {
		t.Fatalf("after deleting 1 of 2, List returned %d items, want 1", len(all))
	}
}

func (h Harness[T, K]) testDeleteMissingKeyIsNoOp(t *testing.T) {
	ctx := t.Context()
	s := h.NewStore(t)

	// Contract: Delete is idempotent. Deleting an absent key returns nil.
	key := h.KeyOf(h.NewItem(99))
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete on a missing key must return nil (Delete is idempotent), got %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("second Delete on a missing key must return nil, got %v", err)
	}

	item := h.NewItem(1)
	if err := s.Put(ctx, item); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, h.KeyOf(item)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, h.KeyOf(item)); err != nil {
		t.Fatalf("re-deleting an already deleted key must return nil, got %v", err)
	}
}

func (h Harness[T, K]) testListEmpty(t *testing.T) {
	ctx := t.Context()
	s := h.NewStore(t)

	page, next, err := s.List(ctx, "")
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
	s := h.NewStore(t)
	seeded := h.seed(t, ctx, s)

	first := h.keys(h.sweep(t, ctx, s))
	second := h.keys(h.sweep(t, ctx, s))

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
	s := h.NewStore(t)
	seeded := h.seed(t, ctx, s)

	seen := make(map[K]int, len(seeded))
	pages := 0
	var cur Cursor
	for {
		page, next, err := s.List(ctx, cur)
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

func (h Harness[T, K]) testContextCanceled(t *testing.T) {
	setup := func(t *testing.T) (Store[T, K], T) {
		t.Helper()
		s := h.NewStore(t)
		item := h.NewItem(1)
		if err := s.Put(t.Context(), item); err != nil {
			t.Fatalf("Put during setup: %v", err)
		}
		return s, item
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

	t.Run("Put", func(t *testing.T) {
		s, _ := setup(t)
		check(t, "Put", s.Put(cancelled(), h.NewItem(2)))
		if _, found, err := s.Get(t.Context(), h.KeyOf(h.NewItem(2))); err != nil || found {
			t.Fatalf("Put on a cancelled context must not mutate the store: found=%v err=%v", found, err)
		}
	})
	t.Run("Get", func(t *testing.T) {
		s, item := setup(t)
		_, _, err := s.Get(cancelled(), h.KeyOf(item))
		check(t, "Get", err)
	})
	t.Run("List", func(t *testing.T) {
		s, _ := setup(t)
		_, _, err := s.List(cancelled(), "")
		check(t, "List", err)
	})
	t.Run("Delete", func(t *testing.T) {
		s, item := setup(t)
		check(t, "Delete", s.Delete(cancelled(), h.KeyOf(item)))
		if _, found, err := s.Get(t.Context(), h.KeyOf(item)); err != nil || !found {
			t.Fatalf("Delete on a cancelled context must not mutate the store: found=%v err=%v", found, err)
		}
	})
	t.Run("Ping", func(t *testing.T) {
		s, _ := setup(t)
		check(t, "Ping", s.Ping(cancelled()))
	})
}

// seed fills the store with 2*PageSize+1 items so pagination spans at least
// three pages, and returns them.
func (h Harness[T, K]) seed(t *testing.T, ctx context.Context, s Store[T, K]) []T {
	t.Helper()
	n := 2*h.PageSize + 1
	items := make([]T, 0, n)
	for i := range n {
		item := h.NewItem(i)
		if err := s.Put(ctx, item); err != nil {
			t.Fatalf("Put(seed %d): %v", i, err)
		}
		items = append(items, item)
	}
	return items
}

// sweep walks every page and returns the items in the order List yielded them.
func (h Harness[T, K]) sweep(t *testing.T, ctx context.Context, s Store[T, K]) []T {
	t.Helper()
	var (
		out   []T
		cur   Cursor
		calls int
	)
	for {
		page, next, err := s.List(ctx, cur)
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
