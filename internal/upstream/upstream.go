// Package upstream forwards requests to the opencode free tier
// (https://opencode.ai/zen/v1) with the client fingerprint the free gate
// requires: opencode user-agent, x-opencode-* headers, stream:true and the
// bash/glob/grep/read tool quartet. No login, Bearer public.
package upstream

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"lanvello/internal/lanes"
)

const (
	ZenBase     = "https://opencode.ai"
	OpencodeUA  = "opencode/1.18.31"
	PublicToken = "public"
)

// ResponsesModels live on /zen/v1/responses, MessagesModels on
// /zen/v1/messages, everything else on /zen/v1/chat/completions.
// Matching mirrors the cli: provider prefix and "model(level)" thinking
// suffix are ignored, muse-spark matches by family regex.
var MessagesModels = map[string]bool{
	"union-alpha": true,
}

func baseID(m string) string {
	m = stripThinking(m)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m
}

func IsResponsesModel(m string) bool {
	b := strings.ToLower(baseID(m))
	if strings.HasPrefix(b, "muse-spark") || strings.HasPrefix(b, "muse_spark") {
		return true
	}
	return ResponsesModelsExact[b]
}

var ResponsesModelsExact = map[string]bool{}

func IsMessagesModel(m string) bool { return MessagesModels[strings.ToLower(baseID(m))] }

func stripThinking(m string) string {
	// "model(level)" -> "model"
	if i := strings.LastIndex(m, "("); i > 0 && strings.HasSuffix(m, ")") {
		return strings.TrimSpace(m[:i])
	}
	return m
}

type Client struct {
	BaseURL    string
	Lanes      *lanes.Manager
	HTTP       *http.Client
	WaitBudget time.Duration
	SessionFor func(identity string) string
}

func NewClient(m *lanes.Manager) *Client {
	return &Client{
		BaseURL:    ZenBase,
		Lanes:      m,
		HTTP:       &http.Client{Timeout: 180 * time.Second},
		WaitBudget: 90 * time.Second,
		SessionFor: func(identity string) string { return StableSession(identity) },
	}
}

const b62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func randB62(n int) string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = b62[int(b[i])%62]
	}
	return string(out)
}

// NewSessionID mirrors the cli shape: ses_<12hex><14base62>.
func NewSessionID() string {
	var h [6]byte
	_, _ = rand.Read(h[:])
	return "ses_" + hex.EncodeToString(h[:]) + randB62(14)
}

// NewRequestID mirrors the cli shape: msg_<12hex><14base62>.
func NewRequestID() string {
	var h [6]byte
	_, _ = rand.Read(h[:])
	return "msg_" + hex.EncodeToString(h[:]) + randB62(14)
}

// StableSession derives one long-lived session per downstream identity:
// minting a fresh session per request burns free quota into 429s.
func StableSession(identity string) string {
	sum := sha256.Sum256([]byte("opencode\x00" + identity))
	return "ses_" + hex.EncodeToString(sum[:6]) + b62map(sum[6:20])
}

func b62map(b []byte) string {
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = b62[int(v)%62]
	}
	return string(out)
}

func (c *Client) clientFor(lane *lanes.Lane) *http.Client {
	if lane == nil || lane.SocksAddr == "" {
		return c.HTTP
	}
	dialer, err := proxy.SOCKS5("tcp", lane.SocksAddr, nil, proxy.Direct)
	if err != nil {
		return c.HTTP
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
	}
	return &http.Client{Transport: transport, Timeout: 180 * time.Second}
}

func endpointFor(model string) string {
	if IsResponsesModel(model) {
		return "/zen/v1/responses"
	}
	if IsMessagesModel(model) {
		return "/zen/v1/messages"
	}
	return "/zen/v1/chat/completions"
}

// FingerprintTools appends the missing bash/glob/grep/read decoys.
// flat=false is chat shape, flat=true is responses shape.
func FingerprintTools(body map[string]any, flat bool) {
	present := map[string]bool{}
	var list []any
	if arr, ok := body["tools"].([]any); ok {
		list = arr
		for _, t := range arr {
			if n := toolName(t); n != "" {
				present[strings.ToLower(n)] = true
			}
		}
	}
	for _, name := range []string{"bash", "glob", "grep", "read"} {
		if present[name] {
			continue
		}
		if flat {
			list = append(list, map[string]any{
				"type": "function", "name": name,
				"description": "This tool is currently unavailable and must not be used.",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			})
		} else {
			list = append(list, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": "This tool is currently unavailable and must not be used.",
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			})
		}
	}
	body["tools"] = list
	if _, ok := body["tool_choice"]; !ok && !flat {
		body["tool_choice"] = "none"
	}
}

func toolName(t any) string {
	m, ok := t.(map[string]any)
	if !ok {
		return ""
	}
	if n, ok := m["name"].(string); ok && n != "" {
		return strings.TrimSpace(n)
	}
	if fn, ok := m["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok {
			return strings.TrimSpace(n)
		}
	}
	return ""
}

// Do forwards one request with lane rotation. identity scopes the stable
// session (use the caller api key). streamUp forces sse upstream.
func (c *Client) Do(model, identity string, w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), 400)
		return
	}
	if m := bodyModel(raw); m != "" {
		model = m
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	flat := IsResponsesModel(model)
	model = stripThinking(model)
	if i := strings.Index(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	body["model"] = model
	body["stream"] = true
	if flat {
		body["store"] = false
		sanitizeResponsesInput(body)
		if _, ok := body["input"]; !ok {
			http.Error(w, "responses requires input", 400)
			return
		}
		normalizeReasoning(body)
		if _, ok := body["tool_choice"]; !ok {
			body["tool_choice"] = "auto"
		}
	} else if !IsMessagesModel(model) {
		if _, ok := body["messages"]; !ok {
			http.Error(w, "chat requires messages", 400)
			return
		}
	}
	FingerprintTools(body, flat)
	fwd, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "encode", 500)
		return
	}
	status, hdr, respBody := c.Raw(model, identity, fwd, r.Header)
	relay(w, status, hdr, respBody)
}

// normalizeReasoning maps reasoning_effort onto reasoning{effort,summary}.
func normalizeReasoning(body map[string]any) {
	cur, _ := body["reasoning"].(map[string]any)
	eff, _ := body["reasoning_effort"].(string)
	if cur == nil && eff == "" {
		if e, ok := body["reasoning"].(string); ok {
			eff = e
		}
	}
	if eff == "" {
		return
	}
	if cur == nil {
		cur = map[string]any{}
	}
	cur["effort"] = strings.ToLower(strings.TrimSpace(eff))
	if _, ok := cur["summary"]; !ok {
		cur["summary"] = "auto"
	}
	body["reasoning"] = cur
	delete(body, "reasoning_effort")
}

// Raw sends a prepared upstream body with lane rotation and returns the raw
// upstream answer (sse bytes).
func (c *Client) Raw(model, identity string, fwd []byte, in http.Header) (int, http.Header, []byte) {
	url := c.BaseURL + endpointFor(model)
	session := identity
	if !ValidSession(session) {
		session = c.SessionFor(identity)
	}
	var probe map[string]any
	if err := json.Unmarshal(fwd, &probe); err == nil {
		normalizeReasoning(probe)
		if b, err := json.Marshal(probe); err == nil {
			fwd = b
		}
	}
	reqID := deriveRequestID(session, lastUserText(probe))

	tried := map[int]bool{}
	deadline := time.Now().Add(c.WaitBudget)
	var lastErr string
	for {
		lane := c.Lanes.Pick(tried)
		if lane == nil {
			if time.Now().After(deadline) {
				h := http.Header{}
				h.Set("Content-Type", "application/json")
				return 502, h, []byte(`{"error":"no lane available` + suffix(lastErr) + `"}`)
			}
			time.Sleep(500 * time.Millisecond)
			tried = map[int]bool{}
			continue
		}
		tried[lane.Index] = true
		status, hdr, respBody, rerr := c.roundTrip(lane, url, session, reqID, fwd, in)
		if rerr != nil {
			lastErr = rerr.Error()
			c.Lanes.Release(lane)
			continue
		}
		c.Lanes.Release(lane)
		switch {
		case status == 429:
			ra := parseRetryAfter(hdr.Get("Retry-After"))
			until := c.Lanes.NoteLimited(lane, ra)
			c.Lanes.Rotate(lane)
			c.Lanes.Emit(lanes.Proof{T: time.Now(), Lane: lane.Index, Country: lane.Country, IP: lane.ExitIP, Model: model, Status: 429, Note: "exit limited until " + until.Format(time.RFC3339)})
			continue
		case status == 502 || status == 503 || status == 504:
			continue
		case status == 500:
			// Chat-path 500s are transient far-end failures; rotate once.
			if len(tried) < len(c.Lanes.Lanes()) {
				continue
			}
			return status, hdr, respBody
		default:
			c.Lanes.NoteResult(lane.Country, status)
			c.Lanes.Emit(lanes.Proof{T: time.Now(), Lane: lane.Index, Country: lane.Country, IP: lane.ExitIP, Model: model, Status: status, Bytes: int64(len(respBody))})
			return status, hdr, respBody
		}
	}
}

func (c *Client) roundTrip(lane *lanes.Lane, url, session, reqID string, body []byte, in http.Header) (int, http.Header, []byte, error) {
	hc := c.clientFor(lane)
	req, err := http.NewRequest("POST", url, bytesReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+PublicToken)
	ua := in.Get("User-Agent")
	if !validOpencodeUA(ua) {
		ua = OpencodeUA
	}
	req.Header.Set("User-Agent", ua)
	if v := in.Get("x-opencode-client"); v != "" {
		req.Header.Set("x-opencode-client", v)
	} else {
		req.Header.Set("x-opencode-client", "desktop")
	}
	req.Header.Set("x-opencode-session", session)
	req.Header.Set("x-opencode-request", reqID)
	req.Header.Set("x-opencode-project", "global")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	return resp.StatusCode, resp.Header, b, nil
}

func validOpencodeUA(ua string) bool {
	s := strings.ToLower(ua)
	i := strings.Index(s, "opencode/")
	if i < 0 {
		return false
	}
	rest := s[i+len("opencode/"):]
	var major, minor int
	n, _ := fmt.Sscanf(rest, "%d.%d", &major, &minor)
	if n < 2 {
		return false
	}
	return major > 1 || (major == 1 && minor >= 17)
}

func relay(w http.ResponseWriter, status int, hdr http.Header, body []byte) {
	for _, k := range []string{"Content-Type", "Retry-After"} {
		if v := hdr.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

var sesRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var msgRe = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func ValidSession(s string) bool { return sesRe.MatchString(s) }

// deriveRequestID is stable per (session, last user text) so retries share
// the id instead of burning quota as fresh requests.
func deriveRequestID(session, text string) string {
	if text == "" {
		return NewRequestID()
	}
	sum := sha256.Sum256([]byte("opencode-req\x00" + session + "\x00" + text))
	id := "msg_" + hex.EncodeToString(sum[:6]) + b62map(sum[6:20])
	if !msgRe.MatchString(id) {
		return NewRequestID()
	}
	return id
}

// lastUserText extracts the trailing user text from chat or responses bodies.
func lastUserText(body map[string]any) string {
	if arr, ok := body["messages"].([]any); ok {
		for i := len(arr) - 1; i >= 0; i-- {
			mm, ok := arr[i].(map[string]any)
			if !ok || mm["role"] != "user" {
				continue
			}
			if s, ok := mm["content"].(string); ok && strings.TrimSpace(s) != "" {
				t := strings.TrimSpace(s)
				if len(t) > 600 {
					t = t[len(t)-600:]
				}
				return t
			}
		}
		return ""
	}
	if s, ok := body["input"].(string); ok {
		return s
	}
	if arr, ok := body["input"].([]any); ok {
		for i := len(arr) - 1; i >= 0; i-- {
			mm, ok := arr[i].(map[string]any)
			if !ok || mm["role"] != "user" {
				continue
			}
			if parts, ok := mm["content"].([]any); ok {
				var sb strings.Builder
				for _, p := range parts {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					if t, ok := pm["text"].(string); ok {
						sb.WriteString(t + " ")
					}
				}
				if t := strings.TrimSpace(sb.String()); t != "" {
					if len(t) > 600 {
						t = t[len(t)-600:]
					}
					return t
				}
			}
		}
	}
	return ""
}

// sanitizeResponsesInput normalizes native responses input the way the
// official client does: string -> message array, empty -> placeholder,
// reasoning items and encrypted blobs dropped (re-sending another
// caller/account encrypted_content 400s), call ids clamped.
func sanitizeResponsesInput(body map[string]any) {
	if s, ok := body["input"].(string); ok {
		if strings.TrimSpace(s) == "" {
			s = "..."
		}
		body["input"] = []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": s}},
		}}
		return
	}
	arr, ok := body["input"].([]any)
	if !ok {
		return
	}
	if len(arr) == 0 {
		body["input"] = []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "..."}},
		}}
		return
	}
	kept := arr[:0]
	for _, it := range arr {
		mm, ok := it.(map[string]any)
		if !ok {
			kept = append(kept, it)
			continue
		}
		if mm["type"] == "reasoning" {
			continue
		}
		delete(mm, "encrypted_content")
		delete(mm, "reasoning_encrypted_content")
		if mm["type"] == "function_call" {
			if id, ok := mm["call_id"].(string); ok {
				mm["call_id"] = clampCallID(id)
			}
			if a := mm["arguments"]; a != nil {
				if s, ok := a.(string); !ok || !validJSON(s) {
					if s, ok := a.(string); ok && s == "" {
						mm["arguments"] = "{}"
					} else if !ok {
						if b, err := json.Marshal(a); err == nil {
							mm["arguments"] = string(b)
						} else {
							mm["arguments"] = "{}"
						}
					} else {
						mm["arguments"] = "{}"
					}
				}
			}
		}
		if mm["type"] == "function_call_output" {
			if id, ok := mm["call_id"].(string); ok {
				mm["call_id"] = clampCallID(id)
			}
			switch o := mm["output"].(type) {
			case string:
			case nil:
				mm["output"] = ""
			default:
				if b, err := json.Marshal(o); err == nil {
					mm["output"] = string(b)
				} else {
					mm["output"] = ""
				}
			}
		}
		kept = append(kept, mm)
	}
	body["input"] = kept
}

func clampCallID(id string) string {
	if id == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		return "call_" + hex.EncodeToString(b[:])
	}
	if len(id) > 64 {
		return id[:64]
	}
	return id
}

func validJSON(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

func suffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

func bodyModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Model
}

type readerFunc struct{}

func bytesReader(b []byte) io.Reader { return &byteReader{b: b} }

type byteReader struct {
	b []byte
	i int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

var _ = bufio.ErrTooLong
