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

func TestBuildERDLimitsAndScope(t *testing.T) {
	cols := []db.Column{{Name: "id", Type: "int", PK: true}, {Name: "ref_id", Type: "int"}}
	tables := []db.TableCols{
		{Name: "lonely1", Columns: cols}, {Name: "lonely2", Columns: cols}, {Name: "lonely3", Columns: cols},
		{Name: "child", Columns: cols}, {Name: "parent", Columns: cols}, {Name: "view1", IsView: true, Columns: cols},
	}
	fks := []db.ForeignKey{
		{Name: "fk_a", Table: "child", Columns: []string{"ref_id"}, RefTable: "parent", RefColumns: []string{"id"}, RefSchema: "public"},
		{Name: "fk_other", Table: "child", Columns: []string{"ref_id"}, RefTable: "elsewhere", RefColumns: []string{"id"}, RefSchema: "hr"},
	}

	// Only relations inside the scope are drawn.
	all := buildERD(tables, fks, "public", 100)
	if all.Truncated || all.Total != 6 || len(all.Tables) != 6 || len(all.Edges) != 1 || all.Edges[0].Name != "fk_a" {
		t.Fatalf("full = %+v", all)
	}
	// FK columns are flagged on the owning table, in database order.
	if !all.Tables[3].Columns[1].FK || all.Tables[3].Columns[0].FK || !all.Tables[3].Columns[0].PK || all.Tables[0].Name != "lonely1" {
		t.Errorf("column flags / order = %+v", all.Tables)
	}
	if !all.Tables[5].View {
		t.Error("view flag lost")
	}

	// With a cap, related tables win over lonely ones and edges stay consistent.
	cut := buildERD(tables, fks, "public", 2)
	if !cut.Truncated || len(cut.Tables) != 2 || len(cut.Edges) != 1 {
		t.Fatalf("cut = %+v", cut)
	}
	names := cut.Tables[0].Name + "," + cut.Tables[1].Name
	if names != "child,parent" {
		t.Errorf("kept %s, want child,parent", names)
	}
	// An edge whose end was dropped must not be emitted.
	cut = buildERD(tables, fks, "public", 1)
	if len(cut.Edges) != 0 {
		t.Errorf("dangling edge: %+v", cut.Edges)
	}
}

func TestObjectsInfoAndERDEndpoints(t *testing.T) {
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

	// Create two tables through the designer; the child gets a foreign key on creation (works on SQLite).
	create := func(extra url.Values) *httptest.ResponseRecorder {
		f := url.Values{"c": {cfg.ID}, "d": {"main"}, "mode": {"new"}, "nf": {"2"}, "nx": {"0"}, "nk": {"0"}}
		for k, v := range extra {
			f[k] = v
		}
		return do("POST", "/design/apply", f)
	}
	if rec := create(url.Values{"t": {"parents"}, "f.0.orig": {""}, "f.0.name": {"id"}, "f.0.type": {"INTEGER"}, "f.0.notnull": {"on"}, "f.0.pk": {"on"},
		"f.1.orig": {""}, "f.1.name": {"label"}, "f.1.type": {"TEXT"}}); !strings.Contains(rec.Header().Get("HX-Trigger"), "schema-changed") {
		t.Fatalf("create parents: %q %s", rec.Header().Get("HX-Trigger"), rec.Body.String())
	}
	if rec := create(url.Values{"t": {"kids"}, "nk": {"1"}, "f.0.orig": {""}, "f.0.name": {"id"}, "f.0.type": {"INTEGER"}, "f.0.notnull": {"on"}, "f.0.pk": {"on"},
		"f.1.orig": {""}, "f.1.name": {"parent_id"}, "f.1.type": {"INTEGER"},
		"k.0.orig": {""}, "k.0.name": {"fk_kids_parent"}, "k.0.cols": {"parent_id"}, "k.0.rtable": {"parents"}, "k.0.rcols": {"id"}, "k.0.ondelete": {"CASCADE"}}); !strings.Contains(rec.Header().Get("HX-Trigger"), "schema-changed") {
		t.Fatalf("create kids: %q %s", rec.Header().Get("HX-Trigger"), rec.Body.String())
	}

	ref := url.Values{"c": {cfg.ID}, "d": {"main"}, "t": {"kids"}}
	general := do("GET", "/objects/info", ref).Body.String()
	for _, want := range []string{"kids", "테이블 (TABLE)", "parent_id", "→ parents", "SQLite"} {
		if !strings.Contains(general, want) {
			t.Errorf("general info misses %q:\n%s", want, general)
		}
	}
	ref.Set("tab", "ddl")
	ddl := do("GET", "/objects/info", ref).Body.String()
	if !strings.Contains(ddl, `class="ddl"`) || !strings.Contains(ddl, "FOREIGN KEY") || !strings.Contains(ddl, "ON DELETE CASCADE") {
		t.Errorf("ddl info:\n%s", ddl)
	}
	missing := do("GET", "/objects/info", url.Values{"c": {cfg.ID}, "d": {"main"}, "t": {"nope"}, "tab": {"ddl"}}).Body.String()
	if !strings.Contains(missing, "errbar") {
		t.Errorf("unknown object must report an error:\n%s", missing)
	}

	rec := do("GET", "/erd/data", url.Values{"c": {cfg.ID}, "d": {"main"}})
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("erd: %d %s", rec.Code, rec.Body.String())
	}
	var got erdData
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tables) != 2 || len(got.Edges) != 1 || got.Edges[0].Table != "kids" || got.Edges[0].RefTable != "parents" ||
		got.Edges[0].OnDelete != "CASCADE" || got.Truncated {
		t.Fatalf("erd = %+v", got)
	}
	if rec := do("GET", "/erd/data", url.Values{"c": {"nope"}, "d": {"main"}}); rec.Code != http.StatusBadGateway {
		t.Errorf("unknown connection = %d, want 502", rec.Code)
	}

	// the objects panel carries the three view modes and the info pane
	page := do("GET", "/objects", url.Values{"c": {cfg.ID}, "d": {"main"}}).Body.String()
	for _, want := range []string{`data-mode="list"`, `data-mode="details"`, `data-mode="er"`, `class="info-pane"`, `data-info="ddl"`, "이름 필터"} {
		if !strings.Contains(page, want) {
			t.Errorf("objects panel misses %q", want)
		}
	}

	// designer: an existing SQLite FK is listed read-only; adding one to an existing table is refused
	design := do("GET", "/design/edit", url.Values{"c": {cfg.ID}, "d": {"main"}, "t": {"kids"}}).Body.String()
	if !strings.Contains(design, `data-subpane="fks"`) || !strings.Contains(design, "fk-tables") || !strings.Contains(design, "읽기 전용") ||
		!strings.Contains(design, `data-act="add-fk" disabled`) {
		t.Errorf("designer FK tab:\n%s", design)
	}
	prev := do("POST", "/design/preview", url.Values{"c": {cfg.ID}, "d": {"main"}, "t": {"kids"}, "mode": {"edit"}, "fmt": {"text"},
		"nf": {"0"}, "nx": {"0"}, "nk": {"1"}, "k.0.orig": {""}, "k.0.cols": {"parent_id"}, "k.0.rtable": {"parents"}, "k.0.rcols": {"id"}})
	if prev.Code != http.StatusUnprocessableEntity || !strings.Contains(prev.Body.String(), "SQLite") {
		t.Errorf("adding an FK on SQLite must be refused: %d %s", prev.Code, prev.Body.String())
	}
}
