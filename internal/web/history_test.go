package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"dbcat/internal/db"
	"dbcat/internal/store"
)

func TestQueryHistoryFlow(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.OpenAt(filepath.Join(dir, "c.json"))
	cfgA, err := st.Save(db.Config{Name: "A", Kind: db.SQLite, Database: filepath.Join(dir, "a.db")})
	if err != nil {
		t.Fatal(err)
	}
	cfgB, _ := st.Save(db.Config{Name: "B", Kind: db.SQLite, Database: filepath.Join(dir, "b.db")})
	hist, err := store.OpenHistoryAt(filepath.Join(dir, "h.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	s.SetHistory(hist)
	defer s.Close()
	h := s.Handler()
	do := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		var req *http.Request
		if method == "GET" {
			req = httptest.NewRequest(method, path+"?"+form.Encode(), nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	run := func(conn, sql string) {
		do("POST", "/query/run", url.Values{"c": {conn}, "d": {"main"}, "sql": {sql}})
	}

	run(cfgA.ID, "SELECT 1 AS first_query")
	run(cfgA.ID, "SELECT 1 AS first_query") // identical consecutive re-run refreshes the entry, no duplicate
	run(cfgA.ID, "SELEC broken")            // failed runs are recorded too
	run(cfgB.ID, "SELECT 2 AS other_conn")  // another connection
	run(cfgA.ID, "   ")                     // blank script is an error, not history

	if n := len(hist.List("", "", 0)); n != 3 {
		for _, e := range hist.List("", "", 0) {
			t.Logf("entry: conn=%s db=%q schema=%q ok=%v sql=%q", e.Conn[:4], e.DB, e.Schema, e.OK, e.SQL)
		}
		t.Fatalf("entries = %d, want 3", n)
	}
	bad := hist.List(cfgA.ID, "broken", 0)
	if len(bad) != 1 || bad[0].OK {
		t.Fatalf("failed run entry = %+v", bad)
	}

	// dialog scoped to a connection, then widened and filtered
	out := do("GET", "/history", url.Values{"c": {cfgA.ID}}).Body.String()
	if !strings.Contains(out, "first_query") || strings.Contains(out, "other_conn") || !strings.Contains(out, "hist-item bad") {
		t.Fatalf("scoped dialog:\n%s", out)
	}
	list := do("GET", "/history/list", url.Values{"scope": {""}, "q": {"OTHER"}}).Body.String()
	if !strings.Contains(list, "other_conn") || strings.Contains(list, "first_query") || !strings.Contains(list, ">B ·") && !strings.Contains(list, "B ·") {
		t.Fatalf("search across connections:\n%s", list)
	}

	// the full SQL comes from /history/sql, addressed by id
	id := regexp.MustCompile(`data-id="([0-9a-f]+)"`).FindStringSubmatch(list)[1]
	if rec := do("GET", "/history/sql", url.Values{"id": {id}}); rec.Body.String() != "SELECT 2 AS other_conn" {
		t.Errorf("sql = %q", rec.Body.String())
	}
	if rec := do("GET", "/history/sql", url.Values{"id": {"nope"}}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d", rec.Code)
	}

	// delete one
	do("POST", "/history/delete", url.Values{"id": {id}})
	if n := len(hist.List("", "", 0)); n != 2 {
		t.Fatalf("after delete: %d", n)
	}

	// turning recording off stops new entries; existing ones stay
	do("POST", "/history/enable", url.Values{}) // checkbox unchecked => no "on"
	run(cfgA.ID, "SELECT 'not recorded'")
	if n := len(hist.List("", "", 0)); n != 2 || hist.Enabled() {
		t.Fatalf("recorded while off: %d enabled=%v", n, hist.Enabled())
	}
	do("POST", "/history/enable", url.Values{"on": {"1"}})
	if !hist.Enabled() {
		t.Fatal("could not re-enable")
	}

	// clear by scope
	do("POST", "/history/clear", url.Values{"scope": {cfgA.ID}})
	if n := len(hist.List(cfgA.ID, "", 0)); n != 0 {
		t.Fatalf("scope clear left %d", n)
	}

	// deleting a connection removes its history
	run(cfgB.ID, "SELECT 3 AS keep_until_delete")
	do("POST", "/conn/delete", url.Values{"c": {cfgB.ID}})
	if n := len(hist.List(cfgB.ID, "", 0)); n != 0 {
		t.Fatalf("history of a deleted connection remains: %d", n)
	}
}

func TestHistoryDisabledWithoutStore(t *testing.T) {
	st, _ := store.OpenAt(filepath.Join(t.TempDir(), "c.json"))
	s, err := New(st) // no SetHistory
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, path := range []string{"/history", "/history/list", "/history/sql"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s without history = %d, want 404", path, rec.Code)
		}
	}
}
