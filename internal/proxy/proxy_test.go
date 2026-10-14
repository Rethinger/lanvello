package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"lanvello/internal/lanes"
)

// A throttled exit must cost the client nothing: the request is replayed on the
// next lane and the client sees one answer. Two lanes, first one spent.
func TestThrottledExitIsRotatedAndReplayed(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"exit spent"}`)
			return
		}
		fmt.Fprintf(w, `{"hit":%d}`, n)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	ca, err := OpenCA(dir)
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	// noTor lanes have no socks address: the dial goes direct, which is what a
	// test upstream on loopback needs. Picking and rotation stay the real code.
	m := lanes.New(dir, []string{"de"}, nil, nil, true, 2)
	p := New(m, ca)
	p.Attempts = 3

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go p.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nAccept: */*\r\n\r\n", upstream.URL, upstream.Listener.Addr().String())
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client saw %d, want 200: the throttle was not rotated away", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (429 then the replay)", got)
	}
	parked := 0
	for _, l := range m.Lanes() {
		if l.LimitedTill.After(time.Now()) {
			parked++
		}
	}
	if parked != 1 {
		t.Fatalf("lanes parked for a spent exit = %d, want 1", parked)
	}
}

// A 403 that says "this exit is not OpenCode" is a spent exit like any other:
// the request is replayed on the next lane and the client never sees it. The
// free tier hands that 403 to exits it has had enough of, so rotating on it is
// what keeps a fleet of them usable.
func TestFreeTierRefusalIsRotatedAndReplayed(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode. Download it at https://opencode.ai and keep building."}}`)
			return
		}
		fmt.Fprintf(w, `{"hit":%d}`, n)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	ca, err := OpenCA(dir)
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	m := lanes.New(dir, []string{"de"}, nil, nil, true, 2)
	p := New(m, ca)
	p.Attempts = 3

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go p.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nAccept: */*\r\n\r\n", upstream.URL, upstream.Listener.Addr().String())
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client saw %d, want 200: the refusal was not rotated away", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != `{"hit":2}` {
		t.Fatalf("client body = %q, want the replayed answer", body)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (refusal then the replay)", got)
	}
	parked := 0
	for _, l := range m.Lanes() {
		if l.LimitedTill.After(time.Now()) {
			parked++
		}
	}
	if parked != 1 {
		t.Fatalf("lanes parked for a refused exit = %d, want 1", parked)
	}
}

// Only the free tier's own refusal earns a rotation: every other 403 is an
// answer the client asked for, and its body must reach the client untouched —
// byte for byte, still compressed, because the client decodes it.
func TestFreeTierRefusedOnlyOnTheRefusal(t *testing.T) {
	refusal := `{"error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode. Download it at https://opencode.ai and keep building."}}`
	cases := []struct {
		name   string
		status int
		enc    string
		body   string
		want   bool
	}{
		{name: "the refusal as it arrives", status: http.StatusForbidden, enc: "br", body: refusal, want: true},
		{name: "the refusal, gzipped", status: http.StatusForbidden, enc: "gzip", body: refusal, want: true},
		{name: "the refusal, plain", status: http.StatusForbidden, body: refusal, want: true},
		{name: "any other 403", status: http.StatusForbidden, body: `{"error":"forbidden"}`, want: false},
		{name: "an empty 403", status: http.StatusForbidden, body: "", want: false},
		{name: "a 200 that mentions the tier", status: http.StatusOK, body: "FreeTierError", want: false},
	}
	for _, c := range cases {
		var wire []byte
		switch c.enc {
		case "br":
			var buf bytes.Buffer
			bw := brotli.NewWriter(&buf)
			if _, err := bw.Write([]byte(c.body)); err != nil {
				t.Fatalf("%s: brotli: %v", c.name, err)
			}
			if err := bw.Close(); err != nil {
				t.Fatalf("%s: brotli close: %v", c.name, err)
			}
			wire = buf.Bytes()
		case "gzip":
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			if _, err := zw.Write([]byte(c.body)); err != nil {
				t.Fatalf("%s: gzip: %v", c.name, err)
			}
			if err := zw.Close(); err != nil {
				t.Fatalf("%s: gzip close: %v", c.name, err)
			}
			wire = buf.Bytes()
		default:
			wire = []byte(c.body)
		}
		hdr := http.Header{}
		if c.enc != "" {
			hdr.Set("Content-Encoding", c.enc)
		}
		resp := &http.Response{StatusCode: c.status, Header: hdr, Body: io.NopCloser(bytes.NewReader(wire))}
		if got := freeTierRefused(resp); got != c.want {
			t.Errorf("%s: freeTierRefused = %v, want %v", c.name, got, c.want)
		}
		kept, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s: body unreadable after the check: %v", c.name, err)
		}
		if !bytes.Equal(kept, wire) {
			t.Errorf("%s: body changed: %d bytes after the check, %d before", c.name, len(kept), len(wire))
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("%s: close: %v", c.name, err)
		}
	}
}

func TestRetryAfterIsHonoredInFull(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"seconds", "7200", 2 * time.Hour},
		{"the 146 minute wall opencode reports", "8760", 146 * time.Minute},
		{"http date", time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second},
		{"missing falls back", "", 10 * time.Minute},
		{"garbage falls back", "soon", 10 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if c.header != "" {
				resp.Header.Set("Retry-After", c.header)
			}
			got := retryAfter(resp)
			if c.name == "http date" {
				// parsed from a round-tripped second boundary
				if got > c.want || got < c.want-time.Second {
					t.Fatalf("retryAfter(%q) = %s, want ~%s", c.header, got, c.want)
				}
				return
			}
			if got != c.want {
				t.Fatalf("retryAfter(%q) = %s, want %s", c.header, got, c.want)
			}
		})
	}
}

func TestSplitHostPortDefaults(t *testing.T) {
	if h, p := splitHostPort("example.com", defaultPort("https")); h != "example.com" || p != "443" {
		t.Fatalf("https default = %s/%s", h, p)
	}
	if h, p := splitHostPort("example.com:8443", defaultPort("https")); h != "example.com" || p != "8443" {
		t.Fatalf("explicit port = %s/%s", h, p)
	}
	if _, p := splitHostPort("example.com", defaultPort("http")); p != "80" {
		t.Fatalf("http default port = %s", p)
	}
}

func TestLoopbackIsNotLaned(t *testing.T) {
	if !isLoopback("127.0.0.1") || !isLoopback("localhost") || !isLoopback("::1") {
		t.Fatal("loopback must bypass the lanes: the client's own server is not lane material")
	}
	if isLoopback("opencode.ai") {
		t.Fatal("opencode.ai must go through a lane")
	}
}

func TestChunkedAnswerReachesTheClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: one\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: two\n\n")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	ca, _ := OpenCA(dir)
	m := lanes.New(dir, []string{"de"}, nil, nil, true, 1)
	p := New(m, ca)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go p.Serve(ln)

	conn, _ := net.Dial("tcp", ln.Addr().String())
	defer conn.Close()
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", upstream.URL, upstream.Listener.Addr().String())
	conn.Write([]byte(req))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream read: %v", err)
	}
	// The client must get the events framed so it can parse them as they
	// arrive: no length was known when the headers went out.
	if got := string(body); got != "data: one\n\ndata: two\n\n" {
		t.Fatalf("streamed body = %q", got)
	}
	if len(resp.TransferEncoding) == 0 && resp.ContentLength < 0 {
		t.Fatal("framing lost: neither chunked nor a length was announced")
	}
}
