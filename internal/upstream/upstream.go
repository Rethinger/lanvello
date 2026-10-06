// Package upstream forwards requests to the opencode free tier
// (https://opencode.ai/zen/v1) with the client fingerprint the free gate
// requires: opencode user-agent, x-opencode-* headers, stream:true and the
// bash/glob/grep/read tool quartet. No login, Bearer public.
//
// Every byte leaves through a lane. There is no code path that talks to the
// free tier directly: a missing lane means an error, never a direct dial.
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

// MessagesModels live on /zen/v1/messages. union-alpha is the only id that
// ever did, and the free tier does not serve it, so /v1/messages translates
// to a chat-backed model instead of proxying. kept so a future tier that
// does serve it routes correctly.
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
	// WantCountry maps model prefix -> exit country (union-alpha -> us).
	WantCountry map[string]string
	// CatalogBudget bounds the lane wait for cheap calls like /v1/models.
	CatalogBudget time.Duration
}

func (c *Client) countryFor(model string) string {
	best, cc := "", ""
	for pref, country := range c.WantCountry {
		if pref != "" && strings.HasPrefix(model, pref) && len(pref) > len(best) {
			best, cc = pref, country
		}
	}
	return cc
}

func NewClient(m *lanes.Manager) *Client {
	return &Client{
		BaseURL: ZenBase,
		Lanes:   m,
		// No client-level timeout: a streamed answer lives for as long as the
		// model thinks. Waiting for response headers is bounded, and a stalled
		// body is caught per-read (see headerTimeout / streamIdle).
		HTTP:          &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: headerTimeout}},
		WaitBudget:    90 * time.Second,
		CatalogBudget: 20 * time.Second,
		SessionFor:    func(identity string) string { return StableSession(identity) },
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
		ResponseHeaderTimeout: headerTimeout,
	}
	return &http.Client{Transport: transport}
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
	if _, ok := body["tool_choice"]; !ok {
		// never "none": a client that sent tools without an explicit choice
		// expects the model to be able to call them.
		body["tool_choice"] = "auto"
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

// Do forwards one non-streaming request with lane rotation.
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
	model = bareID(model)
	body["model"] = model
	body["stream"] = true
	if flat {
		body["store"] = false
		sanitizeResponsesInput(body)
		if _, ok := body["input"]; !ok {
			http.Error(w, "responses requires input", 400)
			return
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
// The responses endpoint speaks reasoning{}, chat/messages speak
// reasoning_effort; effort must land on whichever the endpoint accepts.
func normalizeReasoning(body map[string]any) {
	cur, _ := body["reasoning"].(map[string]any)
	eff, _ := body["reasoning_effort"].(string)
	if eff == "" {
		if e, ok := body["reasoning"].(string); ok {
			eff = e
			cur = nil
		}
	}
	if eff == "" && cur == nil {
		return
	}
	if cur == nil {
		cur = map[string]any{}
	}
	if e, ok := cur["effort"].(string); ok && strings.TrimSpace(e) != "" {
		eff = e
	}
	if eff != "" {
		cur["effort"] = strings.ToLower(strings.TrimSpace(eff))
	}
	// the free tier expects an explicit summary mode alongside the effort
	if _, ok := cur["summary"]; !ok {
		cur["summary"] = "auto"
	}
	body["reasoning"] = cur
	delete(body, "reasoning_effort")
}

// normalizeEffortForPath applies the endpoint's own spelling of effort.
func normalizeEffortForPath(path string, body map[string]any) {
	if path == "/zen/v1/responses" {
		normalizeReasoning(body)
		return
	}
	eff, _ := body["reasoning_effort"].(string)
	if eff == "" {
		if r, ok := body["reasoning"].(map[string]any); ok {
			eff, _ = r["effort"].(string)
		}
	}
	delete(body, "reasoning")
	if eff != "" {
		body["reasoning_effort"] = eff
	}
}

// Raw sends a prepared upstream body with lane rotation and returns the raw
// upstream answer (sse bytes). Used for stream:false aggregation.
func (c *Client) Raw(model, identity string, fwd []byte, in http.Header) (int, http.Header, []byte) {
	var outH http.Header
	var outB []byte
	status, err := c.loop(model, identity, "POST", endpointFor(model), fwd, in,
		func(resp *http.Response, st int, hdr http.Header) (bool, error) {
			outH = hdr
			b, derr := drain(resp)
			outB = b
			return !retryable(st), derr
		})
	if err != nil {
		return status, jsonHeader(), []byte(`{"error":` + quote(err.Error()) + `}`)
	}
	if outH == nil {
		outH = jsonHeader()
	}
	return status, outH, outB
}

// GetJSON fetches a small upstream endpoint through a lane. The model
// catalog must not leave the machine directly, so it uses the same rotation.
func (c *Client) GetJSON(path string) (int, []byte) {
	return c.GetURL(path, c.CatalogBudget)
}

// GetURL fetches any URL through a lane with an explicit budget. The
// capability catalog lives on opencode's mirror host, not on the zen
// api host, so the "path" is a full URL there. As with GetJSON there
// is no direct egress: the request leaves through whichever lane the
// rotation picks, or it does not leave at all.
func (c *Client) GetURL(rawURL string, budget time.Duration) (int, []byte) {
	var out []byte
	if budget <= 0 {
		budget = 20 * time.Second
	}
	status, _ := c.loopBudget("catalog", "catalog", "GET", rawURL, nil, nil, budget,
		func(resp *http.Response, st int, hdr http.Header) (bool, error) {
			b, derr := drain(resp)
			out = b
			return !retryable(st), derr
		})
	return status, out
}

// EventHandler consumes one upstream sse event and returns downstream lines.
// payload is the raw data text ("[DONE]" sentinel included), ev is nil for
// non-json payloads.
type EventHandler func(payload string, ev map[string]any) (lines []string, done bool)

// StreamEvents pipes the live upstream stream to fn, flushing each line as it
// arrives. 429/5xx rotate to another lane until the first byte is committed.
func (c *Client) StreamEvents(model, identity string, fwd []byte, in http.Header, w http.ResponseWriter, fn EventHandler) (int, error) {
	return c.stream(model, identity, fwd, in, w, func(sink *streamSink, resp *http.Response) error {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 256*1024), 8*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				continue
			}
			if payload == "[DONE]" {
				lines, _ := fn(payload, nil)
				writeLines(sink, lines)
				return nil
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue
			}
			lines, done := fn(payload, ev)
			writeLines(sink, lines)
			if done {
				return nil
			}
		}
		return sc.Err()
	})
}

func writeLines(sink *streamSink, lines []string) {
	for _, l := range lines {
		io.WriteString(sink, l+"\n\n")
	}
	sink.flush()
}

// StreamRaw relays upstream sse bytes verbatim, flushing as they arrive.
func (c *Client) StreamRaw(model, identity string, fwd []byte, in http.Header, w http.ResponseWriter) (int, error) {
	return c.stream(model, identity, fwd, in, w, func(sink *streamSink, resp *http.Response) error {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := sink.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if rerr != nil {
				if rerr == io.EOF {
					return nil
				}
				return rerr
			}
		}
	})
}

// headerTimeout bounds the wait for response headers. The free tier answers
// with its first event long before the model finishes thinking.
var headerTimeout = 3 * time.Minute

// streamIdle is the per-read stall budget: a model that thinks for 10 minutes
// between tokens must not be cut, a dead socket must not hang forever.
var streamIdle = 10 * time.Minute

// streamError is what the caller writes when nothing could be streamed.
type StreamError struct {
	Status int
	Err    error
}

func (e *StreamError) Error() string { return e.Err.Error() }

// stream hands the live upstream body to body, retrying retryable answers on
// another lane before a single byte is written downstream.
func (c *Client) stream(model, identity string, fwd []byte, in http.Header, w http.ResponseWriter, body func(sink *streamSink, resp *http.Response) error) (int, error) {
	var sink *streamSink
	committed := false
	status, err := c.loop(model, identity, "POST", endpointFor(model), fwd, in,
		func(resp *http.Response, st int, hdr http.Header) (bool, error) {
			if retryable(st) {
				_, _ = drain(resp)
				return false, nil
			}
			if st != 200 {
				b, _ := drain(resp)
				relay(w, st, hdr, b)
				committed = true
				return true, nil
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(200)
			committed = true
			sink = &streamSink{w: w}
			sink.flush()
			return true, body(sink, resp)
		})
	if err != nil && !committed {
		// Nothing was written downstream yet (no lane up, upstream down):
		// answer with a real status. Returning without a body let net/http
		// send an empty 200, hiding the failure from the client.
		st := http.StatusBadGateway
		if se, ok := err.(*StreamError); ok && se.Status != 0 {
			st = se.Status
		}
		relay(w, st, jsonHeader(), []byte(`{"error":`+quote(err.Error())+`}`))
		return st, err
	}
	if err != nil {
		if se, ok := err.(*StreamError); ok {
			return se.Status, se.Err
		}
		if status == 0 {
			return http.StatusBadGateway, err
		}
	}
	return status, err
}

// streamSink flushes every write so tokens reach the client as they arrive.
type streamSink struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func (s *streamSink) flush() {
	if s.fl == nil {
		s.fl, _ = s.w.(http.Flusher)
	}
	if s.fl != nil {
		s.fl.Flush()
	}
}

func (s *streamSink) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.flush()
	return n, err
}

// consumer handles one upstream attempt: stop=true ends the rotation loop,
// stop=false asks for another lane (body already drained).
type consumer func(resp *http.Response, status int, hdr http.Header) (stop bool, err error)

// loop picks a lane, sends one attempt and applies the rotation policy:
// 429 parks the exit for retry-after, 5xx tries the next lane.
func (c *Client) loop(model, identity, method, path string, fwd []byte, in http.Header, consume consumer) (int, error) {
	return c.loopBudget(model, identity, method, path, fwd, in, c.WaitBudget, consume)
}

// loopBudget is loop with an explicit lane-wait budget, so cheap calls (the
// model catalog) do not sit in the same queue as a 1M-token answer.
func (c *Client) loopBudget(model, identity, method, path string, fwd []byte, in http.Header, budget time.Duration, consume consumer) (int, error) {
	// path is a bare path (appended to the zen base url) or a full URL
	// (the capability mirror on another host). Both leave through a lane.
	url := path
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = c.BaseURL + path
	}
	session := identity
	if !ValidSession(session) {
		session = c.SessionFor(identity)
	}
	var probe map[string]any
	if len(fwd) > 0 && json.Unmarshal(fwd, &probe) == nil {
		normalizeEffortForPath(path, probe)
		if b, err := json.Marshal(probe); err == nil {
			fwd = b
		}
	}
	reqID := deriveRequestID(session, lastUserText(probe))

	tried := map[int]bool{}
	deadline := time.Now().Add(budget)
	var lastErr string
	wantCC := c.countryFor(model)
	for {
		lane := c.Lanes.PickCountry(tried, wantCC)
		if lane == nil {
			if time.Now().After(deadline) {
				return 502, fmt.Errorf("no lane available: %s", lastErr)
			}
			time.Sleep(500 * time.Millisecond)
			tried = map[int]bool{}
			continue
		}
		tried[lane.Index] = true
		resp, rerr := c.open(lane, method, url, session, reqID, fwd, in)
		if rerr != nil {
			lastErr = rerr.Error()
			c.Lanes.Release(lane)
			continue
		}
		st := resp.StatusCode
		hdr := resp.Header
		// The lane stays busy for the whole answer: it is held while the
		// stream runs, not only while the request is in flight.
		stop, cerr := consume(resp, st, hdr)
		c.Lanes.Release(lane)
		switch {
		case st == 429:
			ra := parseRetryAfter(hdr.Get("Retry-After"))
			until := c.Lanes.NoteLimited(lane, ra)
			c.Lanes.Rotate(lane)
			c.Lanes.Emit(lanes.Proof{T: time.Now(), Lane: lane.Index, Country: lane.Country, IP: lane.ExitIP, Model: model, Status: 429, Note: "exit limited until " + until.Format(time.RFC3339)})
			if stop {
				// the consumer already committed this answer downstream
				return st, cerr
			}
			continue
		case st == 502 || st == 503 || st == 504:
			if stop {
				return st, cerr
			}
			continue
		case st == 500:
			// Chat-path 500s are transient far-end failures; rotate once.
			if !stop && len(tried) < len(c.Lanes.Lanes()) {
				continue
			}
			return st, cerr
		default:
			// committed answer: score the exit and leave a proof row
			c.Lanes.NoteResult(lane.Country, st)
			c.Lanes.Emit(lanes.Proof{
				T: time.Now(), Lane: lane.Index, Country: lane.Country, IP: lane.ExitIP,
				Model: model, Status: st, Bytes: bodyBytes(resp),
			})
			return st, cerr
		}
	}
}

func bodyBytes(resp *http.Response) int64 {
	if resp == nil {
		return 0
	}
	return resp.ContentLength
}

// open sends one attempt on a lane and returns the live response; the caller
// owns resp.Body.
func (c *Client) open(lane *lanes.Lane, method, url, session, reqID string, body []byte, in http.Header) (*http.Response, error) {
	hc := c.clientFor(lane)
	var rdr io.Reader
	if body != nil {
		rdr = bytesReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	if in == nil {
		in = http.Header{}
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
		return nil, err
	}
	// Guard against a silently dead upstream: no progress for streamIdle and
	// the stream is considered broken.
	resp.Body = &idleBody{rc: resp.Body, idle: streamIdle}
	return resp, nil
}

// idleBody fails a read that makes no progress for d.
type idleBody struct {
	rc   io.ReadCloser
	idle time.Duration
	last time.Time
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.last.IsZero() {
		b.last = time.Now()
	}
	type res struct {
		n   int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		n, err := b.rc.Read(p)
		ch <- res{n, err}
	}()
	select {
	case r := <-ch:
		b.last = time.Now()
		return r.n, r.err
	case <-time.After(b.idle):
		b.rc.Close()
		return 0, fmt.Errorf("upstream idle for %s", b.idle)
	}
}

func (b *idleBody) Close() error { return b.rc.Close() }

func drain(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func retryable(status int) bool {
	switch status {
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
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

func jsonHeader() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return h
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func relay(w http.ResponseWriter, status int, hdr http.Header, body []byte) {
	for _, k := range []string{"Content-Type", "Retry-After"} {
		if v := hdr.Get(k); v != "" {
			if w != nil {
				w.Header().Set(k, v)
			}
		}
	}
	if w == nil {
		return
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
	if body == nil {
		return ""
	}
	if arr, ok := body["messages"].([]any); ok {
		for i := len(arr) - 1; i >= 0; i-- {
			mm, ok := arr[i].(map[string]any)
			if !ok || mm["role"] != "user" {
				continue
			}
			if s, ok := mm["content"].(string); ok && strings.TrimSpace(s) != "" {
				return tail(s)
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
					return tail(t)
				}
			}
		}
	}
	return ""
}

func tail(s string) string {
	t := strings.TrimSpace(s)
	if len(t) > 600 {
		t = t[len(t)-600:]
	}
	return t
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

func bodyModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Model
}

func bareID(m string) string {
	m = stripThinking(m)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m
}

type byteReader struct {
	b []byte
	i int
}

func bytesReader(b []byte) io.Reader { return &byteReader{b: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

var _ = bufio.ErrTooLong
