// Package config holds lanvello configuration: env + file, stdlib only.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Upstream struct {
	Name      string `json:"name"`
	BaseURL   string `json:"baseURL"`
	Token     string `json:"token"`
	ModelPref string `json:"modelPrefix"`
}
type Config struct {
	Listen      string     `json:"listen"`
	Lanes       int        `json:"lanes"`
	NoTor       bool       `json:"noTor"`
	DataDir     string     `json:"dataDir"`
	GatewayURL  string     `json:"gatewayURL"`
	GatewayTok  string     `json:"gatewayToken"`
	ClientVer   string     `json:"clientVersion"`
	Socks       []string   `json:"socks"`
	Countries   []string   `json:"countries"`
	Fallback    []string   `json:"fallbackCountries"`
	Upstreams   []Upstream `json:"upstreams"`
	RequireKey  bool       `json:"requireKey"`
	WaitBudgetS float64    `json:"waitBudgetS"`
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
		Listen:      "127.0.0.1:11434",
		Lanes:       5,
		DataDir:     DefaultDataDir(),
		GatewayURL:  envOr("LANVELLO_GATEWAY_URL", "https://api.opencode.ai"),
		GatewayTok:  os.Getenv("LANVELLO_GATEWAY_TOKEN"),
		ClientVer:   envOr("LANVELLO_CLIENT_VERSION", "v0.0.0-dev-20455"),
		Countries:   []string{"us", "de", "nl", "fr", "ro", "gb", "ca", "se", "pl", "ch"},
		WaitBudgetS: 90,
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
	// merge non-zero values
	if f.Listen != "" {
		base.Listen = f.Listen
	}
	if f.Lanes > 0 {
		base.Lanes = f.Lanes
	}
	if f.DataDir != "" {
		base.DataDir = f.DataDir
	}
	if f.GatewayURL != "" {
		base.GatewayURL = f.GatewayURL
	}
	if f.GatewayTok != "" {
		base.GatewayTok = f.GatewayTok
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
	if len(f.Upstreams) > 0 {
		base.Upstreams = f.Upstreams
	}
	if f.RequireKey {
		base.RequireKey = true
	}
	if f.WaitBudgetS > 0 {
		base.WaitBudgetS = f.WaitBudgetS
	}
	if f.NoTor {
		base.NoTor = true
	}
	return base, nil
}
