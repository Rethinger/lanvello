package translate

import (
	"encoding/json"
	"strings"
	"time"
)

// MessagesToChat maps an anthropic /v1/messages body onto an openai chat body.
// Model names the free tier does not serve are replaced by fallback so
// claude-code style clients (which insist on their own model ids) still work.
func MessagesToChat(body map[string]any, known func(string) bool, fallback string) map[string]any {
	model, _ := body["model"].(string)
	model = shortID(model)
	if model == "" || (known != nil && !known(model)) {
		model = fallback
	}
	out := map[string]any{"model": model}
	if v, ok := body["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := body["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := body["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := body["metadata"]; ok {
		out["metadata"] = v
	}
	if v, ok := body["stop_sequences"].([]any); ok && len(v) > 0 {
		out["stop"] = v
	}
	if v, ok := body["stream"]; ok {
		out["stream"] = v
	}
	// effort: anthropic spells it thinking.budget_tokens / reasoning_effort
	// depending on the client, both collapse onto reasoning_effort.
	if v, ok := body["reasoning_effort"]; ok {
		out["reasoning_effort"] = v
	} else if th, ok := body["thinking"].(map[string]any); ok {
		if bt, ok := th["budget_tokens"]; ok {
			out["reasoning_effort"] = effortFromBudget(int64Of(bt))
		}
	}

	var msgs []any
	if sys := systemText(body["system"]); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	if arr, ok := body["messages"].([]any); ok {
		for _, raw := range arr {
			mm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			if role != "assistant" {
				role = "user"
			}
			switch content := mm["content"].(type) {
			case string:
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			case []any:
				var text strings.Builder
				var toolCalls []any
				for _, braw := range content {
					bm, ok := braw.(map[string]any)
					if !ok {
						continue
					}
					switch btype, _ := bm["type"].(string); btype {
					case "text":
						text.WriteString(blockText(bm))
					case "thinking":
						// dropped on the way up: reasoning is regenerated
					case "tool_use":
						args := bm["input"]
						s, _ := args.(string)
						if s == "" {
							s = jsonString(args)
						}
						id, _ := bm["id"].(string)
						name, _ := bm["name"].(string)
						toolCalls = append(toolCalls, map[string]any{
							"index": len(toolCalls), "id": id, "type": "function",
							"function": map[string]any{"name": name, "arguments": s},
						})
					case "tool_result":
						// tool results are their own openai tool message
						id, _ := bm["tool_use_id"].(string)
						if id == "" {
							id, _ = bm["id"].(string)
						}
						out2 := blockText(bm)
						if c, ok := bm["content"]; ok {
							if s, ok := c.(string); ok {
								out2 = s
							} else if s := blocksText(c); s != "" {
								out2 = s
							}
						}
						msgs = append(msgs, map[string]any{
							"role": "tool", "tool_call_id": id, "content": out2,
						})
					case "image":
						// free tier is text-only, image blocks are dropped
					}
				}
				if toolCalls != nil {
					m := map[string]any{"role": "assistant", "content": text.String()}
					m["tool_calls"] = toolCalls
					msgs = append(msgs, m)
				} else if text.Len() > 0 || len(content) == 0 {
					msgs = append(msgs, map[string]any{"role": role, "content": text.String()})
				}
			}
		}
	}
	if msgs == nil {
		msgs = append(msgs, map[string]any{"role": "user", "content": "..."})
	}
	out["messages"] = msgs

	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		var conv []any
		for _, raw := range tools {
			tm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tm["name"].(string)
			if name == "" {
				continue
			}
			desc, _ := tm["description"].(string)
			params := tm["input_schema"]
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			conv = append(conv, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": name, "description": desc, "parameters": params,
				},
			})
		}
		if len(conv) > 0 {
			out["tools"] = conv
		}
	}
	if tc, ok := body["tool_choice"].(map[string]any); ok {
		switch t, _ := tc["type"].(string); t {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			if n, _ := tc["name"].(string); n != "" {
				out["tool_choice"] = map[string]any{
					"type": "function", "function": map[string]any{"name": n},
				}
			}
		}
	}
	return out
}

func effortFromBudget(tokens int64) string {
	switch {
	case tokens <= 0:
		return "minimal"
	case tokens < 2000:
		return "low"
	case tokens < 8000:
		return "medium"
	default:
		return "high"
	}
}

func shortID(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

func systemText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		return blocksText(s)
	}
	return ""
}

func blocksText(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, raw := range arr {
		bm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := bm["type"].(string); t != "" && t != "text" {
			continue
		}
		sb.WriteString(blockText(bm))
	}
	return sb.String()
}

func blockText(bm map[string]any) string {
	if s, ok := bm["text"].(string); ok {
		return s
	}
	if c, ok := bm["content"]; ok {
		if s, ok := c.(string); ok {
			return s
		}
		return blocksText(c)
	}
	return ""
}

func jsonString(v any) string {
	if v == nil {
		return "{}"
	}
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	return string(mustJSON(v))
}

// ChunkSink turns openai chat chunks into downstream sse lines. The server
// reuses one sink implementation for openai clients and another for anthropic.
type ChunkSink interface {
	Chunk(map[string]any) []string
	Finish() []string
}

// OpenAISink writes openai chat chunks straight back as chat chunks.
type OpenAISink struct{}

func (OpenAISink) Chunk(c map[string]any) []string {
	return []string{sseLine(string(mustJSON(c)))}
}

func (OpenAISink) Finish() []string { return []string{"data: [DONE]"} }

// AnthropicSink converts openai chat chunks into the anthropic messages
// event sequence (message_start, content_block_*, message_delta, message_stop).
type AnthropicSink struct {
	Model    string
	Thinking bool

	msgID   string
	started bool
	next    int
	text    int // index of the open text block, -1 when none
	think   int // index of the open thinking block, -1 when none
	tools   map[int]int
	open    map[int]bool
	stop    string
	usage   map[string]any
}

func NewAnthropicSink(model string, thinking bool) *AnthropicSink {
	return &AnthropicSink{
		Model: model, Thinking: thinking,
		text: -1, think: -1,
		tools: map[int]int{}, open: map[int]bool{},
		msgID: "msg_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000"), ".", ""),
	}
}

func (a *AnthropicSink) start(usage map[string]any) []string {
	if a.started {
		return nil
	}
	a.started = true
	msg := map[string]any{
		"id": a.msgID, "type": "message", "role": "assistant",
		"model": a.Model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": inTok(usage), "output_tokens": 0},
	}
	return []string{event("message_start", map[string]any{"message": msg})}
}

func (a *AnthropicSink) Chunk(c map[string]any) []string {
	var out []string
	if raw, ok := c["usage"].(map[string]any); ok {
		if u := UsageFromChat(raw); u != nil {
			a.usage = u
		}
	}
	out = append(out, a.start(a.usage)...)
	choices, _ := c["choices"].([]any)
	if len(choices) == 0 {
		return lines(out)
	}
	c0, _ := choices[0].(map[string]any)
	if fr, ok := c0["finish_reason"].(string); ok && fr != "" && fr != "null" {
		a.stop = anthropicStop(fr)
	}
	delta, _ := c0["delta"].(map[string]any)
	if delta == nil {
		return lines(out)
	}
	if s, ok := delta["reasoning_content"].(string); ok && s != "" && a.Thinking {
		out = append(out, a.openThink()...)
		out = append(out, event("content_block_delta", map[string]any{
			"index": a.think,
			"delta": map[string]any{"type": "thinking_delta", "thinking": s},
		}))
	}
	if s, ok := delta["content"].(string); ok && s != "" {
		out = append(out, a.openText()...)
		out = append(out, event("content_block_delta", map[string]any{
			"index": a.text,
			"delta": map[string]any{"type": "text_delta", "text": s},
		}))
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range tcs {
			tc, _ := raw.(map[string]any)
			if tc == nil {
				continue
			}
			idx := int(int64Of(tc["index"]))
			bi, ok := a.tools[idx]
			if !ok {
				fn, _ := tc["function"].(map[string]any)
				name, _ := fn["name"].(string)
				id, _ := tc["id"].(string)
				if id == "" {
					id = "call_" + strings.TrimPrefix(a.msgID, "msg_")
				}
				bi = a.next
				a.next++
				a.tools[idx] = bi
				a.open[bi] = true
				out = append(out, event("content_block_start", map[string]any{
					"index": bi,
					"content_block": map[string]any{
						"type": "tool_use", "id": id, "name": name, "input": map[string]any{},
					},
				}))
			}
			fn, _ := tc["function"].(map[string]any)
			if args, ok := fn["arguments"].(string); ok && args != "" {
				out = append(out, event("content_block_delta", map[string]any{
					"index": bi,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
				}))
			}
		}
	}
	return lines(out)
}

func (a *AnthropicSink) openText() []string {
	if a.text >= 0 {
		return nil
	}
	a.text = a.next
	a.next++
	a.open[a.text] = true
	return []string{event("content_block_start", map[string]any{
		"index": a.text, "content_block": map[string]any{"type": "text", "text": ""},
	})}
}

func (a *AnthropicSink) openThink() []string {
	if a.think >= 0 {
		return nil
	}
	a.think = a.next
	a.next++
	a.open[a.think] = true
	return []string{event("content_block_start", map[string]any{
		"index": a.think, "content_block": map[string]any{"type": "thinking", "thinking": ""},
	})}
}

func (a *AnthropicSink) Finish() []string {
	var out []string
	out = append(out, a.start(a.usage)...)
	for i := 0; i < a.next; i++ {
		if a.open[i] {
			out = append(out, event("content_block_stop", map[string]any{"index": i}))
			a.open[i] = false
		}
	}
	stop := a.stop
	if stop == "" {
		if len(a.tools) > 0 {
			stop = "tool_use"
		} else {
			stop = "end_turn"
		}
	}
	// input_tokens is only known when upstream sends its usage chunk, which
	// arrives after the first deltas; clients read it here as well.
	out = append(out, event("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":  inTok(a.usage),
			"output_tokens": outTok(a.usage),
		},
	}))
	out = append(out, event("message_stop", nil))
	return lines(out)
}

func anthropicStop(finish string) string {
	switch finish {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	}
	return "end_turn"
}

func inTok(u map[string]any) int64 {
	if u == nil {
		return 0
	}
	return int64Of(u["prompt_tokens"])
}

func outTok(u map[string]any) int64 {
	if u == nil {
		return 0
	}
	return int64Of(u["completion_tokens"])
}

func lines(out []string) []string {
	if len(out) == 0 {
		return nil
	}
	return out
}

func event(name string, data map[string]any) string {
	body := map[string]any{"type": name}
	for k, v := range data {
		body[k] = v
	}
	return "event: " + name + "\ndata: " + string(mustJSON(body))
}

// ChatToAnthropicMessage converts one aggregated openai chat completion into an
// anthropic message object.
func ChatToAnthropicMessage(chat []byte, model string) []byte {
	var v map[string]any
	if err := json.Unmarshal(chat, &v); err != nil {
		return mustJSON(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type": "api_error", "message": "upstream returned an unreadable answer",
			},
		})
	}
	content := []any{}
	var finish string
	if choices, ok := v["choices"].([]any); ok && len(choices) > 0 {
		c0, _ := choices[0].(map[string]any)
		msg, _ := c0["message"].(map[string]any)
		finish, _ = c0["finish_reason"].(string)
		if s, _ := msg["content"].(string); s != "" {
			content = append(content, map[string]any{"type": "text", "text": s})
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for _, raw := range tcs {
				tc, _ := raw.(map[string]any)
				if tc == nil {
					continue
				}
				fn, _ := tc["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				if args == "" {
					args = "{}"
				}
				var input any
				if err := json.Unmarshal([]byte(args), &input); err != nil {
					input = map[string]any{}
				}
				id, _ := tc["id"].(string)
				content = append(content, map[string]any{
					"type": "tool_use", "id": id, "name": name, "input": input,
				})
			}
		}
	}
	usage, _ := v["usage"].(map[string]any)
	usage = UsageFromChat(usage)
	msg := map[string]any{
		"id":   "msg_" + strings.TrimPrefix(firstNonEmpty(asString(v["id"]), "lanvello"), "chatcmpl-"),
		"type": "message", "role": "assistant",
		"model": model, "content": content,
		"stop_reason": anthropicStop(finish), "stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inTok(usage),
			"output_tokens": outTok(usage),
		},
	}
	return mustJSON(msg)
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
