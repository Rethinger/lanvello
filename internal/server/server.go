// Package server exposes the universal openai-compatible api backed only by
// the opencode free tier (no login, fingerprinted as the official client).
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"lanvello/internal/keys"
	"lanvello/internal/lanes"
	"lanvello/internal/translate"
	"lanvello/internal/upstream"
)

type Server struct {
	Keys *keys.Store
	Lane *lanes.Manager
	Up   *upstream.Client
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

var freeModels = []string{
	"opencode/muse-spark-1.3-contributor-free",
	"opencode/muse-spark-1.2-contributor-free",
	"opencode/union-alpha",
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

// liveModels asks upstream for the current catalog and keeps free ids.
func (s *Server) liveModels() []string {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", s.Up.BaseURL+"/zen/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "opencode/1.18.31")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil
	}
	var out []string
	for _, m := range v.Data {
		id := m.ID
		if id == "jev-1.13-free" {
			continue // systemone api, not served yet
		}
		if strings.HasSuffix(id, "-free") || id == "big-pickle" || id == "union-alpha" {
			out = append(out, "opencode/"+id)
		}
	}
	return out
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
	ids := s.liveModels()
	if len(ids) == 0 {
		ids = freeModels
	}
	var mods []ent
	for _, id := range ids {
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

// chat serves openai chat completions. Responses-backed models are translated
// chat -> responses upstream and back, the rest pass through natively.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", 400)
		return
	}
	var chat map[string]any
	if err := json.Unmarshal(raw, &chat); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	model, _ := chat["model"].(string)
	model = shortID(model)
	if model == "" {
		model = "muse-spark-1.3-contributor-free"
		chat["model"] = model
	}
	wantStream, _ := chat["stream"].(bool)
	if !upstream.IsResponsesModel(model) {
		// native chat/messages path: forward as-is with fingerprint.
		nr, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw))
		nr.Header = r.Header.Clone()
		s.Up.Do(model, s.identity(r), w, nr)
		return
	}
	respBody := translate.ChatToResponses(chat)
	respBody["model"] = model
	upstream.FingerprintTools(respBody, true)
	fwd, _ := json.Marshal(respBody)
	status, hdr, body := s.Up.Raw(model, s.identity(r), fwd, r.Header)
	if status != 200 || !isSSE(hdr, body) {
		pass(w, status, hdr, body)
		return
	}
	if !wantStream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(translate.AggregateResponsesConv(body, "opencode/"+model))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	conv := &translate.StreamConv{}
	sc := bufio.NewScanner(bytes.NewReader(body))
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
		lines, done := conv.Feed(ev, "opencode/"+model)
		for _, l := range lines {
			io.WriteString(w, l+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		if done {
			break
		}
	}
}

// responses passes native responses clients straight through.
func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	s.Up.Do("", s.identity(r), w, r)
}

// messages passes anthropic-native bodies (union-alpha) straight through.
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	s.Up.Do("union-alpha", s.identity(r), w, r)
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
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
