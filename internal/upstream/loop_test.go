package upstream

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lanvello/internal/lanes"
)

func testClient(t *testing.T, lanesN int, h http.HandlerFunc) (*Client, *lanes.Manager) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	m := lanes.New(t.TempDir(), []string{"us", "de"}, nil, nil, true, lanesN)
	c := NewClient(m)
	c.BaseURL = srv.URL
	c.WaitBudget = 3 * time.Second
	return c, m
}

func sseBody(text string) string {
	return "data: {\"id\":\"c1\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + text + "\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"created\":1,\"model\":\"m\",\"choices\":[{\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}]}\n\n" +
		"data: [DONE]\n\n"
}

func TestGetJSONUsesLaneAndFilters(t *testing.T) {
	c, m := testClient(t, 1, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("catalog must be a GET, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer "+PublicToken {
			t.Errorf("missing free-tier auth: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"data":[{"id":"fledge-alpha-free"}]}`)
	})
	status, body := c.GetJSON("/zen/v1/models")
	if status != 200 || !strings.Contains(string(body), "fledge-alpha-free") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(m.Lanes()) != 1 {
		t.Fatal("lane count")
	}
}

func TestGetJSONRotatesPast429(t *testing.T) {
	var n int64
	c, _ := testClient(t, 2, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&n, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "limited", 429)
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	})
	status, _ := c.GetJSON("/zen/v1/models")
	if status != 200 {
		t.Fatalf("status=%d, catalog must rotate like any other call", status)
	}
	if atomic.LoadInt64(&n) < 2 {
		t.Fatal("expected a retry")
	}
}

func TestStreamEventsDeliversBeforeStreamEnds(t *testing.T) {
	hold := make(chan struct{})
	c, _ := testClient(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"early"}}]}`+"\n\n")
		fl.Flush()
		<-hold
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, err := c.StreamEvents("fledge-alpha-free", "k", []byte(`{"messages":[]}`), nil, w,
			func(payload string, ev map[string]any) ([]string, bool) {
				if ev == nil {
					return nil, true
				}
				return []string{"data: " + payload}, false
			})
		if err != nil {
			t.Errorf("stream: %v", err)
		}
	}))
	defer hs.Close()
	resp, err := http.Get(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	got := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if strings.Contains(sc.Text(), "early") {
				got <- sc.Text()
				return
			}
		}
		close(got)
	}()
	select {
	case l := <-got:
		if !strings.Contains(l, "early") {
			t.Fatalf("bad line %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first event must arrive before the stream ends")
	}
	close(hold)
}

func TestStreamRotatesOn429BeforeFirstByte(t *testing.T) {
	var n int64
	c, _ := testClient(t, 2, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&n, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "limited", 429)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"second"}}]}`+"\n\n")
	})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body strings.Builder
		_, err := c.StreamEvents("fledge-alpha-free", "k", []byte(`{"messages":[]}`), nil, w,
			func(payload string, ev map[string]any) ([]string, bool) {
				if ev == nil {
					return nil, true
				}
				return []string{"data: " + payload}, false
			})
		body.WriteString("")
		if err != nil {
			t.Errorf("stream: %v", err)
		}
	}))
	defer hs.Close()
	resp, err := http.Get(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 4096)
	nr, _ := resp.Body.Read(b)
	if !strings.Contains(string(b[:nr]), "second") {
		t.Fatalf("must rotate before the first byte: %q", string(b[:nr]))
	}
}

func TestEffortNormalizationPerEndpoint(t *testing.T) {
	chat := map[string]any{"reasoning_effort": "HIGH"}
	normalizeEffortForPath("/zen/v1/chat/completions", chat)
	if chat["reasoning_effort"] != "HIGH" {
		t.Fatalf("chat endpoint keeps reasoning_effort: %v", chat)
	}
	if _, bad := chat["reasoning"]; bad {
		t.Fatalf("chat endpoint must not receive reasoning{}: %v", chat["reasoning"])
	}

	resp := map[string]any{"reasoning": map[string]any{"effort": "medium"}}
	normalizeEffortForPath("/zen/v1/responses", resp)
	r, _ := resp["reasoning"].(map[string]any)
	if r == nil || r["effort"] != "medium" || r["summary"] != "auto" {
		t.Fatalf("responses endpoint wants reasoning{effort,summary}: %v", resp["reasoning"])
	}

	flat := map[string]any{"reasoning": map[string]any{"effort": "low"}}
	normalizeEffortForPath("/zen/v1/chat/completions", flat)
	if flat["reasoning_effort"] != "low" {
		t.Fatalf("a reasoning object on chat must flatten to reasoning_effort: %v", flat)
	}
}

func TestUnreachableUpstreamGivesUpInsteadOfHanging(t *testing.T) {
	m := lanes.New(t.TempDir(), []string{"us"}, nil, nil, true, 1)
	c := NewClient(m)
	c.BaseURL = "http://127.0.0.1:1" // nothing listens here
	c.WaitBudget = 300 * time.Millisecond
	c.CatalogBudget = 300 * time.Millisecond

	if status, _ := c.GetJSON("/zen/v1/models"); status == 200 {
		t.Fatal("a dead endpoint must not answer 200")
	}
	done := make(chan struct{})
	go func() {
		c.Raw("fledge-alpha-free", "k", []byte(`{"messages":[]}`), nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Raw hung with no reachable lane")
	}
}

func TestIdleBodyFailsStalledRead(t *testing.T) {
	orig := streamIdle
	streamIdle = 150 * time.Millisecond
	defer func() { streamIdle = orig }()

	pr, pw := net.Pipe()
	defer pr.Close()
	resp := &http.Response{Body: &idleBody{rc: pr, idle: streamIdle}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(pw, "first")
	}()
	buf := make([]byte, 16)
	n, err := resp.Body.Read(buf)
	if n != 5 || err != nil {
		t.Fatalf("first read: n=%d err=%v", n, err)
	}
	start := time.Now()
	if _, err := resp.Body.Read(buf); err == nil {
		t.Fatal("a stalled stream must fail, not hang forever")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("idle guard too slow: %s", time.Since(start))
	}
}

// A stream that never starts (no lane up, upstream unreachable) must answer
// with a real status. It used to return without writing anything, which
// net/http turned into an empty 200 that hid the failure.
func TestStreamAnswers502WhenNothingStarts(t *testing.T) {
	m := lanes.New(t.TempDir(), []string{"us"}, nil, nil, true, 1)
	c := NewClient(m)
	c.BaseURL = "http://127.0.0.1:1" // nothing listens here
	c.WaitBudget = 300 * time.Millisecond
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := c.StreamEvents("fledge-alpha-free", "k", []byte(`{"messages":[]}`), nil, w,
			func(payload string, ev map[string]any) ([]string, bool) { return nil, false }); err == nil {
			t.Errorf("a dead upstream must surface an error")
		}
	}))
	defer hs.Close()
	resp, err := http.Get(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 (an empty 200 hides the failure)", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "error") {
		t.Fatalf("body=%q", b)
	}
}

// A far-end that is plainly down (503 every time) must not spin until the
// wait budget: after a couple of full passes the real answer goes back.
func TestRawBailsOnPersistent503(t *testing.T) {
	var hits int32
	c, _ := testClient(t, 3, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":{"type":"server_error","message":"Endpoint is unavailable."}}`)
	})
	c.WaitBudget = 60 * time.Second // would spin for a minute without the bound
	start := time.Now()
	status, _, body := c.Raw("fledge-alpha-free", "k", []byte(`{"messages":[]}`), nil)
	if status != 503 {
		t.Fatalf("status=%d, want 503", status)
	}
	if !strings.Contains(string(body), "Endpoint is unavailable") {
		t.Fatalf("real upstream error lost: %q", string(body))
	}
	if n := atomic.LoadInt32(&hits); n > 8 {
		t.Fatalf("retried %d times, the bound is not working", n)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("spun %s instead of bailing", d)
	}
}

// Same for the streaming path: the drained upstream answer must be relayed
// to the client instead of an empty 200.
func TestStreamRelaysPersistent503(t *testing.T) {
	c, _ := testClient(t, 3, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":{"type":"server_error","message":"Endpoint is unavailable."}}`)
	})
	c.WaitBudget = 60 * time.Second
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = c.StreamEvents("fledge-alpha-free", "k", []byte(`{"messages":[]}`), nil, w,
			func(payload string, ev map[string]any) ([]string, bool) { return nil, false })
	}))
	defer hs.Close()
	resp, err := http.Get(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "Endpoint is unavailable") {
		t.Fatalf("body=%q", b)
	}
}
