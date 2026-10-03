// Package server exposes the universal openai-compatible api backed only by
// the opencode free tier (no login, fingerprinted as the official client).
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

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

	catMu    sync.Mutex
	catCache []string
	catAt    time.Time
}

func New(ks *keys.Store, lm *lanes.Manager, up *upstream.Client) *Server {
	return &Server{Keys: ks, Lane: lm, Up: up}
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
		return true // open mode: no keys issued yet
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

// catalog asks upstream through a lane (never directly) and caches briefly so
// a chatty client does not spend a lane per /v1/models call.
func (s *Server) catalog() []string {
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if time.Since(s.catAt) < time.Minute && len(s.catCache) > 0 {
		return s.catCache
	}
	out := s.fetchCatalog()
	if len(out) == 0 {
		out = defaultModels
	}
	s.catCache, s.catAt = out, time.Now()
	return out
}

func (s *Server) fetchCatalog() []string {
	status, body := s.Up.GetJSON("/zen/v1/models")
	if status != 200 || len(body) == 0 {
		return nil
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil
	}
	var out []string
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
		out = append(out, "opencode/"+id)
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

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	type ent struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	var mods []ent
	for _, id := range s.catalog() {
		mods = append(mods, ent{ID: id, Object: "model", OwnedBy: "opencode"})
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
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", 400)
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
	fwd := s.fingerprintResponses(body, model)
	if stream, _ := body["stream"].(bool); stream {
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
