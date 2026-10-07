// Package lanes implements N egress lanes like lingling but without stem:
// each lane is either an external socks5 proxy or a spawned tor process
// with ExitNodes pin. Picking is least-loaded healthy lane.
package lanes

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Lane struct {
	Index       int
	Country     string
	SocksAddr   string
	ExitIP      string
	Active      int
	LimitedTill time.Time
	Score       int
	Healthy     bool
	Healing     bool
	Rotating    bool
	LastUsed    time.Time
	LastReal    time.Time
	cmd         *exec.Cmd
}

type Proof struct {
	T       time.Time `json:"t"`
	Lane    int       `json:"lane"`
	Country string    `json:"cc"`
	IP      string    `json:"ip"`
	Model   string    `json:"model"`
	Status  int       `json:"status"`
	Bytes   int64     `json:"bytes"`
	Secs    float64   `json:"secs"`
	Note    string    `json:"note"`
}

type Manager struct {
	mu        sync.Mutex
	lanes     []*Lane
	dataDir   string
	proofPath string
	countries []string
	fallback  []string
	noTor     bool
	seq       int
	// OnRotate, when set (tor-spawned lanes), respawns the lane's process so
	// a 429 lands on a fresh exit. The lane stays retired until RotateDone
	// reports the new exit.
	OnRotate func(*Lane)
}

func New(dataDir string, countries, fallback []string, socks []string, noTor bool, count int) *Manager {
	if len(countries) == 0 {
		countries = []string{"us", "de", "nl", "fr", "se", "ch"}
	}
	m := &Manager{dataDir: dataDir, countries: countries, fallback: fallback, noTor: noTor}
	m.proofPath = filepath.Join(dataDir, "proof.jsonl")
	n := count
	if len(socks) > 0 {
		n = len(socks)
	}
	for i := 0; i < n; i++ {
		cc := countries[i%len(countries)]
		addr := ""
		if i < len(socks) {
			addr = socks[i]
		}
		m.lanes = append(m.lanes, &Lane{Index: i + 1, Country: cc, SocksAddr: addr, Healthy: addr != "" || noTor})
	}
	return m
}

func (m *Manager) Lanes() []*Lane {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Lane, len(m.lanes))
	copy(out, m.lanes)
	return out
}

// Direct reports whether lanes bypass tor (test harnesses only).
// Anything that must never leave through a plain connection — the
// capability catalog fetch among them — checks this first.
func (m *Manager) Direct() bool { return m.noTor }

// Countries loads countries.txt like lingling: line1 primary, line2 fallback, line3 preferred.
func LoadCountriesFile(dataDir string, def, fb []string) ([]string, []string) {
	p := filepath.Join(dataDir, "countries.txt")
	f, err := os.Open(p)
	if err != nil {
		return def, fb
	}
	defer f.Close()
	var pools [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		// strip comments
		for i, c := range line {
			if c == '#' {
				line = line[:i]
				break
			}
		}
		if trim(line) == "" {
			pools = append(pools, nil)
			continue
		}
		var pool []string
		cur := ""
		for _, c := range line + "," {
			if c == ',' || c == ' ' || c == '\t' {
				if len(trim(cur)) == 2 {
					pool = append(pool, lower(trim(cur)))
				}
				cur = ""
				continue
			}
			cur += string(c)
		}
		pools = append(pools, pool)
	}
	if len(pools) > 0 && len(pools[0]) > 0 {
		def = pools[0]
	}
	if len(pools) > 1 && len(pools[1]) > 0 {
		fb = pools[1]
	}
	return def, fb
}

func trim(s string) string {
	a, b := 0, len(s)
	for a < b && (s[a] == ' ' || s[a] == '\t' || s[a] == '\n' || s[a] == '\r') {
		a++
	}
	for b > a && (s[b-1] == ' ' || s[b-1] == '\t' || s[b-1] == '\n' || s[b-1] == '\r') {
		b--
	}
	return s[a:b]
}

func lower(s string) string {
	o := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		o[i] = c
	}
	return string(o)
}

// Pick returns the least-loaded healthy lane not in exclude.
func (m *Manager) Pick(exclude map[int]bool) *Lane {
	return m.PickCountry(exclude, "")
}

// PickCountry prefers lanes pinned to cc (per-model exit country, e.g.
// union-alpha wants us); falls back to any healthy lane.
func (m *Manager) PickCountry(exclude map[int]bool, cc string) *Lane {
	if cc != "" {
		if l := m.pick(exclude, cc); l != nil {
			return l
		}
	}
	return m.pick(exclude, "")
}

func (m *Manager) pick(exclude map[int]bool, cc string) *Lane {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var best *Lane
	for _, l := range m.lanes {
		if exclude != nil && exclude[l.Index] {
			continue
		}
		if !l.Healthy || l.Healing || l.Rotating {
			continue
		}
		if cc != "" && l.Country != cc {
			continue
		}
		if best == nil {
			best = l
			continue
		}
		bl := 0
		if l.LimitedTill.After(now) {
			bl = 1
		}
		bbl := 0
		if best.LimitedTill.After(now) {
			bbl = 1
		}
		if bl != bbl {
			if bl < bbl {
				best = l
			}
			continue
		}
		if l.Active != best.Active {
			if l.Active < best.Active {
				best = l
			}
			continue
		}
		if l.LastUsed.Before(best.LastUsed) {
			best = l
		}
	}
	if best != nil {
		best.Active++
		best.LastUsed = now
		best.LastReal = now
	}
	return best
}

func (m *Manager) Release(l *Lane) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l.Active > 0 {
		l.Active--
	}
}

func (m *Manager) NoteResult(country string, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if country == "" || country == "*" {
		return
	}
	if status != 200 && status != 429 {
		return
	}
	for _, l := range m.lanes {
		if l.Country == country {
			if status == 200 {
				l.Score++
			} else {
				l.Score--
			}
		}
	}
}

// NoteLimited retires the lane exit until retryAfter; returns until.
func (m *Manager) NoteLimited(l *Lane, retryAfter time.Duration) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	span := 10 * time.Minute
	if retryAfter > 0 && retryAfter < span {
		span = retryAfter
	}
	l.LimitedTill = time.Now().Add(span)
	l.Score--
	return l.LimitedTill
}

// Rotate retires the lane's exit after a 429 and asks OnRotate (tor respawn,
// when the lanes are tor-spawned) for a fresh one in the best-scoring
// country. The retry-after limit is kept until the new exit is up — clearing
// it here handed the same limited exit straight back to the next request.
func (m *Manager) Rotate(l *Lane) string {
	m.mu.Lock()
	pool := append(append([]string{}, m.countries...), m.fallback...)
	seen := map[string]bool{}
	var ranked []string
	for _, c := range pool {
		if !seen[c] {
			seen[c] = true
			ranked = append(ranked, c)
		}
	}
	// score of country = max lane score
	score := map[string]int{}
	for _, x := range m.lanes {
		if v, ok := score[x.Country]; !ok || x.Score > v {
			score[x.Country] = x.Score
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return score[ranked[i]] > score[ranked[j]] })
	for _, c := range ranked {
		if c == l.Country {
			continue
		}
		l.Country = c
		break
	}
	hook := m.OnRotate
	if hook != nil && !l.Rotating {
		l.Rotating = true
		go hook(l)
	}
	cc := l.Country
	m.mu.Unlock()
	return cc
}

// RotateDone reports a lane's fresh exit (a respawn finished): the lane is
// usable again and its retry-after limit is lifted.
func (m *Manager) RotateDone(l *Lane, socks, ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if socks != "" {
		l.SocksAddr = socks
		l.Healthy = true
	}
	l.ExitIP = ip
	l.Rotating = false
	l.LimitedTill = time.Time{}
}

// RotateFailed clears the rotating flag after a failed respawn. The lane
// keeps its retry-after limit (with a cooldown floor) so the next request
// does not immediately try to rotate it again.
func (m *Manager) RotateFailed(l *Lane) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l.Rotating = false
	if floor := time.Now().Add(5 * time.Minute); l.LimitedTill.Before(floor) {
		l.LimitedTill = floor
	}
}

// SetProcess records the tor process backing this lane so rotation can
// replace it; nil for lanes that are not tor-spawned.
func (l *Lane) SetProcess(cmd *exec.Cmd) { l.cmd = cmd }

// Process returns the tor process backing this lane, if any.
func (l *Lane) Process() *exec.Cmd { return l.cmd }

func (m *Manager) Emit(p Proof) {
	m.mu.Lock()
	path := m.proofPath
	m.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(p)
	f.Write(append(b, '\n'))
}

// FreePort finds a free 127.0.0.1 port.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

var _ = fmt.Sprint
