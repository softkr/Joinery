package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type sqliteDialect struct{}

func (sqliteDialect) open(cfg Config, _ string) (*sql.DB, error) {
	if strings.TrimSpace(cfg.Database) == "" {
		return nil, errors.New("SQLite 파일 경로가 필요합니다")
	}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	return sql.Open("sqlite", "file:"+(&url.URL{Path: cfg.Database}).EscapedPath()+"?"+q.Encode())
}

func (sqliteDialect) poolKey(string) string                           { return "" }
func (sqliteDialect) hasSchemas() bool                                { return false }
func (sqliteDialect) defaultDatabase(Config) string                   { return "main" }
func (sqliteDialect) quote(s string) string                           { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (sqliteDialect) placeholder(int) string                          { return "?" }
func (d sqliteDialect) qualified(r Ref) string                        { return d.quote(r.Table) }
func (sqliteDialect) useSchema(context.Context, *sql.Conn, Ref) error { return nil }
func (sqliteDialect) databases(context.Context, *sql.DB) ([]string, error) {
	return []string{"main"}, nil
}
func (sqliteDialect) schemas(context.Context, *sql.DB, string) ([]string, error) { return nil, nil }

// sqliteCountBudget bounds the time spent counting rows for the table list:
// SQLite keeps no row statistics, so counts are exact but only filled in while
// the budget lasts (the rest stay unknown).
const sqliteCountBudget = 500 * time.Millisecond

func (d sqliteDialect) tables(ctx context.Context, p *sql.DB, _ Ref) ([]Table, error) {
	rows, err := p.QueryContext(ctx, `SELECT name, type FROM sqlite_master
		WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []Table
	for rows.Next() {
		var t Table
		var typ string
		if err := rows.Scan(&t.Name, &typ); err != nil {
			rows.Close()
			return nil, err
		}
		t.IsView = typ == "view"
		t.Rows = -1
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(sqliteCountBudget)
	for i := range out {
		if out[i].IsView || time.Now().After(deadline) {
			continue
		}
		cctx, cancel := context.WithDeadline(ctx, deadline)
		var n int64
		if err := p.QueryRowContext(cctx, "SELECT count(*) FROM "+d.quote(out[i].Name)).Scan(&n); err == nil {
			out[i].Rows = n
		}
		cancel()
	}
	return out, nil
}

func (sqliteDialect) schemaColumns(ctx context.Context, p *sql.DB, _ Ref, limit int) (map[string][]string, error) {
	rows, err := p.QueryContext(ctx, `SELECT m.name, p.name
		FROM sqlite_master m, pragma_table_info(m.name) p
		WHERE m.type IN ('table','view') AND m.name NOT LIKE 'sqlite_%'
		ORDER BY m.name, p.cid`)
	if err != nil {
		return nil, err
	}
	return collectColumns(rows, limit)
}

func (sqliteDialect) foreignKeys(ctx context.Context, p *sql.DB, r Ref, all bool) ([]ForeignKey, error) {
	if all {
		r.Table = ""
	}
	rows, err := p.QueryContext(ctx, `SELECT m.name, f.id, f."table", f."from", f."to", f.on_update, f.on_delete
		FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND (? = '' OR m.name = ?)
		ORDER BY m.name, f.id, f.seq`, r.Table, r.Table)
	if err != nil {
		return nil, err
	}
	fks, err := scanForeignKeys(rows, func() (fkKey, string, string, ForeignKey, error) {
		var table, refTable, from string
		var id int
		var to, onUpdate, onDelete sql.NullString
		if err := rows.Scan(&table, &id, &refTable, &from, &to, &onUpdate, &onDelete); err != nil {
			return fkKey{}, "", "", ForeignKey{}, err
		}
		return fkKey{table, fmt.Sprint(id)}, from, to.String,
			ForeignKey{RefTable: refTable, OnUpdate: onUpdate.String, OnDelete: onDelete.String}, nil
	})
	// The numeric id only grouped the columns; SQLite constraints have no name.
	for i := range fks {
		fks[i].Name = ""
	}
	return fks, err
}

func (d sqliteDialect) columns(ctx context.Context, p *sql.DB, r Ref) ([]Column, error) {
	rows, err := p.QueryContext(ctx, "PRAGMA table_xinfo("+d.quote(r.Table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		var cid, notnull, pk, hidden int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk, &hidden); err != nil {
			return nil, err
		}
		if hidden == 1 { // hidden virtual-table column
			continue
		}
		c := Column{Name: name, Type: typ, Nullable: notnull == 0, Default: dflt.String, PK: pk > 0}
		if hidden == 2 || hidden == 3 {
			c.Extra = "generated"
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d sqliteDialect) indexes(ctx context.Context, p *sql.DB, r Ref) ([]Index, error) {
	rows, err := p.QueryContext(ctx, "PRAGMA index_list("+d.quote(r.Table)+")")
	if err != nil {
		return nil, err
	}
	var out []Index
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, Index{Name: name, Unique: unique == 1, Primary: origin == "pk"})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		cr, err := p.QueryContext(ctx, "PRAGMA index_info("+d.quote(out[i].Name)+")")
		if err != nil {
			return nil, err
		}
		var cols []string
		for cr.Next() {
			var seqno, cid int
			var name sql.NullString
			if err := cr.Scan(&seqno, &cid, &name); err != nil {
				cr.Close()
				return nil, err
			}
			cols = append(cols, name.String)
		}
		cr.Close()
		out[i].Columns = strings.Join(cols, ", ")
	}
	return out, nil
}
