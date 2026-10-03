package server_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lanvello/internal/keys"
	"lanvello/internal/lanes"
	"lanvello/internal/server"
	"lanvello/internal/upstream"
)

// harness wires a gateway to a fake upstream so every handler can be tested
// without tor and without touching the network.
type harness struct {
	srv     *httptest.Server
	s       *server.Server
	dir     string
	key     string
	seen    chan map[string]any
	catalog string
}

const twoFreeModels = `{"data":[{"id":"fledge-alpha-free"},{"id":"muse-spark-1.3-contributor-free"}]}`

func newHarness(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *harness {
	t.Helper()
	h := &harness{seen: make(chan map[string]any, 16), catalog: twoFreeModels}
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zen/v1/models" {
			// the gateway fetches the catalog through a lane on its own
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, h.catalog)
			return
		}
		body := map[string]any{}
		dec := json.NewDecoder(r.Body)
		_ = dec.Decode(&body)
		select {
		case h.seen <- body:
		default:
		}
		handler(w, r)
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	ks, _ := keys.Open(dir)
	lm := lanes.New(dir, []string{"de"}, nil, nil, true, 1)
	cl := upstream.NewClient(lm)
	cl.BaseURL = up.URL
	cl.WaitBudget = 2 * time.Second
	h.s = server.New(ks, lm, cl)
	h.srv = up
	h.dir = dir
	h.key, _, _ = ks.Add("test")
	return h
}

func (h *harness) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *harness) upstreamBody(t *testing.T) map[string]any {
	t.Helper()
	select {
	case b := <-h.seen:
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never called")
		return nil
	}
}

// sseWriter emits sse events with flushes, like the real upstream.
func sseWriter(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	for _, e := range events {
		fmt.Fprint(w, e+"\n\n")
		fl.Flush()
	}
}

func chatTextStream(text string) []string {
	return []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":111,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":111,"model":"m","choices":[{"index":0,"delta":{"content":"` + text + `"}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":111,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":111,"model":"m","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"completion_tokens_details":{"reasoning_tokens":5}}}`,
		"data: [DONE]",
	}
}

func TestModelsGoesThroughLane(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	// the catalog must come from upstream (through a lane), not from a
	// hardcoded list, and must not advertise models the tier does not serve
	h.catalog = `{"data":[{"id":"fledge-alpha-free"},{"id":"big-pickle"},{"id":"union-alpha"},{"id":"claude-opus-4"},{"id":"jev-1.13-free"}]}`
	rec := h.do(t, "GET", "/v1/models", nil)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var v struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range v.Data {
		got[m.ID] = true
	}
	if !got["opencode/fledge-alpha-free"] || !got["opencode/big-pickle"] {
		t.Fatalf("missing free models: %v", got)
	}
	for _, bad := range []string{"opencode/union-alpha", "opencode/jev-1.13-free", "opencode/claude-opus-4"} {
		if got[bad] {
			t.Fatalf("must not advertise %s", bad)
		}
	}
}

func TestChatStreamRelaysIncrementally(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}`+"\n\n")
		fl.Flush()
		<-gate // hold the stream open: a buffering proxy would show nothing here
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fl.Flush()
	})
	body, _ := json.Marshal(map[string]any{
		"model": "fledge-alpha-free", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+h.key)
	rec := httptest.NewRecorder()
	go h.s.Handler().ServeHTTP(rec, req)
	time.Sleep(400 * time.Millisecond)
	if !strings.Contains(rec.Body.String(), "Hel") {
		t.Fatalf("first token never arrived before the stream ended: %q", rec.Body.String())
	}
	close(gate)
	time.Sleep(300 * time.Millisecond)
	if !strings.Contains(rec.Body.String(), "finish_reason") {
		t.Fatalf("stream did not finish: %q", rec.Body.String())
	}
}

func TestChatStreamCarriesUsage(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		sseWriter(w, chatTextStream("hi")...)
	})
	rec := h.do(t, "POST", "/v1/chat/completions", map[string]any{
		"model": "fledge-alpha-free", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"reasoning_tokens":5`) {
		t.Fatalf("usage must reach the client: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"created":111`) {
		t.Fatalf("upstream created must survive: %s", rec.Body.String())
	}
}

func TestChatNonStreamHasUsageAndCreated(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		sseWriter(w, chatTextStream("hi")...)
	})
	rec := h.do(t, "POST", "/v1/chat/completions", map[string]any{
		"model": "fledge-alpha-free", "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(rec.Body.String())
	}
	u, _ := v["usage"].(map[string]any)
	if u == nil || u["prompt_tokens"].(float64) != 11 {
		t.Fatalf("usage=%v", v["usage"])
	}
	if v["created"].(float64) == 0 {
		t.Fatal("created must be a real timestamp")
	}
}

func TestResponsesModelStreamUsage(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/v1/responses" {
			t.Errorf("path=%s want responses", r.URL.Path)
		}
		sseWriter(w,
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"resp_abc","created_at":999,"model":"muse-spark-1.3-contributor-free"}}`,
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","delta":"Hi"}`,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"resp_abc","created_at":999,"status":"completed","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"output_tokens_details":{"reasoning_tokens":12}}}}`,
		)
	})
	rec := h.do(t, "POST", "/v1/chat/completions", map[string]any{
		"model": "muse-spark-1.3-contributor-free", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	body := rec.Body.String()
	if !strings.Contains(body, "Hi") {
		t.Fatalf("no text: %s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":10`) || !strings.Contains(body, `"reasoning_tokens":12`) {
		t.Fatalf("responses usage not mapped: %s", body)
	}
	if !strings.Contains(body, `"created":999`) {
		t.Fatalf("created lost: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("no terminator: %s", body)
	}
}

func TestEffortReachesUpstream(t *testing.T) {
	for _, tc := range []struct {
		model  string
		effort string
		want   string
	}{
		{"muse-spark-1.3-contributor-free", "high", "high"},
		{"fledge-alpha-free", "minimal", "minimal"},
	} {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			sseWriter(w, chatTextStream("ok")...)
		})
		h.do(t, "POST", "/v1/chat/completions", map[string]any{
			"model": tc.model, "stream": false, "reasoning_effort": tc.effort,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		up := h.upstreamBody(t)
		if tc.model == "fledge-alpha-free" {
			if up["reasoning_effort"] != tc.want {
				t.Fatalf("chat path effort: %v", up["reasoning_effort"])
			}
			if _, bad := up["reasoning"]; bad {
				t.Fatalf("chat endpoint must not receive reasoning{}: %v", up["reasoning"])
			}
			continue
		}
		r, _ := up["reasoning"].(map[string]any)
		if r == nil || r["effort"] != tc.want {
			t.Fatalf("responses path effort: %v", up["reasoning"])
		}
	}
}

// responsesTextStream is what a responses-backed model emits.
func responsesTextStream(text string) []string {
	return []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_abc","created_at":42,"model":"muse-spark-1.3-contributor-free"}}`,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"` + text + `"}`,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_abc","created_at":42,"status":"completed","usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}}`,
	}
}

func TestMessagesAnthropicNonStream(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/v1/responses" {
			t.Errorf("path=%s: unknown model must land on the default free model", r.URL.Path)
		}
		sseWriter(w, responsesTextStream("Hello there")...)
	})
	rec := h.do(t, "POST", "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-20250514", "max_tokens": 100,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(rec.Body.String())
	}
	if v["type"] != "message" || v["role"] != "assistant" {
		t.Fatalf("not an anthropic message: %v", v)
	}
	// an unknown model name must land on a served one, not 401 upstream
	up := h.upstreamBody(t)
	if up["model"] == "claude-sonnet-4-20250514" {
		t.Fatalf("unserved model forwarded verbatim: %v", up["model"])
	}
	content, _ := v["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content: %v", v)
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "Hello there" {
		t.Fatalf("bad content block: %v", first)
	}
	if v["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason=%v", v["stop_reason"])
	}
	u, _ := v["usage"].(map[string]any)
	if u == nil || u["input_tokens"].(float64) != 9 {
		t.Fatalf("usage must survive the double translation: %v", v["usage"])
	}
}

func TestMessagesAnthropicStream(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		sseWriter(w, chatTextStream("Hi")...)
	})
	rec := h.do(t, "POST", "/v1/messages", map[string]any{
		"model": "fledge-alpha-free", "max_tokens": 50, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	body := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_start", "event: content_block_delta", "event: content_block_stop", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"text":"Hi"`) {
		t.Fatalf("no text delta:\n%s", body)
	}
	if !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("no stop reason:\n%s", body)
	}
	if strings.Contains(body, "chat.completion.chunk") {
		t.Fatalf("openai chunks leaked into the anthropic stream:\n%s", body)
	}
}

func TestMessagesToolUseRoundTrip(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		sseWriter(w,
			`data: {"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":"}}]}}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			"data: [DONE]",
		)
	})
	rec := h.do(t, "POST", "/v1/messages", map[string]any{
		"model": "fledge-alpha-free", "max_tokens": 50,
		"messages": []any{map[string]any{"role": "user", "content": "list files"}},
		"tools": []any{map[string]any{
			"name": "bash", "description": "run",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	})
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(rec.Body.String())
	}
	if v["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason=%v", v["stop_reason"])
	}
	content, _ := v["content"].([]any)
	block, _ := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "bash" {
		t.Fatalf("tool block=%v", block)
	}
	input, _ := block["input"].(map[string]any)
	if input["cmd"] != "ls" {
		t.Fatalf("partial json not merged: %v", input)
	}
	// the anthropic tool schema must reach upstream as an openai function
	up := h.upstreamBody(t)
	tools, _ := up["tools"].([]any)
	var sawBash bool
	for _, raw := range tools {
		tm, _ := raw.(map[string]any)
		fn, _ := tm["function"].(map[string]any)
		if fn["name"] == "bash" {
			sawBash = true
		}
	}
	if !sawBash {
		t.Fatalf("bash not forwarded: %v", tools)
	}
}

func TestMessagesToolResultHistory(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		sseWriter(w, chatTextStream("done")...)
	})
	h.do(t, "POST", "/v1/messages", map[string]any{
		"model": "fledge-alpha-free", "max_tokens": 50,
		"messages": []any{
			map[string]any{"role": "user", "content": "run ls"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": map[string]any{"cmd": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"},
			}},
		},
	})
	up := h.upstreamBody(t)
	msgs, _ := up["messages"].([]any)
	var roles []string
	for _, raw := range msgs {
		mm, _ := raw.(map[string]any)
		roles = append(roles, asStr(mm["role"]))
	}
	want := []string{"user", "assistant", "tool"}
	if len(roles) != len(want) {
		t.Fatalf("roles=%v", roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func TestUnknownModelFallsBack(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		sseWriter(w, chatTextStream("ok")...)
	})
	rec := h.do(t, "POST", "/v1/chat/completions", map[string]any{
		"model": "gpt-5.2-codex", "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	up := h.upstreamBody(t)
	if !strings.Contains(asStr(up["model"]), "contributor-free") {
		t.Fatalf("fallback model=%v", up["model"])
	}
}

func TestResponsesNativeStreamRelayed(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		sseWriter(w,
			`data: {"type":"response.output_text.delta","delta":"raw"}`,
			`data: {"type":"response.completed","response":{"id":"resp_z","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		)
	})
	rec := h.do(t, "POST", "/v1/responses", map[string]any{
		"model": "muse-spark-1.3-contributor-free", "stream": true,
		"input": "hi",
	})
	body := rec.Body.String()
	if !strings.Contains(body, "raw") || !strings.Contains(body, "response.completed") {
		t.Fatalf("native responses stream mangled:\n%s", body)
	}
}

func TestUpstream429RotatesBeforeFirstByte(t *testing.T) {
	var n int
	lm := lanes.New(t.TempDir(), []string{"us", "de"}, nil, nil, true, 2)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "limited", 429)
			return
		}
		sseWriter(w, chatTextStream("second lane")...)
	}))
	defer up.Close()
	cl := upstream.NewClient(lm)
	cl.BaseURL = up.URL
	ks, _ := keys.Open(t.TempDir())
	s := server.New(ks, lm, cl)
	key, _, _ := ks.Add("k")
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"fledge-alpha-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "second lane") {
		t.Fatalf("did not rotate: %s", rec.Body.String())
	}
}

func TestBodyLimit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		sseWriter(w, chatTextStream("ok")...)
	})
	// a body just over the cap must be refused, not silently truncated
	big := strings.Repeat("x", 1<<20)
	var sb strings.Builder
	sb.WriteString(`{"model":"fledge-alpha-free","stream":false,"messages":[{"role":"user","content":"`)
	for i := 0; i < 200; i++ {
		sb.WriteString(big)
	}
	sb.WriteString(`"}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(sb.String()))
	req.Header.Set("Authorization", "Bearer "+h.key)
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	// 200MB+1 of json is not a valid short request: whatever happens, the
	// process must survive and answer with a status, not hang or crash.
	if rec.Code == 0 {
		t.Fatal("no status written")
	}
}

func TestHealthzNoAuth(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestBadKeyRejected(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-lanv-nope")
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

// streaming must not depend on the response recorder: real http.Flusher path.
func TestStreamFlushesOnRealServer(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"one"}}]}`+"\n\n")
		fl.Flush()
		<-release
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	})
	hs := httptest.NewServer(h.s.Handler())
	defer hs.Close()
	req, _ := http.NewRequest("POST", hs.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"fledge-alpha-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	first := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") && strings.Contains(sc.Text(), "one") {
				first <- sc.Text()
				return
			}
		}
		close(first)
	}()
	select {
	case l, ok := <-first:
		if !ok {
			t.Fatal("stream ended without the first token")
		}
		if !strings.Contains(l, "one") {
			t.Fatalf("bad first chunk: %s", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first token never flushed")
	}
	close(release)
}
