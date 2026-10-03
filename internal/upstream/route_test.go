package upstream

import (
	"testing"

	"lanvello/internal/lanes"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	lm := lanes.New(t.TempDir(), []string{"de"}, nil, nil, true, 1)
	c := NewClient("https://gw.example", "tok", "v1", lm)
	c.Ups = append(c.Ups, UpstreamSel{Prefix: "ikhdev/", BaseURL: "https://api.ikhdev.xyz/v1", Token: "k"})
	return c
}

func TestRouteGeneric(t *testing.T) {
	c := testClient(t)
	rt := c.route("ikhdev/free-big-pickle")
	if rt.URL != "https://api.ikhdev.xyz/v1/chat/completions" {
		t.Fatalf("url=%s", rt.URL)
	}
	if rt.OpencodeClient {
		t.Fatal("should be generic")
	}
}

func TestRouteGatewayFallback(t *testing.T) {
	c := testClient(t)
	rt := c.route("opencode/muse-spark-1.3-contributor-free")
	if !rt.OpencodeClient {
		t.Fatal("should be gateway")
	}
}

func TestStripAndSwap(t *testing.T) {
	c := testClient(t)
	s, ok := stripPrefix("ikhdev/free-big-pickle", c.Ups)
	if !ok || s != "free-big-pickle" {
		t.Fatalf("strip=%q ok=%v", s, ok)
	}
	got := string(swapModel([]byte(`{"model":"ikhdev/free-big-pickle","x":1}`), "ikhdev/free-big-pickle", "free-big-pickle"))
	if got != `{"model":"free-big-pickle","x":1}` {
		t.Fatalf("got %s", got)
	}
}
