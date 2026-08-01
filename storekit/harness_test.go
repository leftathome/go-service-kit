package storekit_test

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/leftathome/go-service-kit/storekit"
)

// This file is the reference implementation implementers copy.
//
// memStore below is a complete, correct adapter: it is what a service's
// in-memory store should look like, and it is what the harness is proving.
// The broken variants further down exist only to prove the harness assertions
// actually bite.

// widget is a stand-in for a service's own entity type.
type widget struct {
	ID   string
	Name string
}

// memStore is the reference in-memory adapter. Copy this shape.
//
// Ordering: List returns items sorted by key, ascending. That order is total
// and stable, which is what the harness requires; a SQL adapter would get the
// same property from "ORDER BY id".
//
// Cursor: the cursor is an opaque token. This adapter encodes the last key of
// the page returned; callers must not parse it. Encoding rather than returning
// the bare key is deliberate -- it keeps clients from depending on the format.
type memStore struct {
	mu       sync.RWMutex
	items    map[string]widget
	pageSize int
}

var _ storekit.Store[widget, string] = (*memStore)(nil)

func newMemStore(pageSize int) *memStore {
	return &memStore{items: make(map[string]widget), pageSize: pageSize}
}

func (s *memStore) Put(ctx context.Context, item widget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[item.ID] = item // same key overwrites
	return nil
}

func (s *memStore) Get(ctx context.Context, key string) (widget, bool, error) {
	if err := ctx.Err(); err != nil {
		return widget{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.items[key]
	return item, ok, nil
}

func (s *memStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, key) // deleting a missing key is a no-op, not an error
	return nil
}

func (s *memStore) Ping(ctx context.Context) error {
	return ctx.Err()
}

func (s *memStore) List(ctx context.Context, cur storekit.Cursor) ([]widget, storekit.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]string, 0, len(s.items))
	for k := range s.items {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	start := 0
	if cur != "" {
		after, err := decodeCursor(cur)
		if err != nil {
			return nil, "", err
		}
		// First key strictly greater than the cursor.
		start, _ = slices.BinarySearchFunc(keys, after, func(a, b string) int {
			if cmp.Compare(a, b) <= 0 {
				return -1
			}
			return 1
		})
	}

	end := min(start+s.pageSize, len(keys))
	page := make([]widget, 0, end-start)
	for _, k := range keys[start:end] {
		page = append(page, s.items[k])
	}
	var next storekit.Cursor
	if end < len(keys) {
		next = encodeCursor(keys[end-1])
	}
	return page, next, nil
}

func encodeCursor(key string) storekit.Cursor {
	return storekit.Cursor(base64.RawURLEncoding.EncodeToString([]byte(key)))
}

func decodeCursor(cur storekit.Cursor) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(string(cur))
	if err != nil {
		return "", fmt.Errorf("storekit_test: malformed cursor: %w", err)
	}
	return string(raw), nil
}

const refPageSize = 3

// refHarness is the harness wiring a service copies verbatim.
func refHarness() storekit.Harness[widget, string] {
	return storekit.Harness[widget, string]{
		NewStore: func(_ *testing.T) storekit.Store[widget, string] {
			return newMemStore(refPageSize)
		},
		NewItem: func(seed int) widget {
			return widget{ID: fmt.Sprintf("wid-%04d", seed), Name: fmt.Sprintf("widget %d", seed)}
		},
		KeyOf:    func(w widget) string { return w.ID },
		PageSize: refPageSize,
		Mutate:   func(w widget) widget { w.Name += " (revised)"; return w },
	}
}

func TestMemStoreSatisfiesContract(t *testing.T) {
	refHarness().Run(t)
}

// --- proof that the harness assertions bite -------------------------------

// Each broken adapter violates exactly one clause of the contract. The harness
// must fail on it. Because a failing *testing.T cannot be observed from within
// the same test binary run, the failing cases are executed in a subprocess.

const brokenEnv = "STOREKIT_TEST_BROKEN_ADAPTER"

type brokenNoOverwrite struct{ *memStore }

func (s *brokenNoOverwrite) Put(ctx context.Context, item widget) error {
	if _, ok, err := s.memStore.Get(ctx, item.ID); err != nil || ok {
		return err
	}
	return s.memStore.Put(ctx, item)
}

type brokenIgnoresContext struct{ *memStore }

func (s *brokenIgnoresContext) Put(_ context.Context, item widget) error {
	return s.memStore.Put(context.Background(), item)
}

func (s *brokenIgnoresContext) Get(_ context.Context, key string) (widget, bool, error) {
	return s.memStore.Get(context.Background(), key)
}

func (s *brokenIgnoresContext) Delete(_ context.Context, key string) error {
	return s.memStore.Delete(context.Background(), key)
}

func (s *brokenIgnoresContext) Ping(_ context.Context) error { return nil }

func (s *brokenIgnoresContext) List(_ context.Context, cur storekit.Cursor) ([]widget, storekit.Cursor, error) {
	return s.memStore.List(context.Background(), cur)
}

// brokenPagination hands back a cursor pointing one item too far back, so the
// last item of every page repeats as the first item of the next.
type brokenPagination struct{ *memStore }

func (s *brokenPagination) List(ctx context.Context, cur storekit.Cursor) ([]widget, storekit.Cursor, error) {
	page, next, err := s.memStore.List(ctx, cur)
	if err != nil || next == "" || len(page) < 2 {
		return page, next, err
	}
	return page, encodeCursor(page[len(page)-2].ID), nil
}

type brokenDeleteMissing struct{ *memStore }

func (s *brokenDeleteMissing) Delete(ctx context.Context, key string) error {
	if _, ok, err := s.memStore.Get(ctx, key); err != nil {
		return err
	} else if !ok {
		return errors.New("no such key")
	}
	return s.memStore.Delete(ctx, key)
}

func brokenStore(name string) storekit.Store[widget, string] {
	base := newMemStore(refPageSize)
	switch name {
	case "no-overwrite":
		return &brokenNoOverwrite{base}
	case "ignores-context":
		return &brokenIgnoresContext{base}
	case "bad-pagination":
		return &brokenPagination{base}
	case "delete-missing-errors":
		return &brokenDeleteMissing{base}
	default:
		panic("unknown broken adapter: " + name)
	}
}

func TestHarnessRejectsBrokenAdapters(t *testing.T) {
	// Subprocess arm: run the harness against the broken adapter and fail.
	if name := os.Getenv(brokenEnv); name != "" {
		h := refHarness()
		h.NewStore = func(_ *testing.T) storekit.Store[widget, string] { return brokenStore(name) }
		h.Run(t)
		return
	}

	cases := []struct {
		adapter  string
		wantFail string // harness subtest name expected to fail
	}{
		{"no-overwrite", "PutOverwrites"},
		{"ignores-context", "ContextCanceled"},
		{"bad-pagination", "ListPaginates"},
		{"delete-missing-errors", "DeleteMissingKeyIsNoOp"},
	}
	for _, tc := range cases {
		t.Run(tc.adapter, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestHarnessRejectsBrokenAdapters", "-test.v")
			cmd.Env = append(os.Environ(), brokenEnv+"="+tc.adapter)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("harness passed a broken adapter (%s); output:\n%s", tc.adapter, out)
			}
			if !strings.Contains(string(out), "--- FAIL: TestHarnessRejectsBrokenAdapters/"+tc.wantFail) {
				t.Fatalf("expected subtest %q to fail for adapter %s; output:\n%s", tc.wantFail, tc.adapter, out)
			}
		})
	}
}

func TestHarnessRejectsBadWiring(t *testing.T) {
	// A misconfigured harness must say so loudly rather than silently skipping
	// coverage. Checked in-process via the same subprocess trick.
	if os.Getenv(brokenEnv+"_WIRING") != "" {
		h := refHarness()
		h.PageSize = 0
		h.Run(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestHarnessRejectsBadWiring", "-test.v")
	cmd.Env = append(os.Environ(), brokenEnv+"_WIRING=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("harness accepted PageSize=0; output:\n%s", out)
	}
	if !strings.Contains(string(out), "PageSize") {
		t.Fatalf("expected a PageSize diagnostic; output:\n%s", out)
	}
}

func TestCursorZeroValue(t *testing.T) {
	var c storekit.Cursor
	if c != "" {
		t.Fatalf("zero Cursor should be the empty string, got %q", c)
	}
	if got := encodeCursor("wid-0001"); got == "" {
		t.Fatal("encoded cursor should be non-empty")
	}
}
