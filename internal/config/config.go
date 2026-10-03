// Package config holds lanvello configuration: env + file, stdlib only.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	Listen      string            `json:"listen"`
	Lanes       int               `json:"lanes"`
	DataDir     string            `json:"dataDir"`
	BaseURL     string            `json:"baseURL"`
	ClientVer   string            `json:"clientVersion"`
	Socks       []string          `json:"socks"`
	Countries   []string          `json:"countries"`
	Fallback    []string          `json:"fallbackCountries"`
	ModelLanes  map[string]string `json:"modelLanes"`
	WaitBudgetS float64           `json:"waitBudgetS"`
	// CatalogBudgetS bounds the lane wait for /v1/models, which must stay
	// snappy even when every lane is busy with a long answer.
	CatalogBudgetS float64 `json:"catalogBudgetS"`
}

func DefaultDataDir() string {
	if v := os.Getenv("LANVELLO_DATA_DIR"); v != "" {
		return v
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "lanvello")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "lanvello")
}

func Defaults() Config {
	return Config{
		Listen:         "127.0.0.1:11434",
		Lanes:          5,
		DataDir:        DefaultDataDir(),
		BaseURL:        envOr("LANVELLO_BASE_URL", "https://opencode.ai"),
		ClientVer:      envOr("LANVELLO_CLIENT_VERSION", "1.18.31"),
		Countries:      []string{"us", "de", "nl", "fr", "ro", "gb", "ca", "se", "pl", "ch"},
		WaitBudgetS:    90,
		CatalogBudgetS: 20,
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// LoadFile merges json file over defaults; missing file is not an error.
func LoadFile(path string, base Config) (Config, error) {
	if path == "" {
		return base, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return base, nil
		}
		return base, err
	}
	var f Config
	if err := json.Unmarshal(b, &f); err != nil {
		return base, err
	}
	if f.Listen != "" {
		base.Listen = f.Listen
	}
	if f.Lanes > 0 {
		base.Lanes = f.Lanes
	}
	if f.DataDir != "" {
		base.DataDir = f.DataDir
	}
	if f.BaseURL != "" {
		base.BaseURL = f.BaseURL
	}
	if f.ClientVer != "" {
		base.ClientVer = f.ClientVer
	}
	if len(f.Socks) > 0 {
		base.Socks = f.Socks
	}
	if len(f.Countries) > 0 {
		base.Countries = f.Countries
	}
	if len(f.Fallback) > 0 {
		base.Fallback = f.Fallback
	}
	if f.WaitBudgetS > 0 {
		base.WaitBudgetS = f.WaitBudgetS
	}
	if f.CatalogBudgetS > 0 {
		base.CatalogBudgetS = f.CatalogBudgetS
	}
	if len(f.ModelLanes) > 0 {
		if base.ModelLanes == nil {
			base.ModelLanes = map[string]string{}
		}
		for k, v := range f.ModelLanes {
			base.ModelLanes[k] = v
		}
	}
	return base, nil
}
