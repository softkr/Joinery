package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newHistory(t *testing.T) (*History, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "history.json")
	h, err := OpenHistoryAt(path)
	if err != nil {
		t.Fatal(err)
	}
	return h, path
}

func entry(conn, sql string) HistoryEntry {
	return HistoryEntry{Conn: conn, DB: "main", SQL: sql, At: time.Now(), OK: true, Ms: 3}
}

func TestHistoryAddListFilterDelete(t *testing.T) {
	h, path := newHistory(t)
	for _, e := range []HistoryEntry{entry("a", "SELECT 1"), entry("b", "SELECT 2"), entry("a", "UPDATE t SET x = 1"), entry("a", "  ")} {
		if err := h.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.List("", "", 0); len(got) != 3 || got[0].SQL != "UPDATE t SET x = 1" {
		t.Fatalf("all, newest first = %+v", got)
	}
	if got := h.List("a", "", 0); len(got) != 2 {
		t.Fatalf("conn filter = %+v", got)
	}
	if got := h.List("", "select", 0); len(got) != 2 {
		t.Fatalf("case-insensitive search = %+v", got)
	}
	if got := h.List("", "", 1); len(got) != 1 {
		t.Fatalf("limit = %+v", got)
	}

	// persisted with 0600 and reloadable
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("history file mode: %v %v", st, err)
	}
	h2, err := OpenHistoryAt(path)
	if err != nil || len(h2.List("", "", 0)) != 3 || !h2.Enabled() {
		t.Fatalf("reload: %v %v", h2.List("", "", 0), err)
	}

	id := h.List("b", "", 0)[0].ID
	if e, ok := h.Get(id); !ok || e.SQL != "SELECT 2" {
		t.Fatalf("get = %+v %v", e, ok)
	}
	if err := h.Delete(id); err != nil || len(h.List("b", "", 0)) != 0 {
		t.Fatalf("delete: %v", err)
	}
	if err := h.Clear("a"); err != nil || len(h.List("", "", 0)) != 0 {
		t.Fatalf("clear conn: %v", h.List("", "", 0))
	}
}

func TestHistoryDedupeLimitsAndToggle(t *testing.T) {
	h, _ := newHistory(t)
	h.Add(entry("a", "SELECT 1"))
	again := entry("a", "SELECT 1")
	again.OK, again.Ms = false, 99
	h.Add(again)
	if got := h.List("", "", 0); len(got) != 1 || got[0].OK || got[0].Ms != 99 {
		t.Fatalf("identical re-run should refresh the newest entry: %+v", got)
	}
	// same text on another database is a different entry
	other := entry("a", "SELECT 1")
	other.DB = "other"
	h.Add(other)
	if len(h.List("", "", 0)) != 2 {
		t.Fatal("different database must not be merged")
	}

	// oversized scripts are skipped, not truncated
	h.Add(entry("a", strings.Repeat("x", MaxHistorySQL+1)))
	if len(h.List("", "", 0)) != 2 {
		t.Fatal("oversized script recorded")
	}

	// capped at MaxHistory, oldest dropped
	h.Clear("")
	for i := 0; i < MaxHistory+5; i++ {
		h.Add(entry("a", "SELECT "+strings.Repeat("1", 1)+string(rune('a'+i%26))+strings.Repeat("_", i/26)))
	}
	if got := h.List("", "", 0); len(got) != MaxHistory {
		t.Fatalf("len = %d, want %d", len(got), MaxHistory)
	}

	// recording can be switched off and survives a reload
	h.Clear("")
	if err := h.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	h.Add(entry("a", "SELECT secret"))
	if len(h.List("", "", 0)) != 0 || h.Enabled() {
		t.Fatal("recorded while disabled")
	}
	h2, _ := OpenHistoryAt(h.path)
	if h2.Enabled() {
		t.Fatal("disabled state not persisted")
	}
}
