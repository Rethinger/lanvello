package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lanvello/internal/keys"
	"lanvello/internal/lanes"
	"lanvello/internal/server"
	"lanvello/internal/upstream"
)

func TestModelsOpenWhenNoKeys(t *testing.T) {
	dir := t.TempDir()
	ks, _ := keys.Open(dir)
	lm := lanes.New(dir, []string{"de"}, nil, nil, true, 1)
	up := upstream.NewClient("http://127.0.0.1:9", "", "test", lm)
	s := server.New(ks, lm, up)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Data) == 0 {
		t.Fatal("empty models")
	}
}

func TestModelsRequiresKeyWhenKeysExist(t *testing.T) {
	dir := t.TempDir()
	ks, _ := keys.Open(dir)
	if _, _, err := ks.Add("h"); err != nil {
		t.Fatal(err)
	}
	lm := lanes.New(dir, []string{"de"}, nil, nil, true, 1)
	up := upstream.NewClient("http://127.0.0.1:9", "", "test", lm)
	s := server.New(ks, lm, up)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rec.Code)
	}
}
