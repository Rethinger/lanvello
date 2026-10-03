// Package upstream forwards openai-compatible requests to the real gateway
// while pretending to be the opencode client (user-agent + version),
// rotating lanes on 429/502/503/504 like lingling.
package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"lanvello/internal/lanes"
)

type Route struct {
	URL            string
	Token          string
	OpencodeClient bool
}

type Client struct {
	GatewayURL string
	Token      string
	ClientVer  string
	Lanes      *lanes.Manager
	HTTP       *http.Client
	WaitBudget time.Duration
	Ups        []UpstreamSel
}

type UpstreamSel struct {
	Prefix  string
	BaseURL string
	Token   string
}

func NewClient(gatewayURL, token, clientVer string, m *lanes.Manager) *Client {
	return &Client{
		GatewayURL: strings.TrimRight(gatewayURL, "/"),
		Token:      token,
		ClientVer:  clientVer,
		Lanes:      m,
		HTTP:       &http.Client{Timeout: 120 * time.Second},
		WaitBudget: 90 * time.Second,
	}
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
	return &http.Client{Transport: transport, Timeout: 120 * time.Second}
}

// Target maps public model names to upstream paths.
// Gateway mode (opencode-like): <gateway>/ai/v1/proxy/openai/v1/chat/completions.
// Generic mode (any openai-compatible base): <baseURL>/chat/completions.
func (c *Client) route(model string) Route {
	best := -1
	for i, u := range c.Ups {
		if u.Prefix != "" && strings.HasPrefix(model, u.Prefix) && len(u.Prefix) > best {
			best = len(u.Prefix)
			_ = i
		}
	}
	for _, u := range c.Ups {
		if u.Prefix != "" && strings.HasPrefix(model, u.Prefix) && len(u.Prefix) == best && best >= 0 {
			base := strings.TrimRight(u.BaseURL, "/")
			if strings.HasSuffix(base, "/v1") {
				return Route{URL: base + "/chat/completions", Token: u.Token}
			}
			return Route{URL: base + "/v1/chat/completions", Token: u.Token}
		}
	}
	return Route{URL: c.GatewayURL + "/ai/v1/proxy/openai/v1/chat/completions", Token: c.Token, OpencodeClient: true}
}

func (c *Client) openAIURL() string { return c.GatewayURL + "/ai/v1/proxy/openai/v1/chat/completions" }

func (c *Client) Do(model string, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), 400)
		return
	}
	// model lives in the json body; query param is only a fallback.
	if m := bodyModel(body); m != "" {
		model = m
	}
	isStream := strings.Contains(string(body), `"stream":true`) || strings.Contains(string(body), `"stream": true`)
	_ = isStream

	tried := map[int]bool{}
	deadline := time.Now().Add(c.WaitBudget)
	var lastErr string
	for {
		lane := c.Lanes.Pick(tried)
		if lane == nil {
			if time.Now().After(deadline) {
				http.Error(w, "no lane available"+suffix(lastErr), 502)
				return
			}
			time.Sleep(500 * time.Millisecond)
			tried = map[int]bool{}
			continue
		}
		tried[lane.Index] = true
		rt := c.route(model)
		outBody := body
		if !rt.OpencodeClient {
			if stripped, ok := stripPrefix(model, c.Ups); ok {
				outBody = swapModel(body, model, stripped)
			}
		}
		status, hdr, respBody, rerr := c.roundTrip(lane, rt, outBody, r.Header)
		if rerr != nil {
			lastErr = rerr.Error()
			c.Lanes.Release(lane)
			// dial failure: try next lane immediately
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
			relay(w, status, hdr, respBody)
			c.Lanes.NoteResult(lane.Country, status)
			return
		default:
			relay(w, status, hdr, respBody)
			c.Lanes.NoteResult(lane.Country, status)
			c.Lanes.Emit(lanes.Proof{T: time.Now(), Lane: lane.Index, Country: lane.Country, IP: lane.ExitIP, Model: model, Status: status, Bytes: int64(len(respBody))})
			return
		}
	}
}

func suffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// bodyModel extracts the top-level "model" field without full decode.
func bodyModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Model
}

// stripPrefix returns the model id without the matched upstream prefix.
func stripPrefix(model string, ups []UpstreamSel) (string, bool) {
	best := -1
	for _, u := range ups {
		if u.Prefix != "" && strings.HasPrefix(model, u.Prefix) && len(u.Prefix) > best {
			best = len(u.Prefix)
		}
	}
	if best < 0 {
		return model, false
	}
	return strings.TrimPrefix(model[best:], "/"), true
}

// swapModel replaces the exact "model":"from" occurrence with "to".
func swapModel(body []byte, from, to string) []byte {
	old1 := `"model":"` + from + `"`
	new1 := `"model":"` + to + `"`
	if strings.Contains(string(body), old1) {
		return []byte(strings.Replace(string(body), old1, new1, 1))
	}
	old2 := `"model": "` + `"` + from + `"`
	if strings.Contains(string(body), `"model":`) {
		_ = old2
		// spaced variant: rebuild via minimal parse is overkill; string replace both spacings.
		s := strings.Replace(string(body), `"model" :"`+`"`+from+`"`, new1, 1)
		s = strings.Replace(s, `"model": "`+`"`+from+`"`, new1, 1)
		return []byte(s)
	}
	return body
}

func (c *Client) roundTrip(lane *lanes.Lane, rt Route, body []byte, in http.Header) (int, http.Header, []byte, error) {
	hc := c.clientFor(lane)
	req, err := http.NewRequest("POST", rt.URL, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if rt.Token != "" {
		req.Header.Set("Authorization", "Bearer "+rt.Token)
	}
	if rt.OpencodeClient {
		// pretend to be opencode cli, not a generic script
		ua := "opencode/" + c.ClientVer
		if c.ClientVer == "" {
			ua = "opencode/unknown"
		}
		req.Header.Set("User-Agent", ua)
		req.Header.Set("X-Client-Version", c.ClientVer)
		req.Header.Set("X-Opencode-Client", "lanvello")
	}
	if v := in.Get("X-Request-Id"); v != "" {
		req.Header.Set("X-Request-Id", v)
	}
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	_ = start
	return resp.StatusCode, resp.Header, b, nil
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

// DialCheck verifies a socks addr answers; used by demo/lanes status.
func DialCheck(socksAddr, target string, timeout time.Duration) error {
	if socksAddr == "" {
		u, err := url.Parse(target)
		if err != nil {
			return err
		}
		host := u.Host
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(host, "443")
		}
		c, err := net.DialTimeout("tcp", host, timeout)
		if err != nil {
			return err
		}
		return c.Close()
	}
	d, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		return err
	}
	u, err := url.Parse(target)
	if err != nil {
		return err
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "443")
	}
	c, err := d.Dial("tcp", host)
	if err != nil {
		return err
	}
	_ = c.Close()
	return nil
}

var _ = bufio.ErrTooLong
var _ = fmt.Sprint
