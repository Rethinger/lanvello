// Package server exposes the universal openai-compatible api.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"lanvello/internal/keys"
	"lanvello/internal/lanes"
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
	m.HandleFunc("/v1/responses", s.chat)
	m.HandleFunc("/v1/messages", s.anthropic)
	return m
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

type modelEnt struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
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
	mods := []modelEnt{
		{ID: "opencode/muse-spark-1.3-contributor-free", Object: "model", OwnedBy: "opencode"},
		{ID: "opencode/muse-spark-1.2-free", Object: "model", OwnedBy: "opencode"},
		{ID: "opencode/longcat-2.5-preview-free", Object: "model", OwnedBy: "opencode"},
		{ID: "opencode/nemotron-3-ultra-free", Object: "model", OwnedBy: "opencode"},
		{ID: "opencode/space-bunny-free", Object: "model", OwnedBy: "opencode"},
		{ID: "lanvello/auto", Object: "model", OwnedBy: "lanvello"},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": mods})
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	model := sniffModel(r)
	s.Up.Do(model, w, r)
}

// anthropic accepts native messages format and maps it minimally to chat.
func (s *Server) anthropic(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", 400)
		return
	}
	var in struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		System    any    `json:"system"`
		Messages  any    `json:"messages"`
		MaxTokens *int   `json:"max_tokens"`
	}
	_ = json.Unmarshal(b, &in)
	model := in.Model
	if model == "" {
		model = "lanvello/auto"
	}
	msgs := "[]"
	if in.Messages != nil {
		if mb, err := json.Marshal(in.Messages); err == nil {
			msgs = string(mb)
		}
	}
	sys := ""
	if in.System != nil {
		if sb, err := json.Marshal(in.System); err == nil {
			sys = string(sb)
		}
	}
	maxT := 1024
	if in.MaxTokens != nil {
		maxT = *in.MaxTokens
	}
	openai := `{"model":` + quote(model) + `,"messages":[{"role":"system","content":` + quote(sys) + `},{"role":"user","content":` + quote(msgs) + `}],"max_tokens":` + itoa(maxT) + `,"stream":` + boolStr(in.Stream) + `}`
	req, _ := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(openai))
	req.Header = r.Header.Clone()
	s.Up.Do(model, w, req)
}

func sniffModel(r *http.Request) string {
	// body already consumed by upstream.Do; best effort from query.
	if m := r.URL.Query().Get("model"); m != "" {
		return m
	}
	return "lanvello/auto"
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

var _ = time.Now
