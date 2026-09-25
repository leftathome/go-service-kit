package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

type idArgs struct {
	ID string `json:"id"`
}

// The outer envelope is validated as a single JSON value before a tool ever
// sees its arguments, so trailing data cannot arrive over HTTP today. The
// argument check refuses it anyway, so its strictness does not depend on the
// order of checks in ServeHTTP.
func TestCheckArgumentsRejectsTrailingData(t *testing.T) {
	tool := NewTool(ToolSpec{
		Name: "get", Description: "d", Access: ReadOnly,
		InputSchema: map[string]any{"properties": map[string]any{"id": map[string]any{"type": "string"}}},
	}, func(context.Context, idArgs) (Result, error) { return NotFound(), nil })
	if tool.err != nil {
		t.Fatal(tool.err)
	}
	for _, raw := range []string{`{"id":"a"} {"id":"b"}`, `{"id":"a"} x`, `{"id":"a"`} {
		if _, err := tool.checkArguments(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if _, err := tool.checkArguments(json.RawMessage(` {"id":"a"} `)); err != nil {
		t.Errorf("refused a well-formed object: %v", err)
	}
}
