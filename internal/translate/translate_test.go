package translate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"lanvello/internal/translate"
)

func TestChatToResponses(t *testing.T) {
	chat := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "noted"},
		},
		"max_tokens": 5,
	}
	out := translate.ChatToResponses(chat)
	in, _ := out["input"].([]any)
	if len(in) != 3 {
		t.Fatalf("input len=%d", len(in))
	}
	acontent := in[2].(map[string]any)["content"].([]any)
	if acontent[0].(map[string]any)["type"] != "output_text" {
		t.Fatalf("assistant part: %v", acontent[0])
	}
	ucontent := in[1].(map[string]any)["content"].([]any)
	if ucontent[0].(map[string]any)["type"] != "input_text" {
		t.Fatalf("user part: %v", ucontent[0])
	}
	if out["max_output_tokens"] != 5 {
		t.Fatal("max tokens")
	}
	if out["stream"] != true || out["store"] != false {
		t.Fatal("stream/store")
	}
}

func TestAggregate(t *testing.T) {
	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"he\"}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"llo\"}\n\n"
	b := translate.AggregateResponses([]byte(sse), "m")
	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Choices[0].Message.Content != "hello" {
		t.Fatalf("got %q", v.Choices[0].Message.Content)
	}
}

func TestSSEChunk(t *testing.T) {
	lines, done := translate.ResponsesSSEToOpenAI(
		map[string]any{"type": "response.output_text.delta", "delta": "x"}, "m")
	if done || len(lines) != 1 || !strings.HasPrefix(lines[0], "data: ") {
		t.Fatalf("lines=%v done=%v", lines, done)
	}
	lines, done = translate.ResponsesSSEToOpenAI(map[string]any{"type": "response.completed"}, "m")
	if !done || lines[len(lines)-1] != "data: [DONE]" {
		t.Fatal("completed")
	}
}

func TestToolHistory(t *testing.T) {
	chat := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": "{}"},
				}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
		},
	}
	out := translate.ChatToResponses(chat)
	in, _ := out["input"].([]any)
	if len(in) != 3 {
		t.Fatalf("input len=%d", len(in))
	}
	if in[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("item1: %v", in[1])
	}
	if in[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("item2: %v", in[2])
	}
}

func TestToolCallFeed(t *testing.T) {
	c := &translate.StreamConv{}
	lines, done := c.Feed(map[string]any{
		"type": "response.output_item.added",
		"item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "write"},
	}, "m")
	if done || len(lines) != 1 || !strings.Contains(lines[0], "write") {
		t.Fatalf("added: %v %v", lines, done)
	}
	// upstream keys deltas by item id (fc_*), not call id.
	lines, done = c.Feed(map[string]any{
		"type":    "response.function_call_arguments.delta",
		"item_id": "fc_1", "delta": "{\"a\":1}",
	}, "m")
	if done || len(lines) != 1 || !strings.Contains(lines[0], "a") {
		t.Fatalf("delta: %v %v", lines, done)
	}
	lines, done = c.Feed(map[string]any{"type": "response.completed"}, "m")
	if !done {
		t.Fatal("not done")
	}
	if !strings.Contains(lines[0], "tool_calls") {
		t.Fatalf("finish: %v", lines)
	}
}
