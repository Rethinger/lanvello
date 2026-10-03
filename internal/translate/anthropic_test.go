package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func known(ids ...string) func(string) bool {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return func(id string) bool { return set[id] }
}

func TestMessagesToChatBasics(t *testing.T) {
	body := map[string]any{
		"model":      "claude-sonnet-4",
		"max_tokens": 512,
		"system":     "be brief",
		"stream":     true,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "hello"},
		},
		"stop_sequences": []any{"END"},
	}
	out := MessagesToChat(body, known("fledge-alpha-free"), "fledge-alpha-free")
	if out["model"] != "fledge-alpha-free" {
		t.Fatalf("unknown model must fall back: %v", out["model"])
	}
	if out["max_tokens"].(int) != 512 || out["stop"].([]any)[0] != "END" {
		t.Fatalf("%v", out)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("system + 2 messages expected: %d", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("system first: %v", msgs[0])
	}
}

func TestMessagesToChatKeepsServedModel(t *testing.T) {
	out := MessagesToChat(map[string]any{
		"model":    "opencode/fledge-alpha-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, known("fledge-alpha-free"), "muse-spark-1.3-contributor-free")
	if out["model"] != "fledge-alpha-free" {
		t.Fatalf("model=%v", out["model"])
	}
}

func TestMessagesToChatEffortFromThinkingBudget(t *testing.T) {
	for budget, want := range map[int]string{1024: "low", 4096: "medium", 32000: "high", 0: "minimal"} {
		body := map[string]any{
			"model":    "fledge-alpha-free",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"thinking": map[string]any{"type": "enabled", "budget_tokens": budget},
		}
		out := MessagesToChat(body, known("fledge-alpha-free"), "fledge-alpha-free")
		if out["reasoning_effort"] != want {
			t.Fatalf("budget=%d got %v want %s", budget, out["reasoning_effort"], want)
		}
	}
}

func TestMessagesToChatToolChoice(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want any
	}{
		{map[string]any{"type": "auto"}, "auto"},
		{map[string]any{"type": "any"}, "required"},
		{map[string]any{"type": "none"}, "none"},
	}
	for _, c := range cases {
		out := MessagesToChat(map[string]any{
			"model":       "fledge-alpha-free",
			"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
			"tool_choice": c.in,
		}, known("fledge-alpha-free"), "fledge-alpha-free")
		got := out["tool_choice"]
		if c.want == "required" || c.want == "auto" || c.want == "none" {
			if got != c.want {
				t.Fatalf("%v -> %v want %v", c.in, got, c.want)
			}
			continue
		}
		t.Fatalf("unexpected case")
	}
	out := MessagesToChat(map[string]any{
		"model":       "fledge-alpha-free",
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"tool_choice": map[string]any{"type": "tool", "name": "bash"},
	}, known("fledge-alpha-free"), "fledge-alpha-free")
	tc := out["tool_choice"].(map[string]any)
	if tc["type"] != "function" {
		t.Fatalf("named tool choice: %v", tc)
	}
}

func TestMessagesToChatBlocks(t *testing.T) {
	out := MessagesToChat(map[string]any{
		"model": "fledge-alpha-free",
		"messages": []any{
			map[string]any{"role": "user", "content": "run it"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "calling"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "bash",
					"input": map[string]any{"cmd": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1",
					"content": []any{map[string]any{"type": "text", "text": "a.txt"}}},
			}},
		},
		"tools": []any{map[string]any{
			"name": "bash", "description": "run",
			"input_schema": map[string]any{"type": "object"},
		}},
	}, known("fledge-alpha-free"), "fledge-alpha-free")
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages=%d", len(msgs))
	}
	asst := msgs[1].(map[string]any)
	calls := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_use -> tool_calls: %v", asst)
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("input must be serialized: %v", fn["arguments"])
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" || tool["content"] != "a.txt" {
		t.Fatalf("tool_result block: %v", tool)
	}
	tools := out["tools"].([]any)
	fnT := tools[0].(map[string]any)["function"].(map[string]any)
	if fnT["name"] != "bash" {
		t.Fatalf("input_schema must become parameters: %v", fnT)
	}
}

func TestAnthropicSinkSequence(t *testing.T) {
	s := NewAnthropicSink("claude-x", false)
	var body strings.Builder
	emit := func(lines []string) {
		for _, l := range lines {
			body.WriteString(l + "\n")
		}
	}
	emit(s.Chunk(chunkMap(t, `{"id":"c","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"He"}}]}`)))
	emit(s.Chunk(chunkMap(t, `{"id":"c","created":1,"choices":[{"index":0,"delta":{"content":"llo"}}]}`)))
	emit(s.Chunk(chunkMap(t, `{"id":"c","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)))
	emit(s.Chunk(chunkMap(t, `{"id":"c","created":1,"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":9,"total_tokens":13}}`)))
	emit(s.Finish())
	out := body.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start", `"text_delta"`,
		`"text":"llo"`, "event: content_block_stop", "event: message_delta",
		`"stop_reason":"end_turn"`, `"output_tokens":9`, "event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "event: message_start"); n != 1 {
		t.Fatalf("message_start must appear once, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "event: message_stop"); n != 1 {
		t.Fatalf("message_stop must appear once, got %d:\n%s", n, out)
	}
}

func TestAnthropicSinkToolUse(t *testing.T) {
	s := NewAnthropicSink("claude-x", false)
	var body strings.Builder
	emit := func(lines []string) {
		for _, l := range lines {
			body.WriteString(l + "\n")
		}
	}
	emit(s.Chunk(chunkMap(t, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"bash","arguments":"{\"cmd\":"}}]}}]}`)))
	emit(s.Chunk(chunkMap(t, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`)))
	emit(s.Chunk(chunkMap(t, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)))
	emit(s.Finish())
	out := body.String()
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"name":"bash"`) {
		t.Fatalf("tool_use block missing:\n%s", out)
	}
	if !strings.Contains(out, `"input_json_delta"`) {
		t.Fatalf("arguments must stream as partial json:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop reason:\n%s", out)
	}
	// text block must not be opened when the model only calls a tool
	if strings.Contains(out, `"type":"text_delta"`) {
		t.Fatalf("unexpected text block:\n%s", out)
	}
}

func TestAnthropicSinkThinkingGated(t *testing.T) {
	reasonChunk := `{"choices":[{"index":0,"delta":{"reasoning_content":"pondering"}}]}`
	off := NewAnthropicSink("m", false)
	if strings.Contains(strings.Join(off.Chunk(chunkMap(t, reasonChunk)), ""), "thinking_delta") {
		t.Fatal("thinking must be off unless the client asked for it")
	}
	on := NewAnthropicSink("m", true)
	if !strings.Contains(strings.Join(on.Chunk(chunkMap(t, reasonChunk)), ""), "thinking_delta") {
		t.Fatal("thinking block expected when enabled")
	}
}

func TestChatToAnthropicMessage(t *testing.T) {
	chat := []byte(`{"id":"chatcmpl-9","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
	var v map[string]any
	if err := json.Unmarshal(ChatToAnthropicMessage(chat, "claude-x"), &v); err != nil {
		t.Fatal(err)
	}
	if v["type"] != "message" || v["stop_reason"] != "tool_use" {
		t.Fatalf("%v", v)
	}
	block := v["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_use" {
		t.Fatalf("%v", block)
	}
	input := block["input"].(map[string]any)
	if input["cmd"] != "ls" {
		t.Fatalf("arguments must be parsed into input: %v", input)
	}
	u := v["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 7 || u["output_tokens"].(float64) != 5 {
		t.Fatalf("usage=%v", u)
	}
}

func TestChatToAnthropicMessageBadUpstream(t *testing.T) {
	out := ChatToAnthropicMessage([]byte("not json"), "m")
	if !strings.Contains(string(out), `"type":"error"`) {
		t.Fatalf("garbage upstream must become an anthropic error: %s", out)
	}
}

func chunkMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}
