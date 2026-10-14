// Package server exposes the universal openai-compatible api backed only by
// the opencode free tier (no login, fingerprinted as the official client).
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"lanvello/internal/bridge"
	"lanvello/internal/caps"
	"lanvello/internal/clientapi"
	"lanvello/internal/keys"
	"lanvello/internal/lanes"
	"lanvello/internal/translate"
	"lanvello/internal/upstream"
)

// maxBody caps a downstream request body: enough for a 1M-token context,
// bounded so a runaway client cannot exhaust memory.
const maxBody = 128 << 20

type Server struct {
	Keys *keys.Store
	Lane *lanes.Manager
	Up   *upstream.Client
	Caps *caps.Store
	// ClientAPI, when set, is where inference goes: the running opencode
	// client makes the call (it holds the free-tier session), we translate.
	// The replay path (Up) cannot serve the free tier at all any more, so
	// this is not an optimisation, it is the only way in.
	ClientAPI *clientapi.Client
	// BridgeDir is where a caller's request is parked for the client-side
	// plugin (system, history, tools). Empty disables the bridge: the client
	// then answers with its own agent and its own tools.
	BridgeDir string
	// pollGap is how often the client's session is read while answering.
	pollGap time.Duration
	// RequireKey refuses keyless /v1 requests even before the first key is
	// issued (config `requireKey` / LANVELLO_REQUIRE_KEY): a public
	// deployment must never fall back to open mode.
	RequireKey bool

	catMu    sync.Mutex
	catCache []modelEnt
	catAt    time.Time
}

// modelEnt is one catalog row: the advertised id plus whatever
// upstream metadata we keep (the opencode list stamps a created
// time per model).
type modelEnt struct {
	ID      string
	Created int64
}

func New(ks *keys.Store, lm *lanes.Manager, up *upstream.Client, dataDir string) *Server {
	s := &Server{Keys: ks, Lane: lm, Up: up, Caps: caps.Open(dataDir, up, lm), pollGap: 250 * time.Millisecond}
	// warm the capability table in the background; never blocks
	// startup and never dials directly.
	go s.Caps.MaybeRefresh()
	return s
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", s.health)
	m.HandleFunc("/v1/models", s.models)
	m.HandleFunc("/v1/chat/completions", s.chat)
	m.HandleFunc("/v1/responses", s.responses)
	m.HandleFunc("/v1/messages", s.messages)
	return m
}

func (s *Server) identity(r *http.Request) string {
	// Chain a downstream opencode session when it is already valid so
	// quota accounting stays on one conversation.
	if ses := strings.TrimSpace(r.Header.Get("x-opencode-session")); upstream.ValidSession(ses) {
		return ses
	}
	sec := keys.BearerOf(r.Header.Get("Authorization"))
	if sec == "" {
		sec = r.URL.Query().Get("api_key")
	}
	if sec == "" {
		return "anon"
	}
	return sec
}

func (s *Server) authed(r *http.Request) bool {
	if len(s.Keys.List()) == 0 {
		return !s.RequireKey // open mode: no keys issued yet
	}
	sec := keys.BearerOf(r.Header.Get("Authorization"))
	if sec == "" {
		sec = r.URL.Query().Get("api_key")
	}
	return s.Keys.Verify(sec, false)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"ok":true,"name":"lanvello"}`+"\n")
}

// defaultModels is the fallback catalog for when upstream cannot be reached.
// union-alpha is intentionally absent: the free tier does not serve it.
var defaultModels = []string{
	"opencode/muse-spark-1.3-contributor-free",
	"opencode/muse-spark-1.2-contributor-free",
	"opencode/longcat-2.5-preview-free",
	"opencode/space-bunny-free",
	"opencode/fledge-alpha-free",
	"opencode/mimo-v2.6-flash-free",
	"opencode/mimo-v2.5-free",
	"opencode/ling-3.1-flash-free",
	"opencode/ling-3.0-flash-fin-free",
	"opencode/nemotron-3-ultra-free",
	"opencode/nemotron-3.5-lightning-free",
	"opencode/big-pickle",
}

// defaultModel is what an unroutable request lands on.
const defaultModel = "opencode/muse-spark-1.3-contributor-free"

// catalog asks upstream through a lane (never directly) and caches
// briefly so a chatty client does not spend a lane per /v1/models
// call. Capabilities ride along from opencode's catalog mirror.
func (s *Server) catalog() []string {
	ents := s.catalogEntries()
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.ID)
	}
	return out
}

func (s *Server) catalogEntries() []modelEnt {
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if time.Since(s.catAt) < time.Minute && len(s.catCache) > 0 {
		return s.catCache
	}
	out := s.fetchCatalog()
	if len(out) == 0 {
		out = defaultEntries()
	}
	s.catCache, s.catAt = out, time.Now()
	return out
}

func defaultEntries() []modelEnt {
	out := make([]modelEnt, 0, len(defaultModels))
	for _, id := range defaultModels {
		out = append(out, modelEnt{ID: id})
	}
	return out
}

func (s *Server) fetchCatalog() []modelEnt {
	// Client-backed: the client is the authority on what it can drive, and its
	// list needs no lane (the catalog mirror is not gated, but the client has
	// already fetched it).
	if s.ClientAPI != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if mods, err := s.ClientAPI.Models(ctx); err == nil {
			var out []modelEnt
			for _, m := range mods {
				if m.ProviderID != "opencode" || m.ID == "" {
					continue
				}
				out = append(out, modelEnt{ID: "opencode/" + m.ID})
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	status, body := s.Up.GetJSON("/zen/v1/models")
	if status != 200 || len(body) == 0 {
		return nil
	}
	var v struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil
	}
	var out []modelEnt
	for _, m := range v.Data {
		id := m.ID
		if id == "jev-1.13-free" {
			continue // systemone api, not served here
		}
		if !strings.HasSuffix(id, "-free") && id != "big-pickle" {
			continue
		}
		if id == "union-alpha" {
			continue // not actually served by the free tier
		}
		out = append(out, modelEnt{ID: "opencode/" + id, Created: m.Created})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// known reports whether id is a model this gateway serves. Clients that ask
// for a model we do not have get the default instead of a hard failure.
func (s *Server) known(id string) bool {
	if id == "" {
		return false
	}
	id = shortID(id)
	for _, m := range s.catalog() {
		if shortID(m) == id {
			return true
		}
	}
	return false
}

// models serves the openai-compatible model list. Ids are live from
// opencode (through a lane, cached a minute); capability metadata is
// merged in from opencode's catalog mirror so clients can see
// reasoning effort levels, limits and modalities. Extra fields are
// ignored by strict openai clients.
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	s.Caps.MaybeRefresh()
	capsEntries := s.Caps.Entries()
	type ent struct {
		ID               string        `json:"id"`
		Object           string        `json:"object"`
		Created          int64         `json:"created,omitempty"`
		OwnedBy          string        `json:"owned_by"`
		Name             string        `json:"name,omitempty"`
		Reasoning        bool          `json:"reasoning"`
		ReasoningOptions []caps.Option `json:"reasoning_options,omitempty"`
		Limit            *caps.Limit   `json:"limit,omitempty"`
		ToolCall         bool          `json:"tool_call"`
		Attachment       bool          `json:"attachment"`
		Cost             caps.Cost     `json:"cost"`
		Input            []string      `json:"input,omitempty"`
		Output           []string      `json:"output,omitempty"`
		ContextLength    int           `json:"context_length,omitempty"`
		MaxOutputTokens  int           `json:"max_output_tokens,omitempty"`
	}
	mods := []ent{}
	for _, m := range s.catalogEntries() {
		e := ent{
			ID: m.ID, Object: "model", Created: m.Created, OwnedBy: "opencode",
			ToolCall: true, Attachment: true,
		}
		if c, ok := capsEntries[shortID(m.ID)]; ok {
			e.Name = c.Name
			e.Reasoning = c.Reasoning
			e.ReasoningOptions = c.ReasoningOptions
			e.Limit = &c.Limit
			e.ToolCall = c.ToolCall
			e.Attachment = c.Attachment
			e.Cost = c.Cost
			e.Input = c.Modalities.Input
			e.Output = c.Modalities.Output
			e.ContextLength = c.Limit.Context
			e.MaxOutputTokens = c.Limit.Output
		}
		mods = append(mods, e)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": mods})
}

func shortID(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "read body", 400)
		return nil, false
	}
	if int64(len(raw)) > maxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return raw, true
}

// chat serves openai chat completions. Responses-backed models are translated
// chat -> responses upstream and back, the rest pass through natively. Streams
// are relayed event by event, never buffered to the end.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	var chat map[string]any
	if err := json.Unmarshal(raw, &chat); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	model, _ := chat["model"].(string)
	model = shortID(model)
	if !s.known(model) {
		model = shortID(defaultModel)
	}
	chat["model"] = model
	wantStream, _ := chat["stream"].(bool)
	id := s.identity(r)

	// The free tier leases a session to the running client, so when one is
	// wired up the client is the upstream: our own request cannot get in.
	if s.ClientAPI != nil {
		s.chatViaClient(w, r, chat, model, wantStream)
		return
	}

	if !upstream.IsResponsesModel(model) {
		fwd := s.fingerprintChat(chat)
		if wantStream {
			s.streamChat(model, id, fwd, r, w, nil, nil)
			return
		}
		status, hdr, body := s.Up.Raw(model, id, fwd, r.Header)
		if status != 200 || !isSSE(hdr, body) {
			pass(w, status, hdr, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(translate.AggregateOpenAI(body, "opencode/"+model))
		return
	}

	respBody := s.fingerprintResponses(translate.ChatToResponses(chat), model)
	fwd, err := json.Marshal(respBody)
	if err != nil {
		http.Error(w, "encode", 500)
		return
	}
	if wantStream {
		s.streamChat(model, id, fwd, r, w,
			func() *translate.StreamConv { return &translate.StreamConv{} }, nil)
		return
	}
	status, hdr, body := s.Up.Raw(model, id, fwd, r.Header)
	if status != 200 || !isSSE(hdr, body) {
		pass(w, status, hdr, body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(translate.AggregateResponsesConv(body, "opencode/"+model))
}

// chatViaClient answers one chat request through the running opencode client.
//
// The request is parked in the bridge spool (when the plugin is installed), so
// the client-side plugin can rebuild the model call from the caller's own
// system prompt, history and tool schemas — the client's http api takes nothing
// but text. Tool calls the model makes are recorded by the plugin instead of
// being executed, and come back here as an openai tool_calls turn.
func (s *Server) chatViaClient(w http.ResponseWriter, r *http.Request, chat map[string]any, model string, wantStream bool) {
	req := parseChatRequest(chat)
	if req.Prompt == "" {
		http.Error(w, "chat requires messages", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	sessionID, err := s.ClientAPI.Create(ctx, "opencode", model, req.Variant)
	if err != nil {
		clientError(w, err)
		return
	}
	defer s.ClientAPI.Drop(context.WithoutCancel(ctx), sessionID)
	if s.BridgeDir != "" {
		parked := bridge.Request{SessionID: sessionID, System: req.System, Messages: req.Messages, Tools: req.Tools, Model: model}
		if err := bridge.Write(s.BridgeDir, parked); err != nil {
			clientError(w, err)
			return
		}
		defer bridge.Remove(s.BridgeDir, sessionID)
	}
	if err := s.ClientAPI.Prompt(ctx, sessionID, req.Prompt); err != nil {
		clientError(w, err)
		return
	}

	id := "chatcmpl-" + randHex(16)
	created := time.Now().Unix()
	name := "opencode/" + model
	var out *sseWriter
	if wantStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(200)
		out = &sseWriter{w: w}
		out.chunk(openaiChunk(id, created, name, map[string]any{"role": "assistant", "content": ""}, nil))
	}

	// Poll the client: text grows into the answer, and a tool call the plugin
	// recorded ends the turn — the caller runs that tool, not the client.
	var last string
	var calls []bridge.ToolCall
	var snap clientapi.Snapshot
	quiet := time.Now()
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(s.pollGap):
		}
		if err != nil {
			break
		}
		snap, err = s.ClientAPI.Snapshot(ctx, sessionID)
		if err != nil {
			break
		}
		if len(snap.Text) > len(last) {
			if out != nil {
				out.chunk(openaiChunk(id, created, name, map[string]any{"content": snap.Text[len(last):]}, nil))
			}
			last = snap.Text
			quiet = time.Now()
		}
		if s.BridgeDir != "" {
			if found := bridge.Calls(s.BridgeDir, sessionID); len(found) > 0 {
				calls = found
				break
			}
		}
		if snap.Idle || (last != "" && time.Since(quiet) > idleGrace) {
			break
		}
	}
	if snap.Idle {
		// the answer is over; the usage the client counted rides along
	} else if last == "" && len(calls) == 0 {
		if err == nil {
			err = fmt.Errorf("client stopped without an answer")
		}
		clientError(w, err)
		return
	}

	finish := finishReason(snap.Outcome, len(calls))
	if wantStream {
		if len(calls) > 0 {
			for i, c := range calls {
				out.chunk(openaiChunk(id, created, name, map[string]any{
					"tool_calls": []any{map[string]any{"index": i, "id": c.ID, "type": "function",
						"function": map[string]any{"name": c.Name, "arguments": ""}}}}, nil))
				out.chunk(openaiChunk(id, created, name, map[string]any{
					"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": string(c.Arguments)}}}}, nil))
			}
		}
		out.chunk(openaiChunk(id, created, name, map[string]any{}, &finish))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		out.flush()
		return
	}
	msg := map[string]any{"role": "assistant", "content": last}
	if len(calls) > 0 {
		msg["tool_calls"] = openAIToolCalls(calls)
		if last == "" {
			msg["content"] = nil
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": name,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   openAIUsage(snap.Usage),
	})
}

const idleGrace = 3 * time.Second

func openaiChunk(id string, created int64, model string, delta map[string]any, finish *string) map[string]any {
	var fr any
	if finish != nil {
		fr = *finish
	}
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fr}},
	}
}

func openAIToolCalls(calls []bridge.ToolCall) []any {
	out := make([]any, 0, len(calls))
	for _, c := range calls {
		args := string(c.Arguments)
		if args == "" {
			args = "{}"
		}
		out = append(out, map[string]any{
			"id": c.ID, "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": args},
		})
	}
	return out
}

// chatRequest is the caller's request, flattened into what the plugin needs.
type chatRequest struct {
	Prompt   string
	System   string
	Messages []bridge.Message
	Tools    []bridge.Tool
	Variant  string
}

// parseChatRequest reads an openai chat body: the last user text becomes the
// prompt that wakes the client, everything else travels through the spool.
func parseChatRequest(chat map[string]any) chatRequest {
	var out chatRequest
	out.Variant, _ = chat["reasoning_effort"].(string)
	msgs, _ := chat["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		role, _ := mm["role"].(string)
		text := messageText(mm["content"])
		switch role {
		case "system", "developer":
			if out.System != "" {
				out.System += "\n\n"
			}
			out.System += text
		case "assistant":
			am := bridge.Message{Role: "assistant", Content: text}
			if tcs, ok := mm["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					t, _ := tc.(map[string]any)
					fn, _ := t["function"].(map[string]any)
					name, _ := fn["name"].(string)
					args, _ := fn["arguments"].(string)
					id, _ := t["id"].(string)
					am.ToolCalls = append(am.ToolCalls, bridge.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)})
				}
			}
			out.Messages = append(out.Messages, am)
			if text != "" {
				out.Prompt = text
			}
		case "tool":
			id, _ := mm["tool_call_id"].(string)
			out.Messages = append(out.Messages, bridge.Message{Role: "tool", Content: text, ToolCallID: id})
		default:
			out.Messages = append(out.Messages, bridge.Message{Role: "user", Content: text})
			if text != "" {
				out.Prompt = text
			}
		}
	}
	if tools, ok := chat["tools"].([]any); ok {
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			fn, _ := tm["function"].(map[string]any)
			if fn == nil {
				fn = tm
			}
			name, _ := fn["name"].(string)
			if name == "" {
				continue
			}
			desc, _ := fn["description"].(string)
			params, _ := json.Marshal(fn["parameters"])
			if string(params) == "null" {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			out.Tools = append(out.Tools, bridge.Tool{Name: name, Description: desc, Parameters: params})
		}
	}
	return out
}

// finishReason maps the client's outcome onto an openai finish reason.
func finishReason(outcome string, calls int) string {
	if calls > 0 {
		return "tool_calls"
	}
	if outcome != "" && outcome != "succeeded" {
		return "error"
	}
	return "stop"
}

func clientError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(w, `{"error":{"type":"client_upstream_error","message":`+quote(err.Error())+`}}`)
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func openAIUsage(u clientapi.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":             u.Input,
		"completion_tokens":         u.Output,
		"total_tokens":              u.Input + u.Output,
		"prompt_tokens_details":     map[string]any{"cached_tokens": u.CacheRead},
		"completion_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning},
	}
}

func messageText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			pm, _ := p.(map[string]any)
			if s, _ := pm["text"].(string); s != "" {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

// sseWriter frames one openai chunk per event and flushes it, so tokens reach
// the caller while the model is still talking.
type sseWriter struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func (s *sseWriter) chunk(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.flush()
}

func (s *sseWriter) flush() {
	if s.fl == nil {
		s.fl, _ = s.w.(http.Flusher)
	}
	if s.fl != nil {
		s.fl.Flush()
	}
}

func randHex(n int) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b[:])[:n]
}

// fingerprintChat prepares a native chat body for the free gate.
func (s *Server) fingerprintChat(chat map[string]any) []byte {
	chat["stream"] = true
	upstream.FingerprintTools(chat, false)
	fwd, _ := json.Marshal(chat)
	return fwd
}

// fingerprintResponses prepares a responses body for the free gate.
func (s *Server) fingerprintResponses(body map[string]any, model string) map[string]any {
	body["model"] = model
	body["stream"] = true
	body["store"] = false
	upstream.FingerprintTools(body, true)
	return body
}

// streamChat relays a live upstream stream to w. conv is non-nil for
// responses-backed models (events must be translated to openai chunks first);
// nil means upstream already speaks openai chat. sink shapes the downstream
// dialect (openai chunks or anthropic events).
func (s *Server) streamChat(model, id string, fwd []byte, r *http.Request, w http.ResponseWriter, newConv func() *translate.StreamConv, sink translate.ChunkSink) {
	ctx := r.Context()
	if sink == nil {
		sink = translate.OpenAISink{}
	}
	var conv *translate.StreamConv
	if newConv != nil {
		conv = newConv()
	}
	out := "opencode/" + model
	finished := false
	status, err := s.Up.StreamEvents(model, id, fwd, r.Header, w,
		func(payload string, ev map[string]any) ([]string, bool) {
			if ctx.Err() != nil {
				return finishSink(sink, &finished), true // client hung up
			}
			if ev == nil {
				return finishSink(sink, &finished), true
			}
			if conv == nil {
				return sink.Chunk(ev), false
			}
			chunks, done := conv.FeedEvent(ev, out)
			var lines []string
			for _, c := range chunks {
				lines = append(lines, sink.Chunk(c)...)
			}
			if done {
				lines = append(lines, finishSink(sink, &finished)...)
			}
			return lines, done
		})
	if err != nil && status == 0 {
		// nothing was written yet: the client still gets a real status.
		http.Error(w, "upstream stream failed: "+err.Error(), 502)
	}
}

// finishSink emits the dialect's trailer exactly once.
func finishSink(sink translate.ChunkSink, done *bool) []string {
	if *done {
		return nil
	}
	*done = true
	return sink.Finish()
}

// responses passes native responses clients straight through, streamed.
func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	model, _ := body["model"].(string)
	if !s.known(model) {
		model = shortID(defaultModel)
	}
	// Read the client's intent BEFORE fingerprintResponses forces stream=true
	// on this same map — otherwise every stream:false request took the relay
	// branch below and got raw sse back.
	wantStream, _ := body["stream"].(bool)
	fwd := s.fingerprintResponses(body, model)
	if wantStream {
		b, err := json.Marshal(fwd)
		if err != nil {
			http.Error(w, "encode", 500)
			return
		}
		status, serr := s.Up.StreamRaw(model, s.identity(r), b, r.Header, w)
		if serr != nil && status == 0 {
			http.Error(w, "upstream stream failed: "+serr.Error(), 502)
		}
		return
	}
	b, err := json.Marshal(fwd)
	if err != nil {
		http.Error(w, "encode", 500)
		return
	}
	status, hdr, respBody := s.Up.Raw(model, s.identity(r), b, r.Header)
	// A stream:false client asked for one response object; upstream still
	// answers sse (the fingerprint forces streaming), so fold the terminal
	// event back into the response JSON instead of leaking sse bytes.
	if status == 200 && isSSE(hdr, respBody) {
		if agg, ok := translate.AggregateResponsesObject(respBody); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write(agg)
			return
		}
	}
	pass(w, status, hdr, respBody)
}

// messages serves anthropic-native clients. The body is translated to openai
// chat, run upstream, and translated back into the anthropic event sequence.
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	wantStream, _ := req["stream"].(bool)
	chat := translate.MessagesToChat(req, s.known, shortID(defaultModel))
	chat["stream"] = true
	model := chat["model"].(string)
	chat["model"] = model
	id := s.identity(r)
	echoModel, _ := req["model"].(string)
	thinking := req["thinking"] != nil

	if !upstream.IsResponsesModel(model) {
		fwd := s.fingerprintChat(chat)
		if wantStream {
			s.streamChat(model, id, fwd, r, w, nil,
				translate.NewAnthropicSink(echoModel, thinking))
			return
		}
		status, hdr, body := s.Up.Raw(model, id, fwd, r.Header)
		if status != 200 || !isSSE(hdr, body) {
			pass(w, status, hdr, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(translate.ChatToAnthropicMessage(
			translate.AggregateOpenAI(body, "opencode/"+model), echoModel))
		return
	}

	respBody := s.fingerprintResponses(translate.ChatToResponses(chat), model)
	fwd, err := json.Marshal(respBody)
	if err != nil {
		http.Error(w, "encode", 500)
		return
	}
	if wantStream {
		s.streamChat(model, id, fwd, r, w,
			func() *translate.StreamConv { return &translate.StreamConv{} },
			translate.NewAnthropicSink(echoModel, thinking))
		return
	}
	status, hdr, body := s.Up.Raw(model, id, fwd, r.Header)
	if status != 200 || !isSSE(hdr, body) {
		pass(w, status, hdr, body)
		return
	}
	agg := translate.AggregateResponsesConv(body, "opencode/"+model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(translate.ChatToAnthropicMessage(agg, echoModel))
}

func isSSE(hdr http.Header, body []byte) bool {
	ct := hdr.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream") || bytes.Contains(body, []byte("event:"))
}

func pass(w http.ResponseWriter, status int, hdr http.Header, body []byte) {
	for _, k := range []string{"Content-Type", "Retry-After"} {
		if v := hdr.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	if status == 0 {
		status = 502
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
