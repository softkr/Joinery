package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"dbcat/internal/db"
	"dbcat/internal/store"
)

func backupFixture(t *testing.T) (h http.Handler, s *Server, src, dst db.Config) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenAt(filepath.Join(dir, "c.json"))
	if err != nil {
		t.Fatal(err)
	}
	if src, err = st.Save(db.Config{Name: "src", Kind: db.SQLite, Database: filepath.Join(dir, "src.db")}); err != nil {
		t.Fatal(err)
	}
	if dst, err = st.Save(db.Config{Name: "dst", Kind: db.SQLite, Database: filepath.Join(dir, "dst.db")}); err != nil {
		t.Fatal(err)
	}
	s, err = New(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	h = s.Handler()

	c, err := s.conn(t.Context(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.RunScript(t.Context(), db.Ref{}, `CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT);
INSERT INTO a VALUES (1,'it''s'),(2,NULL);
CREATE TABLE b(id INTEGER PRIMARY KEY);
INSERT INTO b VALUES (7);
CREATE VIEW va AS SELECT * FROM a;`); err != nil {
		t.Fatal(err)
	}
	return h, s, src, dst
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestBackupDialogAndDump(t *testing.T) {
	h, _, src, _ := backupFixture(t)
	q := url.Values{"c": {src.ID}, "d": {"main"}}

	dlg := get(h, "/backup/dialog?"+q.Encode())
	if dlg.Code != 200 || !strings.Contains(dlg.Body.String(), `value="a"`) || !strings.Contains(dlg.Body.String(), `value="b"`) || strings.Contains(dlg.Body.String(), `value="va"`) {
		t.Fatalf("dialog = %d %s", dlg.Code, dlg.Body.String())
	}

	q.Set("struct", "1")
	q.Set("data", "1")
	q.Set("views", "1")
	q.Add("t", "a")
	rec := get(h, "/backup/dump?"+q.Encode())
	body := rec.Body.String()
	if cd := rec.Header().Get("Content-Disposition"); rec.Code != 200 || !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, ".sql") {
		t.Fatalf("dump headers = %d %v", rec.Code, rec.Header())
	}
	for _, want := range []string{"CREATE TABLE a(", `'it''s'`, "CREATE VIEW"} {
		if !strings.Contains(body, want) {
			t.Errorf("dump lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "CREATE TABLE b(") || strings.Contains(body, "INSERT INTO \"b\"") {
		t.Errorf("unselected table b leaked into the dump:\n%s", body)
	}

	// views/objects only: no tables at all
	q2 := url.Values{"c": {src.ID}, "d": {"main"}, "struct": {"1"}, "views": {"1"}, "nt": {"1"}}
	if body := get(h, "/backup/dump?"+q2.Encode()).Body.String(); strings.Contains(body, "CREATE TABLE") {
		t.Errorf("nt=1 still dumped tables:\n%s", body)
	}

	// a table name that is not in the database is refused, not interpolated
	bad := url.Values{"c": {src.ID}, "d": {"main"}, "struct": {"1"}, "t": {`a"; DROP TABLE a; --`}}
	if rec := get(h, "/backup/dump?"+bad.Encode()); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown table = %d, want 400", rec.Code)
	}
	if rec := get(h, "/backup/dump?c=nope&d=main&struct=1"); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown connection = %d, want 400", rec.Code)
	}
}

func restoreReq(t *testing.T, dstID, name, content string, extra map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("c", dstID)
	mw.WriteField("d", "main")
	for k, v := range extra {
		mw.WriteField(k, v)
	}
	if name != "" {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write([]byte(content))
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/backup/restore", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	return req
}

func TestRestoreRoundTripAndErrors(t *testing.T) {
	h, s, src, dst := backupFixture(t)
	dump := get(h, "/backup/dump?"+url.Values{"c": {src.ID}, "d": {"main"}, "struct": {"1"}, "data": {"1"}, "views": {"1"}}.Encode()).Body.String()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, restoreReq(t, dst.ID, "full.sql", "\xEF\xBB\xBF"+dump, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "완료") || !strings.Contains(rec.Header().Get("HX-Trigger"), "schema-changed") {
		t.Fatalf("restore = %d trig=%q %s", rec.Code, rec.Header().Get("HX-Trigger"), rec.Body.String())
	}
	again := get(h, "/backup/dump?"+url.Values{"c": {dst.ID}, "d": {"main"}, "struct": {"1"}, "data": {"1"}, "views": {"1"}}.Encode()).Body.String()
	strip := func(s string) string { return s[strings.Index(s, "\n\n"):] } // drop the timestamp header
	if strip(dump) != strip(again) {
		t.Errorf("restored dump differs\n--- src\n%s\n--- dst\n%s", dump, again)
	}

	// no file
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, restoreReq(t, dst.ID, "", "", nil))
	if !strings.Contains(rec.Body.String(), "파일을 선택") {
		t.Errorf("missing file: %s", rec.Body.String())
	}
	// failing statement stops, and "continue" runs the rest
	script := "INSERT INTO b VALUES (7);\nINSERT INTO b VALUES (8);"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, restoreReq(t, dst.ID, "e.sql", script, nil))
	if !strings.Contains(rec.Body.String(), "중단됨") || strings.Contains(rec.Header().Get("HX-Trigger"), "toast") {
		t.Errorf("stop on error: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, restoreReq(t, dst.ID, "e.sql", script, map[string]string{"cont": "1"}))
	if !strings.Contains(rec.Body.String(), "일부 실패") {
		t.Errorf("continue on error: %s", rec.Body.String())
	}
	if n, _ := mustConn(t, s, dst.ID).Count(t.Context(), db.Ref{Table: "b"}, ""); n != 2 {
		t.Errorf("rows in b = %d, want 2", n)
	}

	// CSRF guard: a plain (non-htmx) POST is refused
	req := restoreReq(t, dst.ID, "x.sql", "SELECT 1;", nil)
	req.Header.Del("HX-Request")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-htmx restore = %d, want 403", rec.Code)
	}
}

func mustConn(t *testing.T, s *Server, id string) *db.Conn {
	t.Helper()
	c, err := s.conn(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDumpNameSanitised(t *testing.T) {
	if got := sanitizeName(`a/b\c d:é`); got != "a_b_c_d_é" {
		t.Errorf("sanitizeName = %q", got)
	}
	if got := sanitizeName(""); got != "backup" {
		t.Errorf("empty = %q", got)
	}
}
