// Package translate converts between openai chat completions and the
// responses api used by muse-spark free models.
package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ChatToResponses maps an openai chat body onto a responses body.
func ChatToResponses(chat map[string]any) map[string]any {
	out := map[string]any{"stream": true, "store": false}
	if m, ok := chat["model"]; ok {
		out["model"] = m
	}
	var input []any
	if msgs, ok := chat["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				continue // v1: tool outputs unsupported, skip
			}
			if role == "" {
				role = "user"
			}
			input = append(input, map[string]any{
				"type": "message", "role": role,
				"content": textParts(mm["content"]),
			})
		}
	}
	out["input"] = input
	if v, ok := chat["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if v, ok := chat["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if tools, ok := chat["tools"].([]any); ok {
		var flat []any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tm["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if name == "" {
				name, _ = tm["name"].(string)
			}
			if name == "" {
				continue
			}
			desc, _ := fn["description"].(string)
			params := fn["parameters"]
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			flat = append(flat, map[string]any{
				"type": "function", "name": name,
				"description": desc, "parameters": params,
			})
		}
		out["tools"] = flat
	}
	if tc, ok := chat["tool_choice"]; ok {
		out["tool_choice"] = tc
	}
	return out
}

func textParts(c any) []any {
	switch v := c.(type) {
	case string:
		return []any{map[string]any{"type": "input_text", "text": v}}
	case []any:
		var parts []any
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := pm["type"].(string)
			switch typ {
			case "text", "input_text":
				if t, ok := pm["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "input_text", "text": t})
				}
			case "image_url":
				// v1: drop images
			}
		}
		return parts
	}
	return []any{map[string]any{"type": "input_text", "text": ""}}
}

// ResponsesSSEToOpenAI rewrites one upstream responses sse event into zero or
// more openai chat chunk lines (without the data: prefix handling by caller).
// Returns done=true on response.completed/failed.
func ResponsesSSEToOpenAI(ev map[string]any, model string) (lines []string, done bool) {
	typ, _ := ev["type"].(string)
	switch typ {
	case "response.output_text.delta":
		d, _ := ev["delta"].(string)
		if d == "" {
			return nil, false
		}
		return []string{sseLine(chatChunk(model, d, ""))}, false
	case "response.completed":
		return []string{sseLine(chatChunk(model, "", "stop")), "data: [DONE]"}, true
	case "response.failed", "response.incomplete":
		return []string{sseLine(chatChunk(model, "", "stop")), "data: [DONE]"}, true
	case "response.created", "response.in_progress", "response.output_item.added",
		"response.output_item.done", "response.content_part.added",
		"response.content_part.done", "response.output_text.annotation.added":
		return nil, false
	default:
		// tool calls etc: v1 passes nothing through
		return nil, false
	}
}

func chatChunk(model, content, finish string) string {
	m := map[string]any{
		"id": "chatcmpl-lanvello", "object": "chat.completion.chunk",
		"created": 0, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": content},
			"finish_reason": nil,
		}},
	}
	if finish != "" {
		m["choices"].([]any)[0].(map[string]any)["finish_reason"] = finish
		m["choices"].([]any)[0].(map[string]any)["delta"] = map[string]any{}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func sseLine(payload string) string { return "data: " + payload }

// AggregateResponses collects all deltas into one openai chat completion.
func AggregateResponses(sse []byte, model string) []byte {
	var sb strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(sse))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if t, _ := ev["type"].(string); t == "response.output_text.delta" {
			if d, _ := ev["delta"].(string); d != "" {
				sb.WriteString(d)
			}
		}
	}
	resp := map[string]any{
		"id": "chatcmpl-lanvello", "object": "chat.completion",
		"created": 0, "model": model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": sb.String()},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{},
	}
	b, _ := json.Marshal(resp)
	return b
}

var _ = fmt.Sprint
