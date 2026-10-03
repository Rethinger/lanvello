// Package keys stores local bearer keys (sk-...) in a json file.
// No cgo, no sqlite: file is enough for a localhost gateway.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Entry struct {
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"createdAt"`
	LastUsed  int64  `json:"lastUsed"`
}

type Store struct {
	path string
	mu   sync.Mutex
	en   []Entry
}

func Path(dataDir string) string { return filepath.Join(dataDir, "keys.json") }

func Open(dataDir string) (*Store, error) {
	s := &Store{path: Path(dataDir)}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.en); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) save() error {
	tmp := s.path + ".tmp"
	b, err := json.MarshalIndent(s.en, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func hashOf(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Add creates a new key and returns the plaintext secret (shown once).
func (s *Store) Add(name string) (string, Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", Entry{}, err
	}
	secret := "sk-lanv-" + hex.EncodeToString(raw[:])
	e := Entry{
		Name:      name,
		Prefix:    secret[:12] + "...",
		Hash:      hashOf(secret),
		CreatedAt: time.Now().Unix(),
	}
	// replace same name
	out := s.en[:0]
	for _, x := range s.en {
		if x.Name != name {
			out = append(out, x)
		}
	}
	s.en = append(out, e)
	if err := s.save(); err != nil {
		return "", Entry{}, err
	}
	return secret, e, nil
}

func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]Entry, len(s.en))
	copy(cp, s.en)
	return cp
}

func (s *Store) Revoke(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	out := s.en[:0]
	for _, x := range s.en {
		if x.Name == name {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		return false
	}
	s.en = out
	_ = s.save()
	return true
}

// Verify checks bearer secret; empty store + allowEmpty means open mode.
func (s *Store) Verify(secret string, allowEmpty bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if secret == "" {
		return false
	}
	h := hashOf(secret)
	for i, x := range s.en {
		if x.Hash == h {
			s.en[i].LastUsed = time.Now().Unix()
			_ = s.save()
			return true
		}
	}
	if allowEmpty && len(s.en) == 0 {
		return true
	}
	return false
}

func BearerOf(h string) string {
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

var _ = fmt.Sprint
