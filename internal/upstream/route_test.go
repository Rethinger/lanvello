package upstream

import (
	"regexp"
	"testing"

	"lanvello/internal/lanes"
)

var sesRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var msgRe = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func TestEndpoints(t *testing.T) {
	if endpointFor("muse-spark-1.3-contributor-free") != "/zen/v1/responses" {
		t.Fatal("responses endpoint")
	}
	if endpointFor("union-alpha") != "/zen/v1/messages" {
		t.Fatal("messages endpoint")
	}
	if endpointFor("longcat-2.5-preview-free") != "/zen/v1/chat/completions" {
		t.Fatal("chat endpoint")
	}
}

func TestSessionIDs(t *testing.T) {
	if !sesRe.MatchString(NewSessionID()) {
		t.Fatal("session shape")
	}
	if !msgRe.MatchString(NewRequestID()) {
		t.Fatal("request shape")
	}
	a, b := StableSession("k1"), StableSession("k1")
	if a != b || !sesRe.MatchString(a) {
		t.Fatalf("stable session: %q %q", a, b)
	}
	if StableSession("k1") == StableSession("k2") {
		t.Fatal("sessions must differ per identity")
	}
}

func TestFingerprintToolsChat(t *testing.T) {
	body := map[string]any{}
	FingerprintTools(body, false)
	tools, _ := body["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("want 4 tools, got %d", len(tools))
	}
	if tc, _ := body["tool_choice"].(string); tc != "none" {
		t.Fatalf("tool_choice=%v", tc)
	}
}

func TestFingerprintToolsNoDup(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "bash"}},
	}}
	FingerprintTools(body, false)
	tools, _ := body["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("want 4 tools, got %d", len(tools))
	}
}

func TestValidUA(t *testing.T) {
	if !validOpencodeUA("opencode/1.18.31") {
		t.Fatal("should accept")
	}
	if validOpencodeUA("curl/8.0") {
		t.Fatal("should reject")
	}
	if validOpencodeUA("opencode/1.16.0") {
		t.Fatal("should reject old")
	}
}

func TestLanesPick(t *testing.T) {
	m := lanes.New(t.TempDir(), []string{"de"}, nil, nil, true, 1)
	c := NewClient(m)
	if c.BaseURL != ZenBase {
		t.Fatal("base")
	}
	if c.Lanes.Pick(nil) == nil {
		t.Fatal("no lane")
	}
}
