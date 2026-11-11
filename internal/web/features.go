package web

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dbcat/internal/db"
)

const (
	maxFormRows   = 1024
	exportTimeout = 30 * time.Minute
)

// events sets several HX-Trigger events at once (ASCII-safe, see asciiJSON).
func events(w http.ResponseWriter, ev map[string]any) {
	b, _ := json.Marshal(ev)
	w.Header().Set("HX-Trigger", asciiJSON(b))
}

func toastEvent(kind, msg string) map[string]string {
	return map[string]string{"kind": kind, "msg": msg}
}

func refEvent(r Ref) map[string]string {
	return map[string]string{"c": r.Conn, "d": r.DB, "s": r.Schema, "t": r.Table}
}

func formInt(r *http.Request, key string) int {
	n := atoiDefault(r.FormValue(key), 0)
	return min(max(n, 0), maxFormRows)
}

// ---- insert row -----------------------------------------------------------

type insertField struct {
	Idx int
	db.Column
	Auto bool
}

type insertView struct {
	Ref
	Fields []insertField
	Err    string
}

func (s *Server) insertDialog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "insert-dialog", err)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	cols, err := c.Columns(ctx, ref.dbRef())
	if err != nil {
		s.fail(w, "insert-dialog", err)
		return
	}
	v := insertView{Ref: ref}
	for i, col := range cols {
		v.Fields = append(v.Fields, insertField{Idx: i, Column: col, Auto: c.AutoDefault(col)})
	}
	s.render(w, "insert-dialog", v)
}

func (s *Server) insertRow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "conn-test", err)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	var vals []db.InsertVal
	for i, n := 0, formInt(r, "n"); i < n; i++ {
		k := strconv.Itoa(i)
		switch r.FormValue("m." + k) {
		case "d": // database default: leave the column out
		case "n":
			vals = append(vals, db.InsertVal{Col: r.FormValue("col." + k), Cell: db.Cell{Null: true}})
		default:
			vals = append(vals, db.InsertVal{Col: r.FormValue("col." + k), Cell: db.Cell{V: r.FormValue("v." + k)}})
		}
	}
	if err := c.InsertRow(ctx, ref.dbRef(), vals); err != nil {
		s.render(w, "conn-test", map[string]any{"Err": err.Error()})
		return
	}
	events(w, map[string]any{
		"close-modal":  "",
		"rows-changed": refEvent(ref),
		"toast":        toastEvent("ok", "1개 행을 추가했습니다"),
	})
}

// ---- table designer -------------------------------------------------------

type fieldRow struct {
	I       string
	Orig    string
	Name    string
	Type    string
	Default string
	NotNull bool
	PK      bool
	Alter   bool // attributes of an existing column may be edited
	NewTbl  bool // whole table is new: primary key is editable
}

type indexRow struct {
	I       string
	Orig    string
	Name    string
	Cols    string
	Unique  bool
	Primary bool
}

type fkRow struct {
	I        string
	Existing bool // already in the database (SQLite ones may have no name)
	Orig     string
	Name     string
	Cols     string
	RefTable string
	RefCols  string
	OnDelete string
	OnUpdate string
	CanDrop  bool
}

type designerView struct {
	Ref
	UID     string
	New     bool
	Alter   bool
	FKAlter bool // foreign keys of an existing table can be added/dropped
	Fields  []fieldRow
	Indexes []indexRow
	FKs     []fkRow
	Tables  []string // for the referenced-table suggestions
	Actions []string
	Hints   []string
	Err     string
}

func newUID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var typeHints = map[db.Kind][]string{
	db.SQLite:   {"INTEGER", "TEXT", "REAL", "NUMERIC", "BLOB"},
	db.Postgres: {"integer", "bigint", "serial", "bigserial", "text", "varchar(255)", "boolean", "date", "timestamp", "timestamptz", "numeric(10,2)", "jsonb", "uuid"},
	db.MySQL:    {"int", "bigint", "int AUTO_INCREMENT", "varchar(255)", "text", "datetime", "timestamp", "decimal(10,2)", "tinyint(1)", "json", "date"},
}

func (s *Server) designerNew(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "designer", err)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	ref.Table = ""
	s.render(w, "designer", designerView{
		Ref: ref, UID: newUID(), New: true, Alter: true, FKAlter: true, Actions: db.FKActions[1:],
		Tables: s.tableNames(ctx, c, ref), Hints: typeHints[c.Config.Kind],
		Fields: []fieldRow{{I: "0", Name: "id", Type: typeHints[c.Config.Kind][0], NotNull: true, PK: true, Alter: true, NewTbl: true}},
	})
}

func (s *Server) designerEdit(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	v, err := s.loadDesigner(ctx, ref)
	if err != nil {
		s.fail(w, "designer", err)
		return
	}
	s.render(w, "designer", v)
}

func (s *Server) loadDesigner(ctx context.Context, ref Ref) (designerView, error) {
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		return designerView{}, err
	}
	ref.Schema = effectiveSchema(c, ref)
	cols, err := c.Columns(ctx, ref.dbRef())
	if err != nil {
		return designerView{}, err
	}
	idx, err := c.Indexes(ctx, ref.dbRef())
	if err != nil {
		return designerView{}, err
	}
	fks, err := c.ForeignKeys(ctx, ref.dbRef())
	if err != nil {
		return designerView{}, err
	}
	v := designerView{
		Ref: ref, UID: newUID(), Alter: c.CanAlterFields(), FKAlter: c.CanAlterFK(), Actions: db.FKActions[1:],
		Tables: s.tableNames(ctx, c, ref), Hints: typeHints[c.Config.Kind],
	}
	for i, fk := range fks {
		refTable := fk.RefTable
		if fk.RefSchema != "" && fk.RefSchema != ref.Schema && fk.RefSchema != ref.DB {
			refTable = fk.RefSchema + "." + fk.RefTable
		}
		v.FKs = append(v.FKs, fkRow{
			I: strconv.Itoa(i), Existing: true, Orig: fk.Name, Name: fk.Name, Cols: strings.Join(fk.Columns, ", "),
			RefTable: refTable, RefCols: strings.Join(fk.RefColumns, ", "),
			OnDelete: fk.OnDelete, OnUpdate: fk.OnUpdate, CanDrop: c.CanAlterFK() && fk.Name != "",
		})
	}
	for i, col := range cols {
		v.Fields = append(v.Fields, fieldRow{
			I: strconv.Itoa(i), Orig: col.Name, Name: col.Name, Type: col.Type, Default: col.Default,
			NotNull: !col.Nullable, PK: col.PK, Alter: v.Alter,
		})
	}
	for i, x := range idx {
		v.Indexes = append(v.Indexes, indexRow{I: strconv.Itoa(i), Orig: x.Name, Name: x.Name, Cols: x.Columns, Unique: x.Unique, Primary: x.Primary})
	}
	return v, nil
}

func parseFieldSpecs(r *http.Request) []db.FieldSpec {
	var out []db.FieldSpec
	for i, n := 0, formInt(r, "nf"); i < n; i++ {
		k := "f." + strconv.Itoa(i) + "."
		if _, present := r.Form[k+"orig"]; !present {
			continue // row removed in the browser
		}
		out = append(out, db.FieldSpec{
			Orig:    r.FormValue(k + "orig"),
			Name:    strings.TrimSpace(r.FormValue(k + "name")),
			Type:    strings.TrimSpace(r.FormValue(k + "type")),
			Default: strings.TrimSpace(r.FormValue(k + "default")),
			NotNull: r.FormValue(k+"notnull") == "on",
			PK:      r.FormValue(k+"pk") == "on",
			Drop:    r.FormValue(k+"drop") == "on",
		})
	}
	return out
}

// tableNames lists the tables of the schema for the foreign-key target suggestions.
func (s *Server) tableNames(ctx context.Context, c *db.Conn, ref Ref) []string {
	tabs, err := c.Tables(ctx, ref.dbRef())
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(tabs))
	for _, t := range tabs {
		if !t.IsView {
			names = append(names, t.Name)
		}
	}
	return names
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseFKSpecs(r *http.Request) []db.FKSpec {
	var out []db.FKSpec
	for i, n := 0, formInt(r, "nk"); i < n; i++ {
		k := "k." + strconv.Itoa(i) + "."
		if _, present := r.Form[k+"orig"]; !present {
			continue
		}
		if r.FormValue(k+"existing") == "1" && r.FormValue(k+"drop") != "on" {
			continue // untouched existing constraint
		}
		out = append(out, db.FKSpec{
			Orig: r.FormValue(k + "orig"), Name: strings.TrimSpace(r.FormValue(k + "name")),
			Columns: splitList(r.FormValue(k + "cols")), RefTable: strings.TrimSpace(r.FormValue(k + "rtable")),
			RefColumns: splitList(r.FormValue(k + "rcols")), OnDelete: r.FormValue(k + "ondelete"),
			OnUpdate: r.FormValue(k + "onupdate"), Drop: r.FormValue(k+"drop") == "on",
		})
	}
	return out
}

func parseIndexSpecs(r *http.Request) []db.IndexSpec {
	var out []db.IndexSpec
	for j, n := 0, formInt(r, "nx"); j < n; j++ {
		k := "x." + strconv.Itoa(j) + "."
		if _, present := r.Form[k+"orig"]; !present {
			continue
		}
		var cols []string
		for _, p := range strings.Split(r.FormValue(k+"cols"), ",") {
			if p = strings.TrimSpace(p); p != "" {
				cols = append(cols, p)
			}
		}
		out = append(out, db.IndexSpec{
			Orig: r.FormValue(k + "orig"), Name: strings.TrimSpace(r.FormValue(k + "name")),
			Columns: cols, Unique: r.FormValue(k+"unique") == "on", Drop: r.FormValue(k+"drop") == "on",
		})
	}
	return out
}

// plan turns the posted designer form into DDL statements.
func (s *Server) plan(ctx context.Context, r *http.Request) (c *db.Conn, ref Ref, stmts []string, err error) {
	ref = refFrom(r)
	isNew := r.FormValue("mode") == "new"
	if c, err = s.conn(ctx, ref.Conn); err != nil {
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	fields, idx, fks := parseFieldSpecs(r), parseIndexSpecs(r), parseFKSpecs(r)
	if isNew {
		stmts, err = c.PlanCreate(ref.dbRef(), fields, idx, fks)
		return
	}
	// Browser-disabled inputs are not submitted: take the live values for
	// everything the designer is not allowed to change on an existing column.
	cur, err := c.Columns(ctx, ref.dbRef())
	if err != nil {
		return
	}
	for i, f := range fields {
		if f.Orig == "" {
			continue
		}
		for _, col := range cur {
			if col.Name != f.Orig {
				continue
			}
			fields[i].PK = col.PK
			if !c.CanAlterFields() {
				fields[i].Type, fields[i].NotNull, fields[i].Default = col.Type, !col.Nullable, col.Default
			}
		}
	}
	stmts, err = c.PlanAlter(ctx, ref.dbRef(), fields, idx, fks)
	return
}

func (s *Server) designPreview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	_, _, stmts, err := s.plan(ctx, r)
	if r.FormValue("fmt") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, err.Error())
			return
		}
		fmt.Fprint(w, joinSQL(stmts))
		return
	}
	data := map[string]any{"SQL": joinSQL(stmts)}
	if err != nil {
		data["Err"] = err.Error()
	}
	s.render(w, "sqlpreview", data)
}

func joinSQL(stmts []string) string {
	if len(stmts) == 0 {
		return ""
	}
	return strings.Join(stmts, ";\n\n") + ";"
}

var uidRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

func (s *Server) designApply(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	// Errors are shown inside the form instead of replacing the whole panel.
	fail := func(err error) {
		if uid := r.FormValue("uid"); uidRe.MatchString(uid) {
			w.Header().Set("HX-Retarget", "#dm-"+uid)
			w.Header().Set("HX-Reswap", "innerHTML")
		}
		s.render(w, "conn-test", map[string]any{"Err": err.Error()})
	}
	c, ref, stmts, err := s.plan(ctx, r)
	if err != nil {
		fail(err)
		return
	}
	if len(stmts) == 0 {
		w.Header().Set("HX-Reswap", "none")
		events(w, map[string]any{"toast": toastEvent("ok", "변경 사항이 없습니다")})
		return
	}
	applied, err := c.ExecDDL(ctx, ref.dbRef(), stmts)
	if err != nil {
		if applied > 0 {
			err = fmt.Errorf("%w\n(앞선 %d개 문은 이미 적용되었습니다)", err, applied)
			// Applied statements changed the schema: tell the page to refresh.
			events(w, map[string]any{"schema-changed": refEvent(ref)})
		}
		fail(err)
		return
	}
	events(w, map[string]any{
		"schema-changed": refEvent(ref),
		"rows-changed":   refEvent(ref),
		"toast":          toastEvent("ok", fmt.Sprintf("%d개 문을 적용했습니다", len(stmts))),
	})
	v, err := s.loadDesigner(ctx, ref)
	if err != nil {
		s.fail(w, "designer", err)
		return
	}
	s.render(w, "designer", v)
}

// ---- export ---------------------------------------------------------------

type exportReq struct {
	ref    Ref
	opts   db.PageOpts
	format string
}

func parseExport(q url.Values) (exportReq, error) {
	e := exportReq{
		ref:    Ref{Conn: q.Get("c"), DB: q.Get("d"), Schema: q.Get("s"), Table: q.Get("t")},
		opts:   db.PageOpts{Where: q.Get("where"), Sort: q.Get("sort"), Desc: q.Get("desc") == "1"},
		format: q.Get("fmt"),
	}
	if e.format != "csv" && e.format != "json" {
		return e, errors.New("fmt must be csv or json")
	}
	if e.ref.Table == "" {
		return e, errors.New("missing table")
	}
	return e, nil
}

// ExportName is the suggested file name for an export request.
func (s *Server) ExportName(rawQuery string) string {
	q, _ := url.ParseQuery(rawQuery)
	e, err := parseExport(q)
	if err != nil {
		return "export"
	}
	return e.ref.Table + "." + e.format
}

// ExportTo streams the table (all rows matching the filter) to w as CSV or JSON.
func (s *Server) ExportTo(ctx context.Context, rawQuery string, w io.Writer) error {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return err
	}
	e, err := parseExport(q)
	if err != nil {
		return err
	}
	c, err := s.conn(ctx, e.ref.Conn)
	if err != nil {
		return err
	}
	e.ref.Schema = effectiveSchema(c, e.ref)
	bw := bufio.NewWriterSize(w, 64<<10)
	var emit func([]string, []db.Cell) error
	var finish func() error
	if e.format == "csv" {
		emit, finish = csvEmitter(bw)
	} else {
		emit, finish = jsonEmitter(bw)
	}
	if err := c.ExportTable(ctx, e.ref.dbRef(), e.opts, emit); err != nil {
		return err
	}
	if err := finish(); err != nil {
		return err
	}
	return bw.Flush()
}

// csvEmitter writes UTF-8 with a BOM so Excel detects the encoding. NULL
// becomes an empty field (use JSON to keep the distinction).
func csvEmitter(w io.Writer) (func([]string, []db.Cell) error, func() error) {
	cw := csv.NewWriter(w)
	started := false
	emit := func(cols []string, row []db.Cell) error {
		if !started {
			started = true
			if _, err := io.WriteString(w, "\xEF\xBB\xBF"); err != nil {
				return err
			}
		}
		if row == nil {
			return cw.Write(cols)
		}
		rec := make([]string, len(row))
		for i, cell := range row {
			if !cell.Null {
				rec[i] = cell.V
			}
		}
		return cw.Write(rec)
	}
	return emit, func() error { cw.Flush(); return cw.Error() }
}

func jsonString(s string) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(sb.String(), "\n")
}

// jsonEmitter writes an array of objects; numbers/booleans stay unquoted and
// NULL is null.
func jsonEmitter(w io.Writer) (func([]string, []db.Cell) error, func() error) {
	var keys []string
	n := 0
	emit := func(cols []string, row []db.Cell) error {
		if row == nil {
			keys = make([]string, len(cols))
			for i, c := range cols {
				keys[i] = jsonString(c)
			}
			_, err := io.WriteString(w, "[")
			return err
		}
		var sb strings.Builder
		if n > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("\n  {")
		for i, cell := range row {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(keys[i] + ": ")
			switch {
			case cell.Null:
				sb.WriteString("null")
			case cell.Lit:
				sb.WriteString(cell.V)
			default:
				sb.WriteString(jsonString(cell.V))
			}
		}
		sb.WriteString("}")
		n++
		_, err := io.WriteString(w, sb.String())
		return err
	}
	return emit, func() error { _, err := io.WriteString(w, "\n]\n"); return err }
}

// headerWriter sets the download headers on the first byte so an error before
// any output can still become a proper HTTP error.
type headerWriter struct {
	w       http.ResponseWriter
	ctype   string
	name    string
	written bool
}

func (h *headerWriter) Write(p []byte) (int, error) {
	if !h.written {
		h.written = true
		h.w.Header().Set("Content-Type", h.ctype)
		h.w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": h.name}))
	}
	return h.w.Write(p)
}

func (s *Server) tableExport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), exportTimeout)
	defer cancel()
	ctype := "text/csv; charset=utf-8"
	if r.FormValue("fmt") == "json" {
		ctype = "application/json; charset=utf-8"
	}
	hw := &headerWriter{w: w, ctype: ctype, name: s.ExportName(r.URL.RawQuery)}
	if err := s.ExportTo(ctx, r.URL.RawQuery, hw); err != nil {
		if !hw.written {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Output already started (more than the 64 KiB buffer was sent): the
		// status line is gone, so all we can do is stop and log. Panicking with
		// http.ErrAbortHandler would crash the embedded Wails server instead.
		log.Printf("export %q aborted: %v", r.URL.RawQuery, err)
	}
}
