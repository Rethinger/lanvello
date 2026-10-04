// Package caps attaches model capability metadata — reasoning effort
// levels, context and output limits, modalities — to the live model
// list. Ids come from the gateway's own catalog fetch; capabilities
// come from opencode's catalog mirror, the same file the opencode
// client downloads, pulled through a tor lane and cached on disk.
// A stale or unreachable mirror never breaks the gateway: the
// embedded table for the known free models is the last resort.
package caps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"lanvello/internal/lanes"
	"lanvello/internal/upstream"
)

// CatalogURL is opencode's mirror of the models.dev catalog.
const CatalogURL = "https://models.opencode.ai/api.json"

// ttl bounds how long a cached catalog is trusted. The free roster
// changes rarely; a long ttl keeps the multi-megabyte mirror fetch
// off the lanes.
const ttl = 6 * time.Hour

// fetchBudget is generous on purpose: the mirror is ~5 MB and the
// lanes are tor exits.
const fetchBudget = 3 * time.Minute

type Option struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

type Limit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type Cost struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

type Modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// Entry is the capability record for one model, in the shape the
// mirror serves it.
type Entry struct {
	Name             string     `json:"name"`
	Reasoning        bool       `json:"reasoning"`
	ReasoningOptions []Option   `json:"reasoning_options,omitempty"`
	ToolCall         bool       `json:"tool_call"`
	Attachment       bool       `json:"attachment"`
	Limit            Limit      `json:"limit"`
	Cost             Cost       `json:"cost"`
	Modalities       Modalities `json:"modalities"`
}

// Efforts returns the selectable reasoning effort levels, if any.
// A reasoning model without effort options thinks, but the caller
// gets no levels to offer.
func (e Entry) Efforts() []string {
	for _, o := range e.ReasoningOptions {
		if o.Type == "effort" && len(o.Values) > 0 {
			return o.Values
		}
	}
	return nil
}

type diskCache struct {
	At     int64            `json:"at"`
	Models map[string]Entry `json:"models"`
}

type Store struct {
	mu       sync.RWMutex
	byID     map[string]Entry
	at       time.Time
	dir      string
	up       *upstream.Client
	lm       *lanes.Manager
	fetching atomic.Bool
}

// Open loads the disk cache when present and falls back to the
// embedded table. Open never dials: refresh happens in the
// background via MaybeRefresh.
func Open(dataDir string, up *upstream.Client, lm *lanes.Manager) *Store {
	s := &Store{byID: embedded(), dir: dataDir, up: up, lm: lm}
	if dataDir != "" {
		if c, ok := s.loadDisk(); ok {
			s.byID = c
		}
	}
	return s
}

// Entries returns the current capability table keyed by bare model id.
func (s *Store) Entries() map[string]Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byID
}

// Get returns the capabilities of one bare model id.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[id]
	return e, ok
}

// MaybeRefresh swaps in a fresh catalog when the cached one is older
// than ttl. The fetch runs on a goroutine through a tor lane, so the
// caller never waits and never dials. Direct lanes (tests) are
// skipped: egress stays tor-only.
func (s *Store) MaybeRefresh() {
	if s.dir == "" || s.up == nil || s.lm == nil {
		return
	}
	if s.lm.Direct() {
		return
	}
	s.mu.RLock()
	stale := time.Since(s.at) > ttl
	s.mu.RUnlock()
	if !stale || !s.fetching.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.fetching.Store(false)
		s.refresh()
	}()
}

func (s *Store) refresh() {
	status, body := s.up.GetURL(CatalogURL, fetchBudget)
	if status != 200 || len(body) == 0 {
		return
	}
	var v struct {
		Opencode struct {
			Models map[string]Entry `json:"models"`
		} `json:"opencode"`
	}
	if err := json.Unmarshal(body, &v); err != nil || len(v.Opencode.Models) == 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	s.byID = v.Opencode.Models
	s.at = now
	s.mu.Unlock()
	s.writeDisk(now, v.Opencode.Models)
}

func (s *Store) loadDisk() (map[string]Entry, bool) {
	b, err := os.ReadFile(filepath.Join(s.dir, "caps.json"))
	if err != nil {
		return nil, false
	}
	var c diskCache
	if err := json.Unmarshal(b, &c); err != nil || len(c.Models) == 0 {
		return nil, false
	}
	s.at = time.Unix(c.At, 0)
	return c.Models, true
}

func (s *Store) writeDisk(at time.Time, models map[string]Entry) {
	b, err := json.Marshal(diskCache{At: at.Unix(), Models: models})
	if err != nil {
		return
	}
	tmp := filepath.Join(s.dir, "caps.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(s.dir, "caps.json"))
}
