package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"dbcat/internal/db"
	"dbcat/internal/store"
)

func TestAsciiJSONRoundTrips(t *testing.T) {
	for _, msg := range []string{"plain", "1개 행을 삭제했습니다", "emoji 😀 ok", `quote " and \ slash`} {
		raw, _ := json.Marshal(map[string]string{"msg": msg})
		out := asciiJSON(raw)
		for i := 0; i < len(out); i++ {
			if out[i] >= 0x80 {
				t.Fatalf("non-ASCII byte in %q", out)
			}
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(out), &got); err != nil || got["msg"] != msg {
			t.Fatalf("round trip of %q failed: %q, %v", msg, got["msg"], err)
		}
	}
}

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "c.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s.Handler()
}

func TestGuardRejectsNonHTMXPost(t *testing.T) {
	h := newTestServer(t)
	for _, path := range []string{"/conn/save", "/conn/delete", "/table/cell", "/table/delete", "/query/run"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without HX-Request = %d, want 403", path, rec.Code)
		}
	}
}

func TestIndexAndStatic(t *testing.T) {
	h := newTestServer(t)
	for _, path := range []string{"/", "/static/app.js", "/static/app.css", "/static/htmx.min.js", "/tree/conns"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestConnTestValidation(t *testing.T) {
	h := newTestServer(t)
	post := func(form string) string {
		req := httptest.NewRequest(http.MethodPost, "/conn/test", strings.NewReader(form))
		req.Header.Set("HX-Request", "true")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	for _, form := range []string{
		"kind=oracle&name=x",                                                  // unknown engine
		"kind=postgres&name=x&port=70000",                                     // port out of range
		"kind=sqlite&name=x",                                                  // sqlite without a file path
		"kind=sqlite&name=",                                                   // missing name
		"kind=postgres&name=x&ssl_mode=bogus",                                 // unknown TLS mode
		"kind=postgres&name=x&ssh_on=on&ssh_host=h",                           // tunnel without user
		"kind=postgres&name=x&ssh_on=on&ssh_host=h&ssh_user=u&ssh_port=0",     // bad tunnel port
		"kind=postgres&name=x&ssh_on=on&ssh_host=h&ssh_user=u",                // tunnel without password or key
		"kind=postgres&name=x&ssh_on=on&ssh_host=h&ssh_user=u&ssh_password=p", // known_hosts check refuses an unknown host
	} {
		if out := post(form); !strings.Contains(out, `class="bad"`) {
			t.Errorf("%q should be rejected, got: %s", form, out)
		}
	}
}

func TestExportEmitters(t *testing.T) {
	cols := []string{"id", "name", "note"}
	rows := [][]db.Cell{
		{{V: "1", Lit: true}, {V: `a,"b"`}, {Null: true}},
		{{V: "2", Lit: true}, {V: "<x>"}, {V: ""}},
	}
	var csvOut, jsonOut strings.Builder
	ce, cf := csvEmitter(&csvOut)
	je, jf := jsonEmitter(&jsonOut)
	for _, e := range []func([]string, []db.Cell) error{ce, je} {
		if err := e(cols, nil); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if err := e(cols, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := cf(); err != nil {
		t.Fatal(err)
	}
	if err := jf(); err != nil {
		t.Fatal(err)
	}
	if want := "\xEF\xBB\xBFid,name,note\n1,\"a,\"\"b\"\"\",\n2,<x>,\n"; csvOut.String() != want {
		t.Errorf("csv = %q, want %q", csvOut.String(), want)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(jsonOut.String()), &got); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, jsonOut.String())
	}
	if got[0]["id"] != float64(1) || got[0]["note"] != nil || got[0]["name"] != `a,"b"` || got[1]["note"] != "" || got[1]["name"] != "<x>" {
		t.Errorf("json = %+v", got)
	}
}

func TestQueryRunMultipleStatements(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.OpenAt(filepath.Join(dir, "c.json"))
	cfg, err := st.Save(db.Config{Name: "t", Kind: db.SQLite, Database: filepath.Join(dir, "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run := func(script string) string {
		form := url.Values{"c": {cfg.ID}, "d": {"main"}, "sql": {script}}
		req := httptest.NewRequest("POST", "/query/run", strings.NewReader(form.Encode()))
		req.Header.Set("HX-Request", "true")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Body.String()
	}

	// single statement: no tab strip
	if out := run("SELECT 1 AS one"); strings.Contains(out, "rtabs") || !strings.Contains(out, ">one</th>") {
		t.Fatalf("single statement should render without tabs:\n%s", out)
	}
	// several statements: one tab each, the first result is active
	out := run("CREATE TABLE a(x); INSERT INTO a VALUES (1),(2); SELECT x FROM a; SELECT count(*) AS n FROM a")
	for _, want := range []string{"rtabs", `data-rtab="1"`, "1 실행", "2 실행", "결과 1", "결과 2", "4개 문"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in multi-statement output", want)
		}
	}
	if strings.Count(out, "<table") != 2 || strings.Count(out, " hidden") != 3 {
		t.Errorf("expected 2 grids with 3 hidden panes (first result active):\n%s", out)
	}
	// an error stops the run, is the active tab, and reports what was skipped
	out = run("SELECT 1; SELEC 2; SELECT 3")
	if !strings.Contains(out, "2 오류") || !strings.Contains(out, "1개는 오류로 실행되지 않음") || !strings.Contains(out, `class="errbar"`) {
		t.Errorf("error run output:\n%s", out)
	}
	if strings.Contains(out, "SELECT 3</div>") {
		t.Error("statement after the error must not be shown as run")
	}
	if out := run("  ; -- nothing\n"); !strings.Contains(out, "errbar") {
		t.Error("empty script must report an error")
	}
}

// End to end through the HTTP handlers against a real SQLite file.
func TestInsertDesignExportFlow(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.OpenAt(filepath.Join(dir, "c.json"))
	cfg, err := st.Save(db.Config{Name: "t", Kind: db.SQLite, Database: filepath.Join(dir, "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := s.Handler()
	do := func(method, path, form string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(form))
		req.Header.Set("HX-Request", "true")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	base := "c=" + cfg.ID + "&d=main&mode=new&t=items&nf=2&nx=1" +
		"&f.0.orig=&f.0.name=id&f.0.type=INTEGER&f.0.notnull=on&f.0.pk=on" +
		"&f.1.orig=&f.1.name=label&f.1.type=TEXT" +
		"&x.0.orig=&x.0.name=idx_label&x.0.cols=label"

	if rec := do("POST", "/design/preview", base+"&fmt=text"); !strings.Contains(rec.Body.String(), `CREATE TABLE "items"`) {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do("POST", "/design/preview", strings.Replace(base, "f.1.type=TEXT", "f.1.type=TEXT);DROP", 1)+"&fmt=text"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("injected type must be rejected, got %d", rec.Code)
	}
	if rec := do("POST", "/design/apply", base); rec.Header().Get("HX-Trigger") == "" || !strings.Contains(rec.Header().Get("HX-Trigger"), "schema-changed") {
		t.Fatalf("apply: %q %s", rec.Header().Get("HX-Trigger"), rec.Body.String())
	}

	ref := "c=" + cfg.ID + "&d=main&t=items"
	if rec := do("POST", "/table/insert", ref+"&n=2&col.0=id&m.0=d&col.1=label&m.1=v&v.1=hello"); !strings.Contains(rec.Header().Get("HX-Trigger"), "rows-changed") {
		t.Fatalf("insert: %q %s", rec.Header().Get("HX-Trigger"), rec.Body.String())
	}
	if rec := do("POST", "/table/insert", ref+"&n=1&col.0=evil&m.0=v&v.0=x"); !strings.Contains(rec.Body.String(), `class="bad"`) {
		t.Errorf("unknown column must be reported: %s", rec.Body.String())
	}

	rec := do("GET", "/table/export?"+ref+"&fmt=json", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"label": "hello"`) ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "items.json") {
		t.Fatalf("export: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec := do("GET", "/table/export?"+ref+"&fmt=xml", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad format = %d, want 400", rec.Code)
	}
	if rec := do("GET", "/table/export?"+ref+"&fmt=csv&where=1%3D1%3B+DROP+TABLE+items", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("semicolon filter must be rejected, got %d", rec.Code)
	}
}
