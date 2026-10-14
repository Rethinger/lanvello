package server

import (
	"encoding/json"
	"testing"
)

// The parser is the boundary between an api caller's json and what the
// client-side plugin rebuilds the model call from: a mistake here is invisible
// in the answer and shows up as the client running its own agent.
func TestParseChatRequest(t *testing.T) {
	chat := map[string]any{
		"reasoning_effort": "high",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "system", "content": "answer in french"},
			map[string]any{"role": "user", "content": "weather in paris?"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "get_weather",
						"arguments": `{"city":"paris"}`,
					},
				},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "18C"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "current weather",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{"city": map[string]any{"type": "string"}},
					},
				},
			},
			map[string]any{"name": "ping", "description": "flat shape"},
		},
	}

	got := parseChatRequest(chat)

	if got.System != "be brief\n\nanswer in french" {
		t.Errorf("system = %q, want both system messages joined", got.System)
	}
	if got.Variant != "high" {
		t.Errorf("variant = %q, want high", got.Variant)
	}
	// the prompt is the trigger the client runs: the last user text, not the
	// tool result that follows it
	if got.Prompt != "weather in paris?" {
		t.Errorf("prompt = %q, want the last user text", got.Prompt)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, tool)", len(got.Messages))
	}
	am := got.Messages[1]
	if am.Role != "assistant" || len(am.ToolCalls) != 1 {
		t.Fatalf("assistant %+v, want one tool call", am)
	}
	if tc := am.ToolCalls[0]; tc.ID != "call_1" || tc.Name != "get_weather" || string(tc.Arguments) != `{"city":"paris"}` {
		t.Errorf("tool call %+v, want the caller's id, name and arguments", tc)
	}
	if tm := got.Messages[2]; tm.Role != "tool" || tm.ToolCallID != "call_1" || tm.Content != "18C" {
		t.Errorf("tool message %+v, want the result tied to call_1", tm)
	}

	if len(got.Tools) != 2 {
		t.Fatalf("tools = %d, want 2 (wrapped and flat)", len(got.Tools))
	}
	if w := got.Tools[0]; w.Name != "get_weather" || w.Description != "current weather" {
		t.Errorf("tool %+v, want the wrapped function unpacked", w)
	}
	var schema map[string]any
	if err := json.Unmarshal(got.Tools[0].Parameters, &schema); err != nil {
		t.Fatalf("parameters are not json: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("parameters = %s, want the caller's schema through", got.Tools[0].Parameters)
	}
	// a tool with no schema still has to carry a valid object schema: the
	// client refuses a definition without one
	if p := string(got.Tools[1].Parameters); p != `{"type":"object","properties":{}}` {
		t.Errorf("parameters of a schema-less tool = %s, want an empty object schema", p)
	}
}

// An api caller that sends no tools must not inherit the client's own.
func TestParseChatRequestWithoutTools(t *testing.T) {
	got := parseChatRequest(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if len(got.Tools) != 0 {
		t.Errorf("tools = %d, want none", len(got.Tools))
	}
	if got.System != "" {
		t.Errorf("system = %q, want empty", got.System)
	}
	if got.Prompt != "hi" {
		t.Errorf("prompt = %q, want hi", got.Prompt)
	}
}
