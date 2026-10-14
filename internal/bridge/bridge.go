// Package bridge carries one api request across the client boundary.
//
// The gateway cannot hand a caller's system prompt, history and tool schemas to
// the free tier itself: only the running client gets a session, and its own http
// api takes nothing but text. So the request is parked in a spool directory the
// client-side plugin reads, keyed by the session the gateway just opened:
//
//	<dir>/<sessionID>.json        what the caller asked for (system, messages, tools)
//	<dir>/<sessionID>.calls.jsonl what the model called, written by the plugin
//
// The plugin rewrites the model request from the .json (its `session.hook
// ("context")` hands it system, messages and tools), blocks execution of the
// caller's tools and appends each call to the .jsonl instead. The gateway reads
// that back and answers the api caller with an openai tool_calls turn.
package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Message is one turn of the caller's conversation, flattened enough for the
// plugin to rebuild it: text, plus the tool call/result that travelled with it.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is an openai tool call, either from the caller's history or from the
// model (the plugin fills id/name/arguments).
type ToolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// Tool is one tool the caller declared, in the shape the plugin expects
// (the client's own tool entries are {description, input}).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Request is what the plugin reads.
type Request struct {
	SessionID string    `json:"sessionId"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
	Model     string    `json:"model"`
	Created   time.Time `json:"created"`
}

// Dir is where the spool lives. The plugin reads the same path (it honours the
// same env var), so a moved data dir moves both ends.
func Dir(dataDir string) string {
	if v := os.Getenv("LANVELLO_BRIDGE_DIR"); v != "" {
		return v
	}
	return filepath.Join(dataDir, "bridge")
}

func requestPath(dir, sessionID string) string { return filepath.Join(dir, sessionID+".json") }
func callsPath(dir, sessionID string) string   { return filepath.Join(dir, sessionID+".calls.jsonl") }

// Write parks one request for the plugin.
func Write(dir string, r Request) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	r.Created = time.Now()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := requestPath(dir, r.SessionID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, requestPath(dir, r.SessionID))
}

// Remove drops a finished request together with whatever calls it produced: a
// later request reuses the session id space and must not inherit old calls.
func Remove(dir, sessionID string) {
	_ = os.Remove(requestPath(dir, sessionID))
	_ = os.Remove(callsPath(dir, sessionID))
}

// Calls reads the tool calls the plugin recorded so far.
func Calls(dir, sessionID string) []ToolCall {
	f, err := os.Open(callsPath(dir, sessionID))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []ToolCall
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c ToolCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			continue
		}
		if c.Name == "" {
			continue
		}
		if len(c.Arguments) == 0 {
			c.Arguments = c.Input
		}
		if len(c.Arguments) == 0 {
			c.Arguments = json.RawMessage("{}")
		}
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d_%s", len(out), c.Name)
		}
		out = append(out, c)
	}
	return out
}

// Sweep removes spool entries older than ttl: a crashed gateway must not leave
// the plugin rewriting requests that will never be read again.
func Sweep(dir string, ttl time.Duration) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cut := time.Now().Add(-ttl)
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cut) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
