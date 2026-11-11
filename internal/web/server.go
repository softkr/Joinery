// Package web serves the htmx UI: every endpoint returns an HTML fragment
// (or the single shell page). It is mounted inside the Wails webview and can
// also be served over loopback HTTP for development.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbcat/internal/db"
	"dbcat/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const (
	metaTimeout  = 30 * time.Second
	queryTimeout = 120 * time.Second
	defaultSize  = 200
)

var pageSizes = []int{100, 200, 500, 1000}

type Server struct {
	store   *store.Store
	history *store.History // nil: query history disabled
	tmpl    *template.Template

	mu   sync.Mutex
	live map[string]*db.Conn
}

func New(st *store.Store) (*Server, error) {
	s := &Server{store: st, live: map[string]*db.Conn{}}
	t, err := template.New("").Funcs(template.FuncMap{
		"icon":    icon,
		"rows":    fmtRows,
		"add":     func(a, b int) int { return a + b },
		"json":    func(v any) string { b, _ := json.Marshal(v); return string(b) },
		"kindIco": func(k db.Kind) string { return "conn-" + string(k) },
		"newField": func(i string, newTable bool) fieldRow {
			return fieldRow{I: i, Alter: true, NewTbl: newTable}
		},
		"newIndex": func(i string) indexRow { return indexRow{I: i} },
		"newFK":    func(i string) fkRow { return fkRow{I: i} },
		"slice":    func(v ...string) []string { return v },
		"fmtTime":  func(t time.Time) string { return t.Local().Format("01-02 15:04:05") },
		"resultView": func(it resultItem, limit int) map[string]any {
			return map[string]any{"Err": it.Err, "Res": it.Res, "Elapsed": it.Elapsed, "Limit": limit}
		},
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t
	return s, nil
}

// Close releases every live connection.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.live {
		c.Close()
		delete(s.live, id)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	mux.HandleFunc("GET /{$}", s.index)

	mux.HandleFunc("GET /tree/conns", s.treeConns)
	mux.HandleFunc("GET /tree/dbs", s.treeDBs)
	mux.HandleFunc("GET /tree/schemas", s.treeSchemas)
	mux.HandleFunc("GET /tree/groups", s.treeGroups)

	mux.HandleFunc("GET /objects", s.objects)
	mux.HandleFunc("GET /objects/info", s.objectInfo)
	mux.HandleFunc("GET /objects/cat", s.objectCategory)
	mux.HandleFunc("GET /objects/def", s.objectDef)
	mux.HandleFunc("GET /tree/cat", s.treeCategory)
	mux.HandleFunc("GET /erd/data", s.erdData)
	mux.HandleFunc("GET /table/data", s.tableData)
	mux.HandleFunc("GET /table/count", s.tableCount)
	mux.HandleFunc("POST /table/cell", s.tableCell)
	mux.HandleFunc("POST /table/delete", s.tableDelete)
	mux.HandleFunc("GET /table/design", s.tableDesign)
	mux.HandleFunc("GET /table/insert", s.insertDialog)
	mux.HandleFunc("POST /table/insert", s.insertRow)
	mux.HandleFunc("GET /table/export", s.tableExport)
	mux.HandleFunc("GET /backup/dialog", s.backupDialog)
	mux.HandleFunc("GET /backup/dump", s.backupDump)
	mux.HandleFunc("GET /backup/restore-dialog", s.restoreDialog)
	mux.HandleFunc("POST /backup/restore", s.restoreRun)

	mux.HandleFunc("GET /design/new", s.designerNew)
	mux.HandleFunc("GET /design/edit", s.designerEdit)
	mux.HandleFunc("POST /design/preview", s.designPreview)
	mux.HandleFunc("POST /design/apply", s.designApply)

	mux.HandleFunc("GET /query", s.queryPanel)
	mux.HandleFunc("POST /query/run", s.queryRun)
	mux.HandleFunc("GET /query/schema", s.querySchema)

	mux.HandleFunc("GET /history", s.historyDialog)
	mux.HandleFunc("GET /history/list", s.historyList)
	mux.HandleFunc("GET /history/sql", s.historySQL)
	mux.HandleFunc("POST /history/delete", s.historyDelete)
	mux.HandleFunc("POST /history/clear", s.historyClear)
	mux.HandleFunc("POST /history/enable", s.historyEnable)

	mux.HandleFunc("GET /conn/new", s.connDialog)
	mux.HandleFunc("GET /conn/edit", s.connDialog)
	mux.HandleFunc("POST /conn/test", s.connTest)
	mux.HandleFunc("POST /conn/save", s.connSave)
	mux.HandleFunc("POST /conn/delete", s.connDelete)
	mux.HandleFunc("POST /conn/trust-host", s.connTrustHost)
	mux.HandleFunc("POST /conn/close", s.connClose)
	return guard(mux)
}

// guard rejects state-changing requests that did not come from htmx. The
// custom header cannot be sent by a cross-site form, which matters when the
// UI is served over loopback HTTP instead of inside the webview.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("HX-Request") != "true" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// ---- helpers --------------------------------------------------------------

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// fail answers with an inline error fragment (HTTP 200 so htmx swaps it).
func (s *Server) fail(w http.ResponseWriter, name string, err error) {
	s.render(w, name, errData(err))
}

// errData is the template data for an error. When the failure is an unknown
// SSH host key it also carries what the UI needs to offer "verify and trust".
func errData(err error) map[string]any {
	d := map[string]any{"Err": err.Error()}
	var uh *db.UnknownHostError
	if errors.As(err, &uh) {
		d["Trust"] = uh
	}
	return d
}

// toast tells the page to show a notification via the HX-Trigger header.
func toast(w http.ResponseWriter, kind, msg string) {
	events(w, map[string]any{"toast": toastEvent(kind, msg)})
}

// asciiJSON rewrites non-ASCII runes of a JSON document as \uXXXX escapes.
// HTTP header values are decoded as Latin-1 by browsers, so raw UTF-8 (e.g.
// Korean messages) would arrive garbled.
func asciiJSON(b []byte) string {
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			sb.WriteRune(r)
		case r <= 0xFFFF:
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		}
	}
	return sb.String()
}

// Ref is the (connection, database, schema, table) address carried in query
// strings and hidden form fields.
type Ref struct {
	Conn, DB, Schema, Table string
}

func refFrom(r *http.Request) Ref {
	return Ref{
		Conn:   r.FormValue("c"),
		DB:     r.FormValue("d"),
		Schema: r.FormValue("s"),
		Table:  r.FormValue("t"),
	}
}

func (r Ref) dbRef() db.Ref { return db.Ref{Database: r.DB, Schema: r.Schema, Table: r.Table} }

func (r Ref) Query() string {
	v := url.Values{"c": {r.Conn}, "d": {r.DB}}
	if r.Schema != "" {
		v.Set("s", r.Schema)
	}
	if r.Table != "" {
		v.Set("t", r.Table)
	}
	return v.Encode()
}

// conn returns the live connection for id, opening it on first use.
func (s *Server) conn(ctx context.Context, id string) (*db.Conn, error) {
	s.mu.Lock()
	c, ok := s.live[id]
	s.mu.Unlock()
	if ok {
		return c, nil
	}
	cfg, found := s.store.Get(id)
	if !found {
		return nil, errors.New("연결을 찾을 수 없습니다")
	}
	c, err := db.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.live[id]; ok { // lost a race; keep the first
		c.Close()
		return existing, nil
	}
	s.live[id] = c
	return c, nil
}

func (s *Server) drop(id string) {
	s.mu.Lock()
	c := s.live[id]
	delete(s.live, id)
	s.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func (s *Server) isLive(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.live[id]
	return ok
}

func fmtRows(n int64) string {
	if n < 0 {
		return "—"
	}
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func summary(c db.Config) string {
	if c.Kind == db.SQLite {
		return "SQLite · " + c.Database
	}
	host := c.Host
	if host == "" {
		host = "localhost"
	}
	port := c.Port
	if port == 0 {
		port = c.Kind.DefaultPort()
	}
	return fmt.Sprintf("%s · %s@%s:%d", c.Kind.Label(), c.User, host, port)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.render(w, "layout", nil)
}

// ---- tree -----------------------------------------------------------------

type connNode struct {
	db.Config
	Live    bool
	Summary string
}

func (s *Server) treeConns(w http.ResponseWriter, r *http.Request) {
	var nodes []connNode
	for _, c := range s.store.List() {
		nodes = append(nodes, connNode{Config: c, Live: s.isLive(c.ID), Summary: summary(c)})
	}
	s.render(w, "tree-conns", nodes)
}

type treeItem struct {
	Ref
	Name     string
	Children string // URL that loads the children, empty for leaves / inline groups
	HasKids  bool
	Kind     string // db | schema
}

func (s *Server) treeDBs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	id := r.FormValue("c")
	c, err := s.conn(ctx, id)
	if err != nil {
		s.fail(w, "tree-error", err)
		return
	}
	names, err := c.Databases(ctx)
	if err != nil {
		s.fail(w, "tree-error", err)
		return
	}
	var items []treeItem
	for _, n := range names {
		ref := Ref{Conn: id, DB: n}
		next := "/tree/groups?"
		kind := "db"
		if c.HasSchemas() {
			next = "/tree/schemas?"
		}
		items = append(items, treeItem{Ref: ref, Name: n, Children: next + ref.Query(), HasKids: true, Kind: kind})
	}
	s.render(w, "tree-items", items)
}

func (s *Server) treeSchemas(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "tree-error", err)
		return
	}
	names, err := c.Schemas(ctx, ref.DB)
	if err != nil {
		s.fail(w, "tree-error", err)
		return
	}
	var items []treeItem
	for _, n := range names {
		sr := Ref{Conn: ref.Conn, DB: ref.DB, Schema: n}
		items = append(items, treeItem{Ref: sr, Name: n, Children: "/tree/groups?" + sr.Query(), HasKids: true, Kind: "schema"})
	}
	s.render(w, "tree-items", items)
}

type groupData struct {
	Ref
	Tables      []db.Table
	Views       []db.Table
	HasRoutines bool
}

func (s *Server) treeGroups(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "tree-error", err)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	tabs, err := c.Tables(ctx, ref.dbRef())
	if err != nil {
		s.fail(w, "tree-error", err)
		return
	}
	g := groupData{Ref: ref, HasRoutines: c.HasRoutines()}
	for _, t := range tabs {
		if t.IsView {
			g.Views = append(g.Views, t)
		} else {
			g.Tables = append(g.Tables, t)
		}
	}
	s.render(w, "tree-groups", g)
}

// effectiveSchema keeps Schema empty for engines without schemas.
func effectiveSchema(c *db.Conn, r Ref) string {
	if !c.HasSchemas() {
		return ""
	}
	return r.Schema
}

// ---- objects --------------------------------------------------------------

func (s *Server) objects(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "objects", err)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	tabs, err := c.Tables(ctx, ref.dbRef())
	if err != nil {
		s.fail(w, "objects", err)
		return
	}
	s.render(w, "objects", map[string]any{"Ref": ref, "Tables": tabs, "Estimated": c.Config.Kind == db.Postgres, "HasRoutines": c.HasRoutines()})
}

// ---- table data -----------------------------------------------------------

type dataView struct {
	Ref
	Err      string
	Cols     []db.Column
	Rows     [][]db.Cell
	PKCols   []string
	Editable bool
	Sort     string
	Desc     bool
	Where    string
	Page     int
	Size     int
	Sizes    []int
	More     bool
	From, To int
	SQL      string
	IsView   bool
}

// PK returns the primary-key values of row as JSON, used to address the row on edit/delete.
func (v dataView) PK(row []db.Cell) string {
	pk := map[string]string{}
	for i, col := range v.Cols {
		if col.PK && i < len(row) {
			pk[col.Name] = row[i].V
		}
	}
	b, _ := json.Marshal(pk)
	return string(b)
}

func (s *Server) tableData(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	v := dataView{
		Ref:    ref,
		Sort:   r.FormValue("sort"),
		Desc:   r.FormValue("desc") == "1",
		Where:  strings.TrimSpace(r.FormValue("where")),
		Page:   atoiDefault(r.FormValue("page"), 1),
		Size:   atoiDefault(r.FormValue("size"), defaultSize),
		Sizes:  pageSizes,
		IsView: r.FormValue("v") == "1",
	}
	v.Page = max(v.Page, 1)
	if !validSize(v.Size) {
		v.Size = defaultSize
	}
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		v.Err = err.Error()
		s.render(w, "data", v)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	v.Ref = ref
	offset := (v.Page - 1) * v.Size
	page, err := c.TablePage(ctx, ref.dbRef(), db.PageOpts{
		Sort: v.Sort, Desc: v.Desc, Where: v.Where, Limit: v.Size, Offset: offset,
	})
	if err != nil {
		v.Err = err.Error()
		s.render(w, "data", v)
		return
	}
	v.Cols, v.Rows, v.More, v.SQL = page.Columns, page.Rows, page.More, page.SQL
	for _, col := range v.Cols {
		if col.PK {
			v.PKCols = append(v.PKCols, col.Name)
		}
	}
	v.Editable = len(v.PKCols) > 0 && !v.IsView
	if len(v.Rows) > 0 {
		v.From, v.To = offset+1, offset+len(v.Rows)
	}
	s.render(w, "data", v)
}

func (s *Server) tableCount(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		fmt.Fprint(w, "—")
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	n, err := c.Count(ctx, ref.dbRef(), r.FormValue("where"))
	if err != nil {
		fmt.Fprint(w, "—")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span data-total="%d">전체 %s행</span>`, n, fmtRows(n))
}

func parsePK(raw string) (map[string]string, error) {
	var pk map[string]string
	if err := json.Unmarshal([]byte(raw), &pk); err != nil {
		return nil, errors.New("잘못된 기본키 데이터")
	}
	return pk, nil
}

func (s *Server) tableCell(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	pk, err := parsePK(r.FormValue("pk"))
	if err == nil {
		var c *db.Conn
		if c, err = s.conn(ctx, ref.Conn); err == nil {
			ref.Schema = effectiveSchema(c, ref)
			val := db.Cell{V: r.FormValue("value"), Null: r.FormValue("null") == "1"}
			if err = c.UpdateCell(ctx, ref.dbRef(), pk, r.FormValue("col"), val); err == nil {
				s.render(w, "cell", val)
				return
			}
		}
	}
	toast(w, "error", err.Error())
	w.WriteHeader(http.StatusUnprocessableEntity)
}

func (s *Server) tableDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	pk, err := parsePK(r.FormValue("pk"))
	if err == nil {
		var c *db.Conn
		if c, err = s.conn(ctx, ref.Conn); err == nil {
			ref.Schema = effectiveSchema(c, ref)
			if err = c.DeleteRow(ctx, ref.dbRef(), pk); err == nil {
				toast(w, "ok", "1개 행을 삭제했습니다")
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}
	toast(w, "error", err.Error())
	w.WriteHeader(http.StatusUnprocessableEntity)
}

// ---- table design (read only) ---------------------------------------------

func (s *Server) tableDesign(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "design", err)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	cols, err := c.Columns(ctx, ref.dbRef())
	if err != nil {
		s.fail(w, "design", err)
		return
	}
	idx, err := c.Indexes(ctx, ref.dbRef())
	if err != nil {
		s.fail(w, "design", err)
		return
	}
	s.render(w, "design", map[string]any{"Ref": ref, "Cols": cols, "Indexes": idx})
}

// ---- query editor ---------------------------------------------------------

func (s *Server) queryPanel(w http.ResponseWriter, r *http.Request) {
	ref := refFrom(r)
	label := ref.DB
	if ref.Schema != "" {
		label += "." + ref.Schema
	}
	cfg, _ := s.store.Get(ref.Conn)
	s.render(w, "query", map[string]any{"Ref": ref, "Label": label, "ConnName": cfg.Name, "Kind": cfg.Kind, "HistoryOn": s.history != nil})
}

// querySchema feeds the editor's autocompletion: table -> columns as JSON.
func (s *Server) querySchema(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err == nil {
		ref.Schema = effectiveSchema(c, ref)
		var tables map[string][]string
		var truncated bool
		if tables, truncated, err = c.SchemaColumns(ctx, ref.dbRef()); err == nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]any{"tables": tables, "truncated": truncated})
			return
		}
	}
	http.Error(w, err.Error(), http.StatusBadGateway)
}

type resultItem struct {
	N       int
	SQL     string
	Err     string
	Res     *db.Result
	Elapsed string
	Label   string
}

func (s *Server) queryRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "result", err)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	started := time.Now()
	results, skipped, err := c.RunScript(ctx, ref.dbRef(), r.FormValue("sql"))
	if err != nil {
		s.fail(w, "result", err)
		return
	}
	ok := len(results) > 0 && results[len(results)-1].Err == nil
	s.recordHistory(ref, r.FormValue("sql"), ok, time.Since(started))
	var items []resultItem
	var total time.Duration
	active, queries := -1, 0
	for i, sr := range results {
		it := resultItem{N: i + 1, SQL: sr.SQL, Res: sr.Result}
		switch {
		case sr.Err != nil:
			it.Err = sr.Err.Error()
			it.Label = fmt.Sprintf("%d 오류", it.N)
			if active < 0 || items[active].Err == "" {
				active = i
			}
		case sr.Result.IsQuery:
			queries++
			it.Label = fmt.Sprintf("결과 %d", queries)
			total += sr.Result.Elapsed
			if active < 0 {
				active = i
			}
		default:
			it.Label = fmt.Sprintf("%d 실행", it.N)
			total += sr.Result.Elapsed
		}
		if sr.Result != nil {
			it.Elapsed = fmt.Sprintf("%.3f", sr.Result.Elapsed.Seconds())
		}
		items = append(items, it)
	}
	if active < 0 {
		active = len(items) - 1
	}
	s.render(w, "result", map[string]any{
		"Items":   items,
		"Multi":   len(items) > 1,
		"Active":  active,
		"Skipped": skipped,
		"Total":   fmt.Sprintf("%.3f", total.Seconds()),
		"Limit":   db.MaxQueryRows,
	})
}

// ---- connection dialog ----------------------------------------------------

type connForm struct {
	db.Config
	Kinds       []db.Kind
	Editing     bool
	HasPass     bool
	HasSSHPass  bool
	Err         string
	PortText    string
	SSHPortText string
	SSHOn       bool
	SSH         db.SSHConfig // never nil, so the template can read fields freely
}

func (s *Server) connDialog(w http.ResponseWriter, r *http.Request) {
	f := connForm{Kinds: []db.Kind{db.SQLite, db.Postgres, db.MySQL}}
	f.Kind = db.Postgres
	if id := r.FormValue("id"); id != "" {
		cfg, ok := s.store.Get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.HasPass = cfg.Password != ""
		cfg.Password = ""
		if cfg.SSH != nil {
			f.SSHOn, f.SSH = true, *cfg.SSH
			f.HasSSHPass = f.SSH.Password != ""
			f.SSH.Password = ""
			if f.SSH.Port > 0 {
				f.SSHPortText = strconv.Itoa(f.SSH.Port)
			}
		}
		f.Config, f.Editing = cfg, true
	}
	if f.Port > 0 {
		f.PortText = strconv.Itoa(f.Port)
	}
	s.render(w, "conn-dialog", f)
}

// cfgFromForm validates the dialog fields at the trust boundary.
func (s *Server) cfgFromForm(r *http.Request) (db.Config, error) {
	cfg := db.Config{
		ID:       r.FormValue("id"),
		Name:     strings.TrimSpace(r.FormValue("name")),
		Kind:     db.Kind(r.FormValue("kind")),
		Host:     strings.TrimSpace(r.FormValue("host")),
		User:     r.FormValue("user"),
		Password: r.FormValue("password"),
		Database: strings.TrimSpace(r.FormValue("database")),
	}
	if !cfg.Kind.Valid() {
		return cfg, errors.New("연결 유형을 선택하세요")
	}
	if cfg.Name == "" {
		return cfg, errors.New("연결 이름을 입력하세요")
	}
	if cfg.Kind == db.SQLite {
		if cfg.Database == "" {
			return cfg, errors.New("SQLite 파일 경로를 입력하세요")
		}
		cfg.Host, cfg.User, cfg.Password = "", "", ""
		return cfg, nil
	}
	if p := strings.TrimSpace(r.FormValue("port")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return cfg, errors.New("포트는 1–65535 사이의 숫자여야 합니다")
		}
		cfg.Port = n
	}
	cfg.SSLMode = r.FormValue("ssl_mode")
	if !db.ValidSSLMode(cfg.SSLMode) {
		return cfg, errors.New("알 수 없는 SSL 모드입니다")
	}
	cfg.SSLCA = strings.TrimSpace(r.FormValue("ssl_ca"))

	if r.FormValue("ssh_on") == "on" {
		ssh := &db.SSHConfig{
			Host:     strings.TrimSpace(r.FormValue("ssh_host")),
			User:     strings.TrimSpace(r.FormValue("ssh_user")),
			KeyFile:  strings.TrimSpace(r.FormValue("ssh_key")),
			Password: r.FormValue("ssh_password"),
		}
		if ssh.Host == "" || ssh.User == "" {
			return cfg, errors.New("SSH 호스트와 사용자 이름을 입력하세요")
		}
		if p := strings.TrimSpace(r.FormValue("ssh_port")); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return cfg, errors.New("SSH 포트는 1–65535 사이의 숫자여야 합니다")
			}
			ssh.Port = n
		}
		cfg.SSH = ssh
	}

	// Editing with empty secret fields keeps the stored ones.
	if cfg.ID != "" {
		if old, ok := s.store.Get(cfg.ID); ok {
			if cfg.Password == "" {
				cfg.Password = old.Password
			}
			if cfg.SSH != nil && cfg.SSH.Password == "" && old.SSH != nil {
				cfg.SSH.Password = old.SSH.Password
			}
		}
	}
	return cfg, nil
}

func (s *Server) connTest(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.cfgFromForm(r)
	if err != nil {
		s.render(w, "conn-test", map[string]any{"Err": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := db.Open(ctx, cfg)
	if err != nil {
		s.render(w, "conn-test", errData(err))
		return
	}
	c.Close()
	s.render(w, "conn-test", map[string]any{"OK": true})
}

func (s *Server) connSave(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.cfgFromForm(r)
	if err == nil {
		_, err = s.store.Save(cfg)
	}
	if err != nil {
		s.render(w, "conn-test", map[string]any{"Err": err.Error()})
		return
	}
	s.drop(cfg.ID) // apply changed settings on next use
	w.Header().Set("HX-Trigger", "conns-changed, close-modal")
}

// connTrustHost registers an SSH host key the user confirmed by fingerprint.
func (s *Server) connTrustHost(w http.ResponseWriter, r *http.Request) {
	if err := db.TrustHostKey(r.FormValue("host"), r.FormValue("key"), r.FormValue("fp")); err != nil {
		toast(w, "error", err.Error())
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	toast(w, "ok", "호스트 키를 known_hosts에 등록했습니다. 다시 연결하세요.")
}

func (s *Server) connDelete(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("c")
	s.drop(id)
	if s.history != nil {
		_ = s.history.Clear(id) // a deleted connection must not leave its queries behind
	}
	if err := s.store.Delete(id); err != nil {
		toast(w, "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "conns-changed")
}

func (s *Server) connClose(w http.ResponseWriter, r *http.Request) {
	s.drop(r.FormValue("c"))
	w.Header().Set("HX-Trigger", "conns-changed")
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func validSize(n int) bool {
	for _, s := range pageSizes {
		if s == n {
			return true
		}
	}
	return false
}
