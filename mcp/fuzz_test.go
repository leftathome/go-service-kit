package mcp_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leftathome/go-service-kit/mcp"
)

// FuzzServeHTTP: whatever arrives, the server answers 200 with one well-formed
// JSON-RPC response, 202 with no body, or 413; and no response ever contains
// the planted probe outside structuredContent.
func FuzzServeHTTP(f *testing.F) {
	for _, seed := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_item","arguments":{"id":"a"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_items","arguments":{"text":"x","limit":1}}}`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":{},"method":"ping"}`,
		`{`,
		``,
	} {
		f.Add(seed)
	}
	fx := newFixture()
	srv, err := mcp.New(mcp.Options{Name: "fuzz", Logger: slog.New(slog.DiscardHandler)}, fx.tools()...)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, body string) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusAccepted:
			if rec.Body.Len() != 0 {
				t.Fatalf("202 with a body: %q", rec.Body.String())
			}
			return
		case http.StatusOK, http.StatusRequestEntityTooLarge:
		default:
			t.Fatalf("status %d", rec.Code)
		}
		var env struct {
			JSONRPC string          `json:"jsonrpc"`
			Result  json.RawMessage `json:"result"`
			Error   *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.JSONRPC != "2.0" {
			t.Fatalf("malformed response %q: %v", rec.Body.String(), err)
		}
		if (env.Error == nil) == (env.Result == nil) {
			t.Fatalf("response must carry exactly one of result and error: %q", rec.Body.String())
		}
		if env.Error != nil && strings.Contains(env.Error.Message, "IGNORE") {
			t.Fatalf("error echoes the probe: %q", env.Error.Message)
		}
		var res struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if env.Result != nil && json.Unmarshal(env.Result, &res) == nil {
			for _, c := range res.Content {
				if strings.Contains(c.Text, "IGNORE") {
					t.Fatalf("text block carries the probe: %q", c.Text)
				}
			}
		}
	})
}
