package storekit_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/leftathome/go-service-kit/storekit"
)

// A real service's store is often NARROWER than storekit.Store. nagus's is
// Put/Get/Search(Query)/DeleteStale with a natural order of SeenAt DESC, which
// is not the total order the List clauses demand. Before these entry points it
// had two options: add three methods to three adapters purely for a test and
// change a live query's ORDER BY, or use none of the harness at all.

// pingOnlyStore is the smallest useful adapter: readiness and nothing else.
type pingOnlyStore struct {
	// ignoreCancellation models the bug PingHarness is there to catch.
	ignoreCancellation bool
	err                error
}

func (s pingOnlyStore) Ping(ctx context.Context) error {
	if !s.ignoreCancellation {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return s.err
}

var _ storekit.Pinger = pingOnlyStore{}

// nagusShapedStore has Put, Get and Ping. It has no List (its real read path is
// a rich Search) and no single-key Delete (its real one is a bulk DeleteStale).
type nagusShapedStore struct{ items map[string]widget }

func newNagusShapedStore() *nagusShapedStore {
	return &nagusShapedStore{items: make(map[string]widget)}
}

func (s *nagusShapedStore) Put(ctx context.Context, item widget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.items[item.ID] = item
	return nil
}

func (s *nagusShapedStore) Get(ctx context.Context, key string) (widget, bool, error) {
	if err := ctx.Err(); err != nil {
		return widget{}, false, err
	}
	w, ok := s.items[key]
	return w, ok, nil
}

func (s *nagusShapedStore) Ping(ctx context.Context) error { return ctx.Err() }

func nagusHarness() storekit.Harness[widget, string] {
	return storekit.Harness[widget, string]{
		NewAdapter: func(*testing.T) storekit.Adapter[widget, string] {
			s := newNagusShapedStore()
			return storekit.Adapter[widget, string]{Put: s.Put, Get: s.Get, Ping: s.Ping}
		},
		NewItem: func(seed int) widget {
			return widget{ID: fmt.Sprintf("wid-%04d", seed), Name: fmt.Sprintf("widget %d", seed)}
		},
		KeyOf: func(w widget) string { return w.ID },
	}
}

// The clauses a nagus-shaped store can actually promise.
var nagusClauses = []string{
	storekit.ClausePing,
	storekit.ClausePutGetRoundTrip,
	storekit.ClauseGetMissingKey,
	storekit.ClauseContextCanceled,
}

// --- PingHarness --------------------------------------------------------

// Readiness is the clause with the best value-to-cost ratio, and it must not
// require the whole five-method shape: a store that cannot be probed forces a
// service to hardcode /readyz, which is how a fleet reports healthy while every
// request fails.
func TestPingHarnessNeedsNothingButPing(t *testing.T) {
	storekit.PingHarness{
		NewStore: func(*testing.T) storekit.Pinger { return pingOnlyStore{} },
	}.Run(t)
}

const subsetEnv = "STOREKIT_SUBSET_CASE"

// The Ping-only path must still BITE. A harness that passes everything is
// worse than no harness.
func TestPingHarnessCatchesAnIgnoredCancellation(t *testing.T) {
	if os.Getenv(subsetEnv) == "ping-ignores-context" {
		storekit.PingHarness{
			NewStore: func(*testing.T) storekit.Pinger { return pingOnlyStore{ignoreCancellation: true} },
		}.Run(t)
		return
	}
	out, err := reexec(t, "TestPingHarnessCatchesAnIgnoredCancellation", "ping-ignores-context")
	if err == nil {
		t.Fatalf("PingHarness passed a store that ignores cancellation; output:\n%s", out)
	}
	if !strings.Contains(out, "ContextCanceled/Ping") {
		t.Fatalf("expected the ContextCanceled/Ping subtest to fail; output:\n%s", out)
	}
}

func TestPingHarnessRejectsBadWiring(t *testing.T) {
	if os.Getenv(subsetEnv) == "ping-no-factory" {
		storekit.PingHarness{}.Run(t)
		return
	}
	out, err := reexec(t, "TestPingHarnessRejectsBadWiring", "ping-no-factory")
	if err == nil {
		t.Fatalf("PingHarness accepted a nil NewStore; output:\n%s", out)
	}
	if !strings.Contains(out, "PingHarness.NewStore is required") {
		t.Fatalf("expected a NewStore diagnostic; output:\n%s", out)
	}
}

// --- partial adapters ---------------------------------------------------

// The clauses a nagus-shaped store DOES promise still run, and still pass.
func TestRunClausesGatesTheSubsetThatApplies(t *testing.T) {
	nagusHarness().RunClauses(t, nagusClauses...)
}

// A partial adapter given to Run gets the clauses it supports and a SKIP,
// naming the missing operation, for the ones it does not. A skip is visible in
// `go test -v` and in CI output; silently reduced coverage is not.
func TestRunSkipsClausesTheAdapterCannotSupport(t *testing.T) {
	if os.Getenv(subsetEnv) == "partial-run" {
		nagusHarness().Run(t)
		return
	}
	out, err := reexec(t, "TestRunSkipsClausesTheAdapterCannotSupport", "partial-run")
	if err != nil {
		t.Fatalf("Run on a partial adapter failed; it should skip what it cannot run:\n%s", out)
	}
	for _, want := range []string{
		"--- PASS: TestRunSkipsClausesTheAdapterCannotSupport/PutGetRoundTrip",
		"--- PASS: TestRunSkipsClausesTheAdapterCannotSupport/GetMissingKey",
		"--- PASS: TestRunSkipsClausesTheAdapterCannotSupport/Ping",
		"--- SKIP: TestRunSkipsClausesTheAdapterCannotSupport/ListPaginates",
		"--- SKIP: TestRunSkipsClausesTheAdapterCannotSupport/Delete",
		"adapter supplies no [List]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// ContextCanceled adapts rather than skipping: cancellation is a
	// per-method promise, so three methods can honor it.
	for _, want := range []string{
		"--- PASS: TestRunSkipsClausesTheAdapterCannotSupport/ContextCanceled/Put",
		"--- PASS: TestRunSkipsClausesTheAdapterCannotSupport/ContextCanceled/Get",
		"--- PASS: TestRunSkipsClausesTheAdapterCannotSupport/ContextCanceled/Ping",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ContextCanceled/List") {
		t.Errorf("ContextCanceled ran a List subtest against an adapter with no List:\n%s", out)
	}
}

// Naming a clause the adapter cannot support is a FAILURE, not a skip. Asking
// for a clause by name and silently not running it is the one outcome worse
// than not running it.
func TestRunClausesFailsWhenANamedClauseCannotRun(t *testing.T) {
	if os.Getenv(subsetEnv) == "named-unsupported" {
		nagusHarness().RunClauses(t, storekit.ClausePing, storekit.ClauseListPaginates)
		return
	}
	out, err := reexec(t, "TestRunClausesFailsWhenANamedClauseCannotRun", "named-unsupported")
	if err == nil {
		t.Fatalf("RunClauses silently skipped an explicitly named clause; output:\n%s", out)
	}
	if !strings.Contains(out, "clause ListPaginates was requested but the adapter supplies no [List]") {
		t.Fatalf("expected a diagnostic naming the clause and the missing operation; output:\n%s", out)
	}
}

func TestRunClausesRejectsAnUnknownName(t *testing.T) {
	if os.Getenv(subsetEnv) == "unknown-clause" {
		nagusHarness().RunClauses(t, "PutGetRoundtrip") // wrong case
		return
	}
	out, err := reexec(t, "TestRunClausesRejectsAnUnknownName", "unknown-clause")
	if err == nil {
		t.Fatalf("RunClauses accepted an unknown clause name; output:\n%s", out)
	}
	if !strings.Contains(out, "unknown clause") {
		t.Fatalf("expected an unknown-clause diagnostic; output:\n%s", out)
	}
}

func TestBothFactoriesIsAWiringError(t *testing.T) {
	if os.Getenv(subsetEnv) == "both-factories" {
		h := refHarness()
		h.NewAdapter = func(*testing.T) storekit.Adapter[widget, string] {
			return storekit.AdapterFor[widget, string](newMemStore(3))
		}
		h.Run(t)
		return
	}
	out, err := reexec(t, "TestBothFactoriesIsAWiringError", "both-factories")
	if err == nil {
		t.Fatalf("harness accepted both NewStore and NewAdapter; output:\n%s", out)
	}
	if !strings.Contains(out, "exactly one of Harness.NewStore and Harness.NewAdapter") {
		t.Fatalf("expected a factory diagnostic; output:\n%s", out)
	}
}

// A Ping-only adapter needs no NewItem and no KeyOf: validation must demand
// only what the clauses that will actually run require.
func TestPingOnlyAdapterNeedsNoFixtures(t *testing.T) {
	storekit.Harness[widget, string]{
		NewAdapter: func(*testing.T) storekit.Adapter[widget, string] {
			return storekit.Adapter[widget, string]{Ping: pingOnlyStore{}.Ping}
		},
	}.RunClauses(t, storekit.ClausePing, storekit.ClauseContextCanceled)
}

// --- the full path is unchanged ----------------------------------------

// RunClauses over every clause must be exactly Run, and AdapterFor must lift a
// full Store without loss. Together these are the "existing consumers keep
// working" assertion.
func TestFullStoreViaAdapterForRunsEveryClause(t *testing.T) {
	h := refHarness()
	h.NewStore = nil
	h.NewAdapter = func(*testing.T) storekit.Adapter[widget, string] {
		return storekit.AdapterFor[widget, string](newMemStore(3))
	}
	h.RunClauses(t, storekit.Clauses()...)
}

func TestClausesIsCompleteAndUnique(t *testing.T) {
	got := storekit.Clauses()
	if len(got) != 10 {
		t.Errorf("Clauses() has %d entries, want 10: %v", len(got), got)
	}
	seen := make(map[string]bool, len(got))
	for _, c := range got {
		if seen[c] {
			t.Errorf("Clauses() repeats %q", c)
		}
		seen[c] = true
	}
	for _, c := range []string{
		storekit.ClausePing, storekit.ClausePutGetRoundTrip, storekit.ClauseGetMissingKey,
		storekit.ClausePutOverwrites, storekit.ClauseDelete, storekit.ClauseDeleteMissingKeyIsNoOp,
		storekit.ClauseListEmpty, storekit.ClauseListStableOrder, storekit.ClauseListPaginates,
		storekit.ClauseContextCanceled,
	} {
		if !slices.Contains(got, c) {
			t.Errorf("Clauses() omits %q", c)
		}
	}
}

func TestRunClausesNeedsAtLeastOneName(t *testing.T) {
	if os.Getenv(subsetEnv) == "no-names" {
		nagusHarness().RunClauses(t)
		return
	}
	out, err := reexec(t, "TestRunClausesNeedsAtLeastOneName", "no-names")
	if err == nil {
		t.Fatalf("RunClauses() with no names did not fail; output:\n%s", out)
	}
	if !strings.Contains(out, "at least one clause name") {
		t.Fatalf("expected a diagnostic; output:\n%s", out)
	}
}

// The single-clause interfaces compose back into Store with the same method
// set, so an existing adapter that declares `var _ storekit.Store[...]` keeps
// compiling.
func TestStoreStillComposesFromTheClauseInterfaces(t *testing.T) {
	var s storekit.Store[widget, string] = newMemStore(3)

	// Store's method set is unchanged, so an existing adapter's
	// `var _ storekit.Store[...]` assertion keeps compiling -- and each clause
	// is now nameable on its own.
	var (
		_ storekit.Putter[widget]         = s
		_ storekit.Getter[widget, string] = s
		_ storekit.Lister[widget]         = s
		_ storekit.Deleter[string]        = s
		_ storekit.Pinger                 = s
		_ storekit.Pinger                 = pingOnlyStore{}
		_ storekit.Putter[widget]         = newNagusShapedStore()
		_ storekit.Getter[widget, string] = newNagusShapedStore()
	)
	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// reexec runs one test of this binary with the case marker set, which is the
// only way to observe a *testing.T failure.
func reexec(t *testing.T, testName, caseName string) (string, error) {
	t.Helper()
	// #nosec G204,G702 -- os.Args[0] is this test binary and the arguments come
	// from constants in this file, not from external input.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+testName+"$", "-test.v")
	cmd.Env = append(os.Environ(), subsetEnv+"="+caseName)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
