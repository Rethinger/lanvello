package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func chunk(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUsageFromResponses(t *testing.T) {
	u := UsageFromResponses(map[string]any{
		"input_tokens":  10,
		"output_tokens": 20,
		"total_tokens":  30,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": 12,
		},
		"input_tokens_details": map[string]any{"cached_tokens": 4},
	})
	if u["prompt_tokens"].(int64) != 10 || u["completion_tokens"].(int64) != 20 {
		t.Fatalf("%v", u)
	}
	det := u["completion_tokens_details"].(map[string]any)
	if det["reasoning_tokens"].(int64) != 12 {
		t.Fatalf("reasoning tokens are how effort becomes visible: %v", det)
	}
	pdet := u["prompt_tokens_details"].(map[string]any)
	if pdet["cached_tokens"].(int64) != 4 {
		t.Fatalf("%v", pdet)
	}
}

func TestUsageFromChatKeepsReasoning(t *testing.T) {
	u := UsageFromChat(map[string]any{
		"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11,
		"completion_tokens_details": map[string]any{"reasoning_tokens": 3},
	})
	det, _ := u["completion_tokens_details"].(map[string]any)
	if det == nil || det["reasoning_tokens"].(int64) != 3 {
		t.Fatalf("%v", u)
	}
	if UsageFromChat(nil) != nil {
		t.Fatal("no usage must stay nil, not an empty object")
	}
}

func TestStreamConvKeepsUpstreamIdentity(t *testing.T) {
	c := &StreamConv{}
	c.FeedEvent(map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": "resp_abc", "created_at": 999, "model": "muse-spark"},
	}, "ignored")
	chunks, done := c.FeedEvent(map[string]any{"type": "response.completed"}, "ignored")
	if !done || len(chunks) != 1 {
		t.Fatalf("done=%v chunks=%d", done, len(chunks))
	}
	got := chunks[0]
	if got["id"] != "chatcmpl-abc" {
		t.Fatalf("id=%v", got["id"])
	}
	if got["created"].(int64) != 999 {
		t.Fatalf("created=%v", got["created"])
	}
	if got["model"] != "muse-spark" {
		t.Fatalf("model=%v", got["model"])
	}
}

func TestStreamConvIncompleteIsLength(t *testing.T) {
	c := &StreamConv{}
	_, done := c.FeedEvent(map[string]any{
		"type":     "response.incomplete",
		"response": map[string]any{"status": "incomplete"},
	}, "m")
	if !done {
		t.Fatal("expected done")
	}
}

func TestAggregateResponsesCarriesUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_q","created_at":7}}`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7,"output_tokens_details":{"reasoning_tokens":2}}}}`,
	}, "\n\n")
	var v map[string]any
	if err := json.Unmarshal(AggregateResponsesConv([]byte(sse), "opencode/m"), &v); err != nil {
		t.Fatal(err)
	}
	u := v["usage"].(map[string]any)
	if u["prompt_tokens"].(float64) != 3 {
		t.Fatalf("usage=%v", u)
	}
	if v["created"].(float64) != 7 {
		t.Fatalf("created=%v", v["created"])
	}
	msg := v["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello" {
		t.Fatalf("content=%v", msg["content"])
	}
}

func TestAggregateOpenAICarriesUsageAndToolMerge(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-x","created":42,"choices":[{"index":0,"delta":{"content":"a"}}]}`,
		`data: {"id":"chatcmpl-x","created":42,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"id":"chatcmpl-x","created":42,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`data: {"id":"chatcmpl-x","created":42,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chatcmpl-x","created":42,"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":8,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":6}}}`,
		"data: [DONE]",
	}, "\n\n")
	var v map[string]any
	if err := json.Unmarshal(AggregateOpenAI([]byte(sse), "opencode/m"), &v); err != nil {
		t.Fatal(err)
	}
	choice := v["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish=%v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("argument fragments must merge by index: %v", calls)
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("args=%v", fn["arguments"])
	}
	u := v["usage"].(map[string]any)
	if u["total_tokens"].(float64) != 10 {
		t.Fatalf("usage=%v", u)
	}
	if v["created"].(float64) != 42 {
		t.Fatalf("created=%v", v["created"])
	}
}

func TestAggregateOpenAIKeepsReasoningContent(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{"reasoning_content":"thinking "}}]}`,
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{"content":"answer"}}]}`,
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}, "\n\n")
	var v map[string]any
	if err := json.Unmarshal(AggregateOpenAI([]byte(sse), "m"), &v); err != nil {
		t.Fatal(err)
	}
	msg := v["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "thinking " {
		t.Fatalf("reasoning lost: %v", msg)
	}
	if msg["content"] != "answer" {
		t.Fatalf("content=%v", msg["content"])
	}
}
