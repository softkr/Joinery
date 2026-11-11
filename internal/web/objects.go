package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"dbcat/internal/db"
)

const (
	infoCountTimeout = 4 * time.Second
	erdMaxTables     = 200
	erdMaxColumns    = 6000
)

// ---- information pane (general / DDL) -------------------------------------

type infoView struct {
	Ref
	Err     string
	IsView  bool
	Kind    string
	Rows    string
	Cols    []db.Column
	PKs     string
	Indexes []db.Index
	FKs     []db.ForeignKey
	Comment string
	DDL     string
}

func (s *Server) objectInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "objinfo-general", err)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	tab := r.FormValue("tab")

	if kind := r.FormValue("kind"); kind != "" && kind != "table" && kind != "view" {
		name, data := s.objectKindInfo(ctx, c, ref, kind, r.FormValue("id"), tab)
		s.render(w, name, data)
		return
	}

	if tab == "ddl" {
		ddl, err := c.DDL(ctx, ref.dbRef())
		if err != nil {
			s.fail(w, "objinfo-ddl", err)
			return
		}
		s.render(w, "objinfo-ddl", infoView{Ref: ref, DDL: ddl})
		return
	}

	v := infoView{Ref: ref, Kind: c.Config.Kind.Label()}
	tabs, err := c.Tables(ctx, ref.dbRef())
	if err != nil {
		s.fail(w, "objinfo-general", err)
		return
	}
	for _, t := range tabs {
		if t.Name == ref.Table {
			v.IsView, v.Comment = t.IsView, t.Comment
		}
	}
	if v.Cols, err = c.Columns(ctx, ref.dbRef()); err != nil {
		s.fail(w, "objinfo-general", err)
		return
	}
	var pks []string
	for _, col := range v.Cols {
		if col.PK {
			pks = append(pks, col.Name)
		}
	}
	v.PKs = strings.Join(pks, ", ")
	v.Indexes, _ = c.Indexes(ctx, ref.dbRef())
	v.FKs, _ = c.ForeignKeys(ctx, ref.dbRef())
	v.Rows = "—"
	if !v.IsView {
		cctx, ccancel := context.WithTimeout(ctx, infoCountTimeout)
		if n, err := c.Count(cctx, ref.dbRef(), ""); err == nil {
			v.Rows = fmtRows(n)
		}
		ccancel()
	}
	s.render(w, "objinfo-general", v)
}

// ---- ER diagram data --------------------------------------------------------

type erdColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
	PK   bool   `json:"pk"`
	FK   bool   `json:"fk"`
}

type erdTable struct {
	Name    string      `json:"name"`
	View    bool        `json:"view"`
	Columns []erdColumn `json:"columns"`
}

type erdEdge struct {
	Name       string   `json:"name"`
	Table      string   `json:"table"`
	Columns    []string `json:"columns"`
	RefTable   string   `json:"refTable"`
	RefColumns []string `json:"refColumns"`
	OnDelete   string   `json:"onDelete"`
}

type erdData struct {
	Tables    []erdTable `json:"tables"`
	Edges     []erdEdge  `json:"edges"`
	Truncated bool       `json:"truncated"` // tables or columns were cut off
	Total     int        `json:"total"`
}

func (s *Server) erdData(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	tables, colsCut, err := c.SchemaColumnInfo(ctx, ref.dbRef(), erdMaxColumns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	fks, err := c.SchemaForeignKeys(ctx, ref.dbRef())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	scope := ref.Schema
	if scope == "" {
		scope = ref.DB
	}
	out := buildERD(tables, fks, scope, erdMaxTables)
	out.Truncated = out.Truncated || colsCut
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(out)
}

// buildERD keeps at most maxTables tables, preferring those with relations,
// and only the edges whose two ends are both kept.
func buildERD(tables []db.TableCols, fks []db.ForeignKey, scope string, maxTables int) erdData {
	// Only relations inside this schema/database are drawn.
	var local []db.ForeignKey
	for _, fk := range fks {
		if fk.RefSchema == "" || fk.RefSchema == scope {
			local = append(local, fk)
		}
	}
	fks = local
	degree := map[string]int{}
	for _, fk := range fks {
		degree[fk.Table]++
		degree[fk.RefTable]++
	}
	order := make([]db.TableCols, len(tables))
	copy(order, tables)
	if len(order) > maxTables {
		sort.SliceStable(order, func(i, j int) bool { return degree[order[i].Name] > degree[order[j].Name] })
	}
	out := erdData{Total: len(tables), Tables: []erdTable{}, Edges: []erdEdge{}}
	keep := map[string]bool{}
	for i, t := range order {
		if i >= maxTables {
			out.Truncated = true
			break
		}
		keep[t.Name] = true
	}
	fkCols := map[string]map[string]bool{}
	for _, fk := range fks {
		if fkCols[fk.Table] == nil {
			fkCols[fk.Table] = map[string]bool{}
		}
		for _, col := range fk.Columns {
			fkCols[fk.Table][col] = true
		}
	}
	for _, t := range tables { // keep the database's own table order in the output
		if !keep[t.Name] {
			continue
		}
		et := erdTable{Name: t.Name, View: t.IsView, Columns: []erdColumn{}}
		for _, col := range t.Columns {
			et.Columns = append(et.Columns, erdColumn{Name: col.Name, Type: col.Type, PK: col.PK, FK: fkCols[t.Name][col.Name]})
		}
		out.Tables = append(out.Tables, et)
	}
	for _, fk := range fks {
		if keep[fk.Table] && keep[fk.RefTable] {
			out.Edges = append(out.Edges, erdEdge{
				Name: fk.Name, Table: fk.Table, Columns: fk.Columns,
				RefTable: fk.RefTable, RefColumns: fk.RefColumns, OnDelete: fk.OnDelete,
			})
		}
	}
	return out
}
