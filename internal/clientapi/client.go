// Package clientapi drives the running opencode client as the model upstream.
//
// It exists because the free tier leases a session to the client process only:
// a replayed request (our own headers, our own session id) is answered with 403
// FreeTierError, while the same bytes carrying a session the client just used
// are answered normally. So the client makes the call, and this package is the
// thin, honest contract with it:
//
//	POST /api/session              create a session pinned to a model
//	POST /api/session/{id}/prompt  send the text
//	GET  /api/session/{id}/message read the assistant text (and the idle part)
//	DELETE /api/session/{id}       drop it again
//
// Auth is HTTP Basic with the username "opencode" and the password the client
// writes into its own service.json.
package clientapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to one opencode client server.
type Client struct {
	BaseURL  string
	Password string
	HTTP     *http.Client
}

// Usage is what the client reports for a finished session.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Reasoning  int `json:"reasoning"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

type serviceFile struct {
	URL      string `json:"url"`
	Password string `json:"password"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
}

// ServicePath is where the client records the server it started.
func ServicePath() string {
	if v := os.Getenv("OPENCODE_SERVICE_FILE"); v != "" {
		return v
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, _ := os.UserHomeDir()
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "opencode", "service.json")
}

// Discover returns a client for the running opencode server, or an error when
// there is none: without the client there is no free tier, and pretending
// otherwise is how a gateway ends up serving 403s.
func Discover() (*Client, error) {
	path := ServicePath()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no opencode client server (%s): %w", path, err)
	}
	var sf serviceFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, fmt.Errorf("bad %s: %w", path, err)
	}
	if sf.URL == "" || sf.Password == "" {
		return nil, fmt.Errorf("%s has no url/password", path)
	}
	return &Client{
		BaseURL:  strings.TrimRight(sf.URL, "/"),
		Password: sf.Password,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	token := base64.StdEncoding.EncodeToString([]byte("opencode:" + c.Password))
	req.Header.Set("Authorization", "Basic "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("client %s %s: HTTP %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 200)])))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("client %s %s: %w", method, path, err)
	}
	return nil
}

// Ready reports whether the client is answering, which is what decides if free
// models can be served at all.
func (c *Client) Ready(ctx context.Context) error {
	var v map[string]any
	return c.do(ctx, "GET", "/api/info", nil, &v)
}

// Model is one entry of the client's own model list.
type Model struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Name       string `json:"name"`
}

// Models returns what the client can drive right now. This is the honest
// catalog for a client-backed gateway: the client's list is the one its own
// requests are built from, retired ids included.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out struct {
		Data []Model `json:"data"`
	}
	if err := c.do(ctx, "GET", "/api/model", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// Create opens a session pinned to a model.
func (c *Client) Create(ctx context.Context, provider, model, variant string) (string, error) {
	ref := map[string]any{"providerID": provider, "id": model}
	if variant != "" && variant != "default" {
		ref["variant"] = variant
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, "POST", "/api/session", map[string]any{"model": ref}, &out); err != nil {
		return "", err
	}
	if out.Data.ID == "" {
		return "", fmt.Errorf("client returned no session id")
	}
	return out.Data.ID, nil
}

// Drop deletes a session: a gateway call should not litter the client's list.
func (c *Client) Drop(ctx context.Context, sessionID string) {
	_ = c.do(ctx, "DELETE", "/api/session/"+sessionID, nil, nil)
}

// Prompt sends the text that will be answered.
func (c *Client) Prompt(ctx context.Context, sessionID, text string) error {
	return c.do(ctx, "POST", "/api/session/"+sessionID+"/prompt", map[string]any{"text": text}, nil)
}

type part struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Outcome string `json:"outcome"`
	Text    string `json:"text"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type messagePage struct {
	Data []part `json:"data"`
}

func (p part) answer() string {
	var b strings.Builder
	for _, c := range p.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// Snapshot is the state of one session's answer right now: the text so far,
// whether the turn is over, and — once it is — what it cost.
type Snapshot struct {
	Text    string
	Idle    bool
	Outcome string
	Usage   Usage
}

// Snapshot reads a session's answer. The caller decides when to stop asking
// (its own deadline, an idle part, or a tool call it wants to answer itself).
func (c *Client) Snapshot(ctx context.Context, sessionID string) (Snapshot, error) {
	var page messagePage
	if err := c.do(ctx, "GET", "/api/session/"+sessionID+"/message", nil, &page); err != nil {
		return Snapshot{}, err
	}
	var out Snapshot
	for _, p := range page.Data {
		switch p.Type {
		case "assistant":
			out.Text = p.answer()
		case "idle":
			out.Idle = true
			out.Outcome = p.Outcome
		}
	}
	if out.Idle {
		c.fillUsage(ctx, sessionID, &out)
	}
	return out, nil
}

func (c *Client) fillUsage(ctx context.Context, sessionID string, snap *Snapshot) {
	var out struct {
		Data struct {
			Tokens struct {
				Input     int `json:"input"`
				Output    int `json:"output"`
				Reasoning int `json:"reasoning"`
				Cache     struct {
					Read  int `json:"read"`
					Write int `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
			Outcome string `json:"outcome"`
		} `json:"data"`
	}
	if err := c.do(ctx, "GET", "/api/session/"+sessionID, nil, &out); err != nil {
		return
	}
	t := out.Data.Tokens
	snap.Usage = Usage{Input: t.Input, Output: t.Output, Reasoning: t.Reasoning, CacheRead: t.Cache.Read, CacheWrite: t.Cache.Write}
	if out.Data.Outcome != "" {
		snap.Outcome = out.Data.Outcome
	}
}
