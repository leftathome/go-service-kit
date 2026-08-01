package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/leftathome/go-service-kit/httpapi"
	"github.com/leftathome/go-service-kit/storekit"
)

// The envelope is fixed so that N services do not invent N pagination schemes.
// Its JSON member names are part of the house contract; changing them is a
// breaking change for every client in the fleet.
func TestPageEnvelopeShape(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(httpapi.NewPage([]string{"a", "b"}, "cur-2"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"items":["a","b"],"next_cursor":"cur-2","has_more":true}`; got != want {
		t.Errorf("page JSON = %s, want %s", got, want)
	}
}

// An empty page must serialize as [] and not null: `null.length` is the single
// most common client-side crash in a paginated API.
func TestEmptyPageSerializesAsAnEmptyArray(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(httpapi.NewPage[string, string](nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"items":[],"has_more":false}`; got != want {
		t.Errorf("empty page JSON = %s, want %s", got, want)
	}
}

func TestHasMoreTracksNextCursor(t *testing.T) {
	t.Parallel()

	if p := httpapi.NewPage([]int{1}, ""); p.HasMore {
		t.Error("HasMore = true with an empty cursor")
	}
	if p := httpapi.NewPage([]int{1}, "more"); !p.HasMore || p.NextCursor != "more" {
		t.Errorf("page = %+v, want HasMore with NextCursor", p)
	}
}

// storekit.Cursor is what the store layer already returns. The envelope must
// accept it without either package importing the other: the kit's dependency
// direction rule allows no edges between sibling packages.
func TestNewPageAcceptsAStorekitCursorWithoutAnImportEdge(t *testing.T) {
	t.Parallel()

	var next storekit.Cursor = "opaque-token"
	p := httpapi.NewPage([]int{1, 2, 3}, next)
	if p.NextCursor != "opaque-token" {
		t.Errorf("NextCursor = %q", p.NextCursor)
	}
	if !p.HasMore {
		t.Error("HasMore = false")
	}
}

func TestEffectiveLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   int
		want int
	}{
		{"unset falls back to the default", 0, httpapi.DefaultPageSize},
		{"negative falls back to the default", -1, httpapi.DefaultPageSize},
		{"in range is honored", 7, 7},
		{"over the cap is clamped", httpapi.MaxPageSize + 1000, httpapi.MaxPageSize},
		{"at the cap is honored", httpapi.MaxPageSize, httpapi.MaxPageSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := (httpapi.PageParams{Limit: tc.in}).EffectiveLimit(); got != tc.want {
				t.Errorf("EffectiveLimit(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// Limit is validated by the schema, so an out-of-range value is a 422 with a
// per-field location rather than a silent clamp. Silent clamping is how a
// client ends up paging forever without noticing.
func TestLimitOutOfRangeIsRejectedBySchemaValidation(t *testing.T) {
	t.Parallel()

	h := newFixtureAPI(t, false).Handler()
	for _, q := range []string{"?limit=0", "?limit=-3", "?limit=100000"} {
		rec := do(t, h, http.MethodGet, "/widgets"+q)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("GET /widgets%s = %d, want 422: %s", q, rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, h, http.MethodGet, "/widgets?limit=10"); rec.Code != http.StatusOK {
		t.Errorf("GET /widgets?limit=10 = %d, want 200", rec.Code)
	}
}

// A cursor is opaque, but it is also attacker-controlled input that a store
// adapter will decode. Bound it in the schema so a multi-megabyte cursor is
// rejected before any handler sees it.
func TestOversizedCursorIsRejected(t *testing.T) {
	t.Parallel()

	h := newFixtureAPI(t, false).Handler()
	rec := do(t, h, http.MethodGet, "/widgets?cursor="+strings.Repeat("a", httpapi.MaxCursorLength+1))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("oversized cursor = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	ok := do(t, h, http.MethodGet, "/widgets?cursor="+strings.Repeat("a", httpapi.MaxCursorLength))
	if ok.Code != http.StatusOK {
		t.Errorf("max-length cursor = %d, want 200: %s", ok.Code, ok.Body.String())
	}
}
