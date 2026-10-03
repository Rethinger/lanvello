package upstream

import (
	"regexp"
	"testing"

	"lanvello/internal/lanes"
)

var testSesRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var testMsgRe = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

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
	if !testSesRe.MatchString(NewSessionID()) {
		t.Fatal("session shape")
	}
	if !testMsgRe.MatchString(NewRequestID()) {
		t.Fatal("request shape")
	}
	a, b := StableSession("k1"), StableSession("k1")
	if a != b || !testSesRe.MatchString(a) {
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

func TestValidSession(t *testing.T) {
	if !ValidSession(NewSessionID()) {
		t.Fatal("fresh session invalid")
	}
	if ValidSession("nope") {
		t.Fatal("bad session valid")
	}
}

func TestDeriveRequestID(t *testing.T) {
	a := deriveRequestID("ses_abc", "hello")
	b := deriveRequestID("ses_abc", "hello")
	if a != b || !msgRe.MatchString(a) {
		t.Fatalf("unstable: %q %q", a, b)
	}
	if deriveRequestID("ses_abc", "hello") == deriveRequestID("ses_abc", "world") {
		t.Fatal("not text-bound")
	}
}

func TestSanitizeStripsReasoning(t *testing.T) {
	body := map[string]any{"input": []any{
		map[string]any{"type": "reasoning", "encrypted_content": "x"},
		map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		map[string]any{"type": "function_call_output", "call_id": "c", "output": map[string]any{"a": 1}},
	}}
	sanitizeResponsesInput(body)
	arr, _ := body["input"].([]any)
	if len(arr) != 2 {
		t.Fatalf("len=%d", len(arr))
	}
	out := arr[1].(map[string]any)
	if out["output"] != `{"a":1}` {
		t.Fatalf("output=%v", out["output"])
	}
}

func TestSanitizeStringInput(t *testing.T) {
	body := map[string]any{"input": "hi"}
	sanitizeResponsesInput(body)
	arr, _ := body["input"].([]any)
	if len(arr) != 1 {
		t.Fatal("not normalized")
	}
}

func TestNormalizeReasoning(t *testing.T) {
	body := map[string]any{"reasoning_effort": "High"}
	normalizeReasoning(body)
	r, _ := body["reasoning"].(map[string]any)
	if r["effort"] != "high" || r["summary"] != "auto" {
		t.Fatalf("%v", r)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatal("not deleted")
	}
}

func TestFamilyMatch(t *testing.T) {
	for _, m := range []string{"muse-spark-1.3", "opencode/muse_spark-x", "Muse-Spark-1.3-contributor-free(max)"} {
		if !IsResponsesModel(m) {
			t.Fatalf("should match: %s", m)
		}
	}
	if IsResponsesModel("longcat-2.5-preview-free") {
		t.Fatal("false positive")
	}
	if !IsMessagesModel("opencode/union-alpha") {
		t.Fatal("union-alpha with prefix")
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
