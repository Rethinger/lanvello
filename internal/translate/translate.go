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
				callID, _ := mm["tool_call_id"].(string)
				if callID == "" {
					callID, _ = mm["id"].(string)
				}
				input = append(input, map[string]any{
					"type": "function_call_output", "call_id": callID,
					"output": textOf(mm["content"]),
				})
				continue
			}
			if role == "" {
				role = "user"
			}
			if role == "assistant" {
				if tc, ok := mm["tool_calls"].([]any); ok && len(tc) > 0 {
					for _, c := range tc {
						cm, ok := c.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := cm["function"].(map[string]any)
						id, _ := cm["id"].(string)
						name, _ := fn["name"].(string)
						args, _ := fn["arguments"].(string)
						input = append(input, map[string]any{
							"type": "function_call", "call_id": id,
							"name": name, "arguments": args,
						})
					}
					continue
				}
			}
			input = append(input, map[string]any{
				"type": "message", "role": role,
				"content": textParts(role, mm["content"]),
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

func textOf(c any) string {
	switch v := c.(type) {
	case string:
		return v
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func textParts(role string, c any) []any {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	switch v := c.(type) {
	case string:
		return []any{map[string]any{"type": kind, "text": v}}
	case []any:
		var parts []any
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := pm["type"].(string)
			switch typ {
			case "text", "input_text", "output_text":
				if t, ok := pm["text"].(string); ok {
					parts = append(parts, map[string]any{"type": kind, "text": t})
				}
			case "image_url":
				// v1: drop images
			}
		}
		return parts
	}
	return []any{map[string]any{"type": kind, "text": ""}}
}

// StreamConv carries responses->openai stream state (tool calls).
type StreamConv struct {
	Tools   map[string]*toolAcc
	Order   []string
	HasTool bool
}

type toolAcc struct {
	ID   string
	Name string
	Args strings.Builder
}

// Feed consumes one responses sse event and returns openai chunk lines.
func (s *StreamConv) Feed(ev map[string]any, model string) (lines []string, done bool) {
	typ, _ := ev["type"].(string)
	switch typ {
	case "response.output_text.delta":
		d, _ := ev["delta"].(string)
		if d == "" {
			return nil, false
		}
		return []string{sseLine(chatChunk(model, d, ""))}, false
	case "response.output_item.added":
		item, _ := ev["item"].(map[string]any)
		if item == nil || item["type"] != "function_call" {
			return nil, false
		}
		if s.Tools == nil {
			s.Tools = map[string]*toolAcc{}
		}
		callID, _ := item["call_id"].(string)
		itemID, _ := item["id"].(string)
		acc := &toolAcc{ID: callID, Name: nameOf(item)}
		// Deltas are keyed by item id (fc_*), not call id.
		key := itemID
		if key == "" {
			key = callID
		}
		s.Tools[key] = acc
		s.Order = append(s.Order, key)
		s.HasTool = true
		return []string{sseLine(toolChunk(model, len(s.Order)-1, callID, acc.Name, ""))}, false
	case "response.function_call_arguments.delta":
		callID := callIDOf(ev)
		acc := s.Tools[callID]
		if acc == nil {
			return nil, false
		}
		d, _ := ev["delta"].(string)
		acc.Args.WriteString(d)
		idx := 0
		for i, id := range s.Order {
			if id == callID {
				idx = i
			}
		}
		return []string{sseLine(toolChunk(model, idx, "", "", d))}, false
	case "response.completed", "response.failed", "response.incomplete":
		finish := "stop"
		if s.HasTool {
			finish = "tool_calls"
		}
		return []string{sseLine(chatChunk(model, "", finish)), "data: [DONE]"}, true
	case "response.created", "response.in_progress", "response.output_item.done",
		"response.content_part.added", "response.content_part.done",
		"response.output_text.annotation.added", "response.function_call_arguments.done":
		return nil, false
	default:
		return nil, false
	}
}

func nameOf(item map[string]any) string {
	if n, ok := item["name"].(string); ok {
		return n
	}
	return ""
}

func callIDOf(ev map[string]any) string {
	if id, ok := ev["item_id"].(string); ok && id != "" {
		return id
	}
	if id, ok := ev["call_id"].(string); ok {
		return id
	}
	return ""
}

func toolChunk(model string, index int, id, name, args string) string {
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	if args != "" {
		fn["arguments"] = args
	}
	m := map[string]any{
		"id": "chatcmpl-lanvello", "object": "chat.completion.chunk",
		"created": 0, "model": model,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"index": index, "id": id, "type": "function",
					"function": fn,
				}},
			},
			"finish_reason": nil,
		}},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// ResponsesSSEToOpenAI rewrites one upstream responses sse event into zero or
// more openai chat chunk lines (without the data: prefix handling by caller).
// Returns done=true on response.completed/failed.
func ResponsesSSEToOpenAI(ev map[string]any, model string) (lines []string, done bool) {
	c := &StreamConv{}
	return c.Feed(ev, model)
}

// AggregateResponsesConv collects deltas and tool calls into one completion.
func AggregateResponsesConv(sse []byte, model string) []byte {
	var sb strings.Builder
	conv := &StreamConv{}
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
		conv.Feed(ev, model)
	}
	choice := map[string]any{
		"index":         0,
		"message":       map[string]any{"role": "assistant", "content": sb.String()},
		"finish_reason": "stop",
	}
	if conv.HasTool {
		var tc []any
		for i, id := range conv.Order {
			acc := conv.Tools[id]
			tc = append(tc, map[string]any{
				"index": i, "id": acc.ID, "type": "function",
				"function": map[string]any{"name": acc.Name, "arguments": acc.Args.String()},
			})
		}
		choice["message"].(map[string]any)["tool_calls"] = tc
		choice["finish_reason"] = "tool_calls"
	}
	resp := map[string]any{
		"id": "chatcmpl-lanvello", "object": "chat.completion",
		"created": 0, "model": model,
		"choices": []any{choice},
		"usage":   map[string]any{},
	}
	b, _ := json.Marshal(resp)
	return b
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

// AggregateOpenAI collects openai sse chunks into one chat completion.
func AggregateOpenAI(sse []byte, model string) []byte {
	var sb strings.Builder
	var tools []any
	finish := "stop"
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
		var ch map[string]any
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			continue
		}
		choices, _ := ch["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0, _ := choices[0].(map[string]any)
		if fr, ok := c0["finish_reason"].(string); ok && fr != "" && fr != "null" {
			finish = fr
		}
		delta, _ := c0["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if s, ok := delta["content"].(string); ok {
			sb.WriteString(s)
		}
		if tc, ok := delta["tool_calls"].([]any); ok {
			tools = append(tools, tc...)
		}
	}
	msg := map[string]any{"role": "assistant", "content": sb.String()}
	if len(tools) > 0 {
		msg["tool_calls"] = tools
		finish = "tool_calls"
	}
	resp := map[string]any{
		"id": "chatcmpl-lanvello", "object": "chat.completion",
		"created": 0, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
		"usage": map[string]any{},
	}
	b, _ := json.Marshal(resp)
	return b
}

var _ = fmt.Sprint
