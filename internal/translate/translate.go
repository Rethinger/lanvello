// Package translate converts between the api dialects lanvello fronts:
// openai chat completions, the openai responses api (muse-spark free models)
// and anthropic messages.
package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
	// scalar generation params pass through 1:1 (responses names match).
	for _, k := range []string{"temperature", "top_p", "parallel_tool_calls", "reasoning", "reasoning_effort", "metadata"} {
		if v, ok := chat[k]; ok {
			out[k] = v
		}
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

// StreamConv carries responses->openai stream state (tool calls, ids, usage).
type StreamConv struct {
	Tools   map[string]*toolAcc
	Order   []string
	HasTool bool
	ID      string
	Created int64
	Reason  strings.Builder
	Usage   map[string]any
	Model   string
}

type toolAcc struct {
	ID   string
	Name string
	Args strings.Builder
}

// FeedEvent consumes one responses sse event and returns openai chat chunks.
// done=true on response.completed/failed/incomplete.
func (s *StreamConv) FeedEvent(ev map[string]any, model string) (chunks []map[string]any, done bool) {
	typ, _ := ev["type"].(string)
	switch typ {
	case "response.created", "response.in_progress":
		s.absorb(ev)
		return nil, false
	case "response.output_text.delta":
		d, _ := ev["delta"].(string)
		if d == "" {
			return nil, false
		}
		return []map[string]any{s.chunk(model, textDelta(d), nil, "")}, false
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// The free tier ships reasoning encrypted, but if a summary ever
		// arrives in plain text it must reach the client, not vanish.
		d, _ := ev["delta"].(string)
		if d == "" {
			return nil, false
		}
		s.Reason.WriteString(d)
		return []map[string]any{s.chunk(model, nil, textDelta(d), "")}, false
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
		return []map[string]any{s.toolChunk(model, len(s.Order)-1, callID, acc.Name, "")}, false
	case "response.function_call_arguments.delta":
		key := callIDOf(ev)
		acc := s.Tools[key]
		if acc == nil {
			return nil, false
		}
		d, _ := ev["delta"].(string)
		acc.Args.WriteString(d)
		idx := 0
		for i, id := range s.Order {
			if id == key {
				idx = i
			}
		}
		return []map[string]any{s.toolChunk(model, idx, "", "", d)}, false
	case "response.completed", "response.failed", "response.incomplete":
		s.absorb(ev)
		finish := "stop"
		if s.HasTool {
			finish = "tool_calls"
		}
		if st, _ := s.statusOf(ev); st == "incomplete" {
			finish = "length"
		}
		return []map[string]any{s.chunk(model, map[string]any{}, s.Usage, finish)}, true
	default:
		return nil, false
	}
}

// absorb keeps upstream id/created/model/usage so the translated chunks stop
// looking synthetic to clients.
func (s *StreamConv) absorb(ev map[string]any) {
	r, ok := ev["response"].(map[string]any)
	if !ok {
		return
	}
	if id, _ := r["id"].(string); id != "" {
		s.ID = "chatcmpl-" + strings.TrimPrefix(id, "resp_")
	}
	if c := int64Of(r["created_at"]); c > 0 {
		s.Created = c
	}
	if m, _ := r["model"].(string); m != "" {
		s.Model = m
	}
	if u, ok := r["usage"].(map[string]any); ok && len(u) > 0 {
		s.Usage = UsageFromResponses(u)
	}
}

func (s *StreamConv) statusOf(ev map[string]any) (string, bool) {
	r, ok := ev["response"].(map[string]any)
	if !ok {
		return "", false
	}
	st, _ := r["status"].(string)
	return st, true
}

func (s *StreamConv) header(model string) map[string]any {
	id := s.ID
	if id == "" {
		id = "chatcmpl-lanvello"
	}
	created := s.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	m := model
	if s.Model != "" {
		m = s.Model
	}
	return map[string]any{
		"id": id, "object": "chat.completion.chunk",
		"created": created, "model": m,
	}
}

func (s *StreamConv) chunk(model string, delta, usage map[string]any, finish string) map[string]any {
	m := s.header(model)
	choices := []any{map[string]any{
		"index": 0, "delta": delta, "finish_reason": nil,
	}}
	if finish != "" {
		choices[0].(map[string]any)["delta"] = map[string]any{}
		choices[0].(map[string]any)["finish_reason"] = finish
	}
	m["choices"] = choices
	if usage != nil {
		m["usage"] = usage
	}
	return m
}

func (s *StreamConv) toolChunk(model string, index int, id, name, args string) map[string]any {
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	if args != "" {
		fn["arguments"] = args
	}
	delta := map[string]any{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"index": index, "id": id, "type": "function",
			"function": fn,
		}},
	}
	if id == "" && name == "" {
		delta = map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "function": fn,
		}}}
	}
	return s.chunk(model, delta, nil, "")
}

func textDelta(s string) map[string]any {
	return map[string]any{"role": "assistant", "content": s}
}

// Feed is the line-oriented wrapper around FeedEvent.
func (s *StreamConv) Feed(ev map[string]any, model string) (lines []string, done bool) {
	chunks, done := s.FeedEvent(ev, model)
	for _, c := range chunks {
		lines = append(lines, sseLine(string(mustJSON(c))))
	}
	if done {
		lines = append(lines, "data: [DONE]")
	}
	return lines, done
}

// ResponsesSSEToOpenAI rewrites one upstream responses sse event into zero or
// more openai chat chunk lines.
func ResponsesSSEToOpenAI(ev map[string]any, model string) (lines []string, done bool) {
	c := &StreamConv{}
	return c.Feed(ev, model)
}

// UsageFromResponses converts a responses usage object into the openai chat
// usage shape (harnesses meter tokens from this).
func UsageFromResponses(u map[string]any) map[string]any {
	in := int64Of(u["input_tokens"])
	out := int64Of(u["output_tokens"])
	total := int64Of(u["total_tokens"])
	if total == 0 {
		total = in + out
	}
	return map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      total,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": int64Of(nested(u, "input_tokens_details", "cached_tokens")),
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": int64Of(nested(u, "output_tokens_details", "reasoning_tokens")),
		},
	}
}

// UsageFromChat normalizes an openai chat usage object (keeps reasoning
// tokens, which is how effort becomes visible to the client).
func UsageFromChat(u map[string]any) map[string]any {
	if u == nil || len(u) == 0 {
		return nil
	}
	out := map[string]any{
		"prompt_tokens":     int64Of(u["prompt_tokens"]),
		"completion_tokens": int64Of(u["completion_tokens"]),
		"total_tokens":      int64Of(u["total_tokens"]),
	}
	if d := nested(u, "completion_tokens_details", "reasoning_tokens"); d > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": d}
	}
	if d := nested(u, "prompt_tokens_details", "cached_tokens"); d > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": d}
	}
	return out
}

func nested(m map[string]any, keys ...string) int64 {
	cur := any(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = mm[k]
	}
	return int64Of(cur)
}

func int64Of(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
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

// AggregateResponsesConv collects deltas and tool calls into one completion.
func AggregateResponsesConv(sse []byte, model string) []byte {
	var sb strings.Builder
	conv := &StreamConv{}
	forEachPayload(sse, func(ev map[string]any) {
		if t, _ := ev["type"].(string); t == "response.output_text.delta" {
			if d, _ := ev["delta"].(string); d != "" {
				sb.WriteString(d)
			}
		}
		conv.FeedEvent(ev, model) // tracks tool calls, ids and usage
	})
	msg := map[string]any{"role": "assistant", "content": sb.String()}
	if conv.Reason.Len() > 0 {
		msg["reasoning_content"] = conv.Reason.String()
	}
	choice := map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}
	if conv.HasTool {
		choice["message"].(map[string]any)["tool_calls"] = conv.toolCalls()
		choice["finish_reason"] = "tool_calls"
	}
	resp := map[string]any{
		"id": firstNonEmpty(conv.ID, "chatcmpl-lanvello"), "object": "chat.completion",
		"created": firstNonZero(conv.Created, time.Now().Unix()), "model": model,
		"choices": []any{choice},
		"usage":   orEmptyUsage(conv.Usage),
	}
	return mustJSON(resp)
}

func (s *StreamConv) toolCalls() []any {
	var tc []any
	for i, id := range s.Order {
		acc := s.Tools[id]
		tc = append(tc, map[string]any{
			"index": i, "id": acc.ID, "type": "function",
			"function": map[string]any{"name": acc.Name, "arguments": acc.Args.String()},
		})
	}
	return tc
}

func sseLine(payload string) string { return "data: " + payload }

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

func orEmptyUsage(u map[string]any) map[string]any {
	if u == nil {
		return map[string]any{}
	}
	return u
}

// forEachPayload walks sse data payloads (skipping [DONE]).
func forEachPayload(sse []byte, fn func(ev map[string]any)) {
	sc := bufio.NewScanner(bytes.NewReader(sse))
	sc.Buffer(make([]byte, 256*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		fn(ev)
	}
}

// AggregateResponses collects all text deltas into one openai chat completion.
func AggregateResponses(sse []byte, model string) []byte {
	var sb strings.Builder
	forEachPayload(sse, func(ev map[string]any) {
		if t, _ := ev["type"].(string); t == "response.output_text.delta" {
			if d, _ := ev["delta"].(string); d != "" {
				sb.WriteString(d)
			}
		}
	})
	resp := map[string]any{
		"id": "chatcmpl-lanvello", "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": sb.String()},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{},
	}
	return mustJSON(resp)
}

// AggregateResponsesObject folds a native responses sse stream into the one
// response object a stream:false client expects: the terminal event carries
// the full response. ok=false when the stream never completed.
func AggregateResponsesObject(sse []byte) ([]byte, bool) {
	var last map[string]any
	forEachPayload(sse, func(ev map[string]any) {
		switch t, _ := ev["type"].(string); t {
		case "response.completed", "response.failed", "response.incomplete":
			if r, ok := ev["response"].(map[string]any); ok {
				last = r
			}
		}
	})
	if last == nil {
		return nil, false
	}
	return mustJSON(last), true
}

// AggregateOpenAI collects openai sse chunks into one chat completion.
func AggregateOpenAI(sse []byte, model string) []byte {
	var sb, reason strings.Builder
	calls := map[int]*toolAcc{}
	var order []int
	finish := "stop"
	var usage map[string]any
	id, created := "", int64(0)
	forEachPayload(sse, func(ch map[string]any) {
		if v, _ := ch["id"].(string); v != "" && id == "" {
			id = v
		}
		if c := int64Of(ch["created"]); c > 0 && created == 0 {
			created = c
		}
		if raw, ok := ch["usage"].(map[string]any); ok {
			if u := UsageFromChat(raw); u != nil {
				usage = u
			}
		}
		choices, _ := ch["choices"].([]any)
		if len(choices) == 0 {
			return
		}
		c0, _ := choices[0].(map[string]any)
		if fr, ok := c0["finish_reason"].(string); ok && fr != "" && fr != "null" {
			finish = fr
		}
		delta, _ := c0["delta"].(map[string]any)
		if delta == nil {
			return
		}
		if s, ok := delta["content"].(string); ok {
			sb.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			reason.WriteString(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range tcs {
				tc, _ := raw.(map[string]any)
				if tc == nil {
					continue
				}
				idx := int(int64Of(tc["index"]))
				acc := calls[idx]
				if acc == nil {
					acc = &toolAcc{}
					calls[idx] = acc
					order = append(order, idx)
				}
				if v, _ := tc["id"].(string); v != "" {
					acc.ID = v
				}
				if fn, ok := tc["function"].(map[string]any); ok {
					if n, _ := fn["name"].(string); n != "" {
						acc.Name = n
					}
					if a, _ := fn["arguments"].(string); a != "" {
						acc.Args.WriteString(a)
					}
				}
			}
		}
	})
	msg := map[string]any{"role": "assistant", "content": sb.String()}
	if reason.Len() > 0 {
		msg["reasoning_content"] = reason.String()
	}
	if len(order) > 0 {
		var tc []any
		for i, idx := range order {
			acc := calls[idx]
			tc = append(tc, map[string]any{
				"index": i, "id": acc.ID, "type": "function",
				"function": map[string]any{"name": acc.Name, "arguments": acc.Args.String()},
			})
		}
		msg["tool_calls"] = tc
		finish = "tool_calls"
	}
	resp := map[string]any{
		"id": firstNonEmpty(id, "chatcmpl-lanvello"), "object": "chat.completion",
		"created": firstNonZero(created, time.Now().Unix()), "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
		"usage": orEmptyUsage(usage),
	}
	return mustJSON(resp)
}

var _ = fmt.Sprint
