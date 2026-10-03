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
		},
		"max_tokens": 5,
	}
	out := translate.ChatToResponses(chat)
	in, _ := out["input"].([]any)
	if len(in) != 2 {
		t.Fatalf("input len=%d", len(in))
	}
	if out["max_output_tokens"] != 5 {
		t.Fatal("max tokens")
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
