package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsHaveTorOnlyBudgets(t *testing.T) {
	d := Defaults()
	if d.WaitBudgetS <= 0 {
		t.Fatalf("waitBudgetS=%v", d.WaitBudgetS)
	}
	if d.CatalogBudgetS <= 0 || d.CatalogBudgetS >= d.WaitBudgetS {
		t.Fatalf("catalog budget %v must be positive and below the answer budget %v",
			d.CatalogBudgetS, d.WaitBudgetS)
	}
	if d.Listen == "" || len(d.Countries) == 0 || d.Lanes <= 0 {
		t.Fatalf("defaults incomplete: %+v", d)
	}
}

func TestLoadFileMergesOverDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	body := map[string]any{
		"listen":            "127.0.0.1:9999",
		"lanes":             2,
		"catalogBudgetS":    7,
		"modelLanes":        map[string]any{"fledge": "de"},
		"fallbackCountries": []string{"se"},
	}
	b, _ := json.Marshal(body)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(p, Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9999" || cfg.Lanes != 2 {
		t.Fatalf("%+v", cfg)
	}
	if cfg.CatalogBudgetS != 7 {
		t.Fatalf("catalogBudgetS=%v", cfg.CatalogBudgetS)
	}
	if cfg.ModelLanes["fledge"] != "de" {
		t.Fatalf("modelLanes=%v", cfg.ModelLanes)
	}
	// untouched keys keep their defaults
	if len(cfg.Countries) == 0 || cfg.WaitBudgetS <= 0 {
		t.Fatalf("defaults lost: %+v", cfg)
	}
}

func TestLoadFileMissingIsNotAnError(t *testing.T) {
	cfg, err := LoadFile(filepath.Join(t.TempDir(), "nope.json"), Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != Defaults().Listen {
		t.Fatalf("missing file must keep defaults: %+v", cfg)
	}
}

func TestLoadFileRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(p, Defaults()); err == nil {
		t.Fatal("garbage config must be an error, not silently defaulted")
	}
}

func TestLoadFileRequireKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"requireKey":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(p, Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RequireKey {
		t.Fatalf("requireKey lost: %+v", cfg)
	}
}
