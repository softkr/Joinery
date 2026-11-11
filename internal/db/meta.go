package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// TableCols is one table or view with its columns, for the ER diagram.
type TableCols struct {
	Name    string
	IsView  bool
	Rows    int64 // -1 when unknown
	Columns []Column
}

// SchemaColumnInfo returns every table and view of r's database/schema with
// columns (type, primary key flag), using one query per engine. At most
// limitCols columns are read; truncated reports that the limit was hit.
func (c *Conn) SchemaColumnInfo(ctx context.Context, r Ref, limitCols int) (tables []TableCols, truncated bool, err error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, false, err
	}
	list, err := c.d.tables(ctx, p, r)
	if err != nil {
		return nil, false, err
	}
	byName := make(map[string]*TableCols, len(list))
	tables = make([]TableCols, len(list))
	for i, t := range list {
		tables[i] = TableCols{Name: t.Name, IsView: t.IsView, Rows: t.Rows}
		byName[t.Name] = &tables[i]
	}
	rows, err := c.d.schemaColumnInfo(ctx, p, r, limitCols+1)
	if err != nil {
		return nil, false, err
	}
	n := 0
	for _, rc := range rows {
		if n++; n > limitCols {
			truncated = true
			break
		}
		if t := byName[rc.table]; t != nil {
			t.Columns = append(t.Columns, rc.col)
		}
	}
	return tables, truncated, nil
}

type tableColumn struct {
	table string
	col   Column
}

func scanTableColumns(rows *sql.Rows, limit int) ([]tableColumn, error) {
	defer rows.Close()
	var out []tableColumn
	for rows.Next() {
		var tc tableColumn
		if err := rows.Scan(&tc.table, &tc.col.Name, &tc.col.Type, &tc.col.PK); err != nil {
			return nil, err
		}
		out = append(out, tc)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// DDL returns the CREATE statement(s) of a table or view. SQLite and MySQL
// report what the server stores; PostgreSQL has no single statement for it, so
// the definition is rebuilt from the catalog (columns, constraints, indexes).
func (c *Conn) DDL(ctx context.Context, r Ref) (string, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return "", err
	}
	return c.d.ddl(ctx, p, r)
}

func (sqliteDialect) schemaColumnInfo(ctx context.Context, p *sql.DB, _ Ref, limit int) ([]tableColumn, error) {
	rows, err := p.QueryContext(ctx, `SELECT m.name, p.name, p.type, p.pk > 0
		FROM sqlite_master m, pragma_table_info(m.name) p
		WHERE m.type IN ('table','view') AND m.name NOT LIKE 'sqlite_%'
		ORDER BY m.name, p.cid`)
	if err != nil {
		return nil, err
	}
	return scanTableColumns(rows, limit)
}

func (sqliteDialect) ddl(ctx context.Context, p *sql.DB, r Ref) (string, error) {
	rows, err := p.QueryContext(ctx, `SELECT sql FROM sqlite_master
		WHERE tbl_name = ? AND sql IS NOT NULL
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'view' THEN 0 WHEN 'index' THEN 1 ELSE 2 END, name`, r.Table)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
		parts = append(parts, strings.TrimSpace(s)+";")
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("%s: 정의를 찾을 수 없습니다", r.Table)
	}
	return strings.Join(parts, "\n\n"), nil
}

func (mysqlDialect) schemaColumnInfo(ctx context.Context, p *sql.DB, r Ref, limit int) ([]tableColumn, error) {
	rows, err := p.QueryContext(ctx, `SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, COLUMN_KEY = 'PRI'
		FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME, ORDINAL_POSITION`, r.Database)
	if err != nil {
		return nil, err
	}
	return scanTableColumns(rows, limit)
}

func (d mysqlDialect) ddl(ctx context.Context, p *sql.DB, r Ref) (string, error) {
	// SHOW CREATE TABLE also works for views; the statement is the 2nd column.
	rows, err := p.QueryContext(ctx, "SHOW CREATE TABLE "+d.qualified(r))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if !rows.Next() {
		return "", fmt.Errorf("%s: 정의를 찾을 수 없습니다", r.Table)
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}
	if len(vals) < 2 {
		return "", fmt.Errorf("%s: 예상하지 못한 SHOW CREATE 결과", r.Table)
	}
	return strings.TrimSpace(string(vals[1])) + ";", nil
}

func (postgresDialect) schemaColumnInfo(ctx context.Context, p *sql.DB, r Ref, limit int) ([]tableColumn, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod),
		       EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = a.attrelid
		               AND i.indisprimary AND a.attnum = ANY(i.indkey))
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','f') AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY c.relname, a.attnum`, r.Schema)
	if err != nil {
		return nil, err
	}
	return scanTableColumns(rows, limit)
}

func (d postgresDialect) ddl(ctx context.Context, p *sql.DB, r Ref) (string, error) {
	var kind string
	var oid uint32
	err := p.QueryRowContext(ctx, `SELECT c.relkind::text, c.oid::bigint FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2`, r.Schema, r.Table).Scan(&kind, &oid)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("%s: 정의를 찾을 수 없습니다", r.Table)
		}
		return "", err
	}
	qt := d.qualified(r)
	if kind == "v" || kind == "m" {
		var def string
		if err := p.QueryRowContext(ctx, "SELECT pg_get_viewdef($1::oid, true)", oid).Scan(&def); err != nil {
			return "", err
		}
		kw := "CREATE VIEW "
		if kind == "m" {
			kw = "CREATE MATERIALIZED VIEW "
		}
		return kw + qt + " AS\n" + strings.TrimSpace(def), nil
	}

	cols, err := d.columns(ctx, p, r)
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(cols)+4)
	for _, c := range cols {
		l := "  " + d.quote(c.Name) + " " + c.Type
		if !c.Nullable {
			l += " NOT NULL"
		}
		if c.Default != "" {
			l += " DEFAULT " + c.Default
		}
		lines = append(lines, l)
	}
	crows, err := p.QueryContext(ctx, `SELECT conname, pg_get_constraintdef(oid, true) FROM pg_constraint
		WHERE conrelid = $1::oid AND contype IN ('p','u','f','c') ORDER BY contype = 'p' DESC, contype, conname`, oid)
	if err != nil {
		return "", err
	}
	var consNames []string
	for crows.Next() {
		var name, def string
		if err := crows.Scan(&name, &def); err != nil {
			crows.Close()
			return "", err
		}
		consNames = append(consNames, name)
		lines = append(lines, "  CONSTRAINT "+d.quote(name)+" "+def)
	}
	crows.Close()
	out := "CREATE TABLE " + qt + " (\n" + strings.Join(lines, ",\n") + "\n);"

	irows, err := p.QueryContext(ctx, `SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = $1 AND tablename = $2 ORDER BY indexname`, r.Schema, r.Table)
	if err != nil {
		return "", err
	}
	defer irows.Close()
	skip := map[string]bool{}
	for _, n := range consNames {
		skip[n] = true // indexes that back a constraint are part of it
	}
	for irows.Next() {
		var name, def string
		if err := irows.Scan(&name, &def); err != nil {
			return "", err
		}
		if !skip[name] {
			out += "\n\n" + def + ";"
		}
	}
	return out, irows.Err()
}
