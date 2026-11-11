package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// MaxHistory is how many entries are kept; the oldest are dropped first.
	MaxHistory = 1000
	// MaxHistorySQL is the largest script that is recorded. Bigger scripts are
	// skipped rather than truncated, because a cut-off entry would be unsafe to re-run.
	MaxHistorySQL = 20000
)

// HistoryEntry is one executed script.
type HistoryEntry struct {
	ID     string    `json:"id"`
	Conn   string    `json:"conn"`
	DB     string    `json:"db,omitempty"`
	Schema string    `json:"schema,omitempty"`
	SQL    string    `json:"sql"`
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Ms     int64     `json:"ms"`
}

type historyFile struct {
	Enabled bool           `json:"enabled"`
	Entries []HistoryEntry `json:"entries"`
}

// History persists executed queries next to the connections file (0600).
// Query text can contain sensitive literals, so recording can be switched off.
type History struct {
	mu      sync.Mutex
	path    string
	enabled bool
	entries []HistoryEntry // oldest first
}

// OpenHistory loads (or creates) <user config dir>/dbcat/history.json.
func OpenHistory() (*History, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return OpenHistoryAt(filepath.Join(dir, "dbcat", "history.json"))
}

func OpenHistoryAt(path string) (*History, error) {
	h := &History{path: path, enabled: true}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	var f historyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	h.enabled, h.entries = f.Enabled, f.Entries
	return h, nil
}

func (h *History) Enabled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.enabled
}

// SetEnabled turns recording on or off. Existing entries are kept.
func (h *History) SetEnabled(on bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled = on
	return h.flush()
}

// Add records a run. It does nothing when recording is off, when sql is blank
// or larger than MaxHistorySQL. Re-running the newest identical script for the
// same connection/database only refreshes that entry.
func (h *History) Add(e HistoryEntry) error {
	e.SQL = strings.TrimSpace(e.SQL)
	if e.SQL == "" || len(e.SQL) > MaxHistorySQL {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.enabled {
		return nil
	}
	if n := len(h.entries); n > 0 {
		last := &h.entries[n-1]
		if last.Conn == e.Conn && last.DB == e.DB && last.Schema == e.Schema && last.SQL == e.SQL {
			last.At, last.OK, last.Ms = e.At, e.OK, e.Ms
			return h.flush()
		}
	}
	id, err := newID()
	if err != nil {
		return err
	}
	e.ID = id
	h.entries = append(h.entries, e)
	if over := len(h.entries) - MaxHistory; over > 0 {
		h.entries = append([]HistoryEntry(nil), h.entries[over:]...)
	}
	return h.flush()
}

// List returns entries newest first. conn "" means every connection; query is
// a case-insensitive substring of the SQL; limit <= 0 means no limit.
func (h *History) List(conn, query string, limit int) []HistoryEntry {
	q := strings.ToLower(strings.TrimSpace(query))
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []HistoryEntry
	for i := len(h.entries) - 1; i >= 0; i-- {
		e := h.entries[i]
		if conn != "" && e.Conn != conn {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.SQL), q) {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (h *History) Get(id string) (HistoryEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		if e.ID == id {
			return e, true
		}
	}
	return HistoryEntry{}, false
}

func (h *History) Delete(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.entries[:0]
	for _, e := range h.entries {
		if e.ID != id {
			kept = append(kept, e)
		}
	}
	h.entries = kept
	return h.flush()
}

// Clear removes the entries of one connection, or all of them when conn is "".
func (h *History) Clear(conn string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if conn == "" {
		h.entries = nil
		return h.flush()
	}
	kept := h.entries[:0]
	for _, e := range h.entries {
		if e.Conn != conn {
			kept = append(kept, e)
		}
	}
	h.entries = kept
	return h.flush()
}

func (h *History) flush() error {
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(historyFile{Enabled: h.enabled, Entries: h.entries})
	if err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}
