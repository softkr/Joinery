package db

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// DumpOptions selects what a backup contains.
type DumpOptions struct {
	Tables    []string // tables to include; empty means all tables
	NoTables  bool     // include no tables at all (views/objects only)
	Views     bool     // include all views
	Structure bool     // CREATE statements (tables, indexes, foreign keys, views)
	Data      bool     // INSERT statements
	Drop      bool     // DROP ... IF EXISTS before creating (only with Structure)
	Objects   bool     // triggers and, where supported, functions/procedures
}

const dumpBatch = 100

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Dump writes a portable SQL script that recreates the selected objects. It is
// produced without pg_dump/mysqldump: values are rendered as literals by the
// server itself (quote() / quote_nullable() / QUOTE()), so escaping, NULLs and
// binary data do not depend on Go-side formatting.
func (c *Conn) Dump(ctx context.Context, r Ref, o DumpOptions, w io.Writer) error {
	if !o.Structure && !o.Data {
		return errors.New("구조 또는 데이터 중 하나는 선택해야 합니다")
	}
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return err
	}
	list, err := c.d.tables(ctx, p, r)
	if err != nil {
		return err
	}
	isTable := map[string]bool{}
	var allTables, views []string
	for _, t := range list {
		if t.IsView {
			views = append(views, t.Name)
		} else {
			isTable[t.Name] = true
			allTables = append(allTables, t.Name)
		}
	}
	tables := allTables
	if o.NoTables {
		tables = nil
	} else if len(o.Tables) > 0 {
		tables = nil
		want := map[string]bool{}
		for _, n := range o.Tables {
			if !isTable[n] {
				return fmt.Errorf("알 수 없는 테이블: %q", n)
			}
			want[n] = true
		}
		for _, n := range allTables { // keep the database order
			if want[n] {
				tables = append(tables, n)
			}
		}
	}
	if !o.Views {
		views = nil
	}

	bw := bufio.NewWriterSize(w, 64<<10)
	d := &dumper{c: c, ctx: ctx, r: r, o: o, w: bw, tables: tables, views: views}
	fmt.Fprintf(bw, "-- DBCat dump · %s · %s · %s\n-- 구조: %v · 데이터: %v · 테이블 %d개 · 뷰 %d개\n\n",
		c.Config.Kind.Label(), orDefault(r.Schema, r.Database), time.Now().Format("2006-01-02 15:04:05"), o.Structure, o.Data, len(tables), len(views))
	switch c.Config.Kind {
	case SQLite:
		err = d.sqlite(p)
	case MySQL:
		err = d.mysql(p)
	case Postgres:
		err = d.postgres()
	default:
		err = fmt.Errorf("지원하지 않는 DB: %s", c.Config.Kind)
	}
	if err != nil {
		return err
	}
	return bw.Flush()
}

func orDefault(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type dumper struct {
	c      *Conn
	ctx    context.Context
	r      Ref
	o      DumpOptions
	w      *bufio.Writer
	tables []string
	views  []string

	override string // PostgreSQL: " OVERRIDING SYSTEM VALUE" while loading a table with GENERATED ALWAYS identity columns
}

func (d *dumper) printf(format string, args ...any) { fmt.Fprintf(d.w, format, args...) }

func (d *dumper) quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = d.c.d.quote(n)
	}
	return strings.Join(q, ", ")
}

// data streams INSERT statements. exprs render one literal per column on the server.
func (d *dumper) data(q queryer, target string, cols, exprs []string, from string) error {
	if len(cols) == 0 {
		return nil
	}
	rows, err := q.QueryContext(d.ctx, "SELECT "+strings.Join(exprs, ", ")+" FROM "+from)
	if err != nil {
		return fmt.Errorf("%s 데이터를 읽을 수 없습니다: %w", target, err)
	}
	defer rows.Close()
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	head := "INSERT INTO " + target + " (" + d.quoteList(cols) + ")" + d.override + " VALUES\n"
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if n%dumpBatch == 0 {
			if n > 0 {
				d.printf(";\n")
			}
			d.printf("%s", head)
		} else {
			d.printf(",\n")
		}
		d.printf("(")
		for i, v := range vals {
			if i > 0 {
				d.printf(", ")
			}
			d.w.Write(v)
		}
		d.printf(")")
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n > 0 {
		d.printf(";\n")
	}
	return nil
}

// dataColumns lists the columns to insert (generated columns are skipped).
func (d *dumper) dataColumns(p *sql.DB, table string) ([]Column, error) {
	cols, err := d.c.d.columns(d.ctx, p, Ref{Database: d.r.Database, Schema: d.r.Schema, Table: table})
	if err != nil {
		return nil, err
	}
	var out []Column
	for _, c := range cols {
		if !strings.Contains(strings.ToUpper(c.Extra), "GENERATED") {
			out = append(out, c)
		}
	}
	return out, nil
}

func names(cols []Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// ---- SQLite -------------------------------------------------------------------------

func (d *dumper) sqlite(p *sql.DB) error {
	q := d.c.d.quote
	d.printf("PRAGMA foreign_keys=OFF;\nBEGIN;\n\n")
	if d.o.Structure && d.o.Drop {
		for i := len(d.views) - 1; i >= 0; i-- {
			d.printf("DROP VIEW IF EXISTS %s;\n", q(d.views[i]))
		}
		for _, t := range d.tables {
			d.printf("DROP TABLE IF EXISTS %s;\n", q(t))
		}
		d.printf("\n")
	}
	for _, t := range d.tables {
		if d.o.Structure {
			var def sql.NullString
			if err := p.QueryRowContext(d.ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, t).Scan(&def); err != nil {
				return fmt.Errorf("%s 정의를 읽을 수 없습니다: %w", t, err)
			}
			d.printf("%s;\n", strings.TrimSpace(def.String))
		}
		if d.o.Data {
			cols, err := d.dataColumns(p, t)
			if err != nil {
				return err
			}
			exprs := make([]string, len(cols))
			for i, c := range cols {
				exprs[i] = "quote(" + q(c.Name) + ")"
			}
			if err := d.data(p, q(t), names(cols), exprs, q(t)); err != nil {
				return err
			}
		}
		d.printf("\n")
	}
	if d.o.Structure {
		scope := append(append([]string{}, d.tables...), d.views...)
		// indexes after the data load, then views, then triggers
		for _, kind := range []string{"index", "view", "trigger"} {
			if kind == "trigger" && !d.o.Objects {
				continue
			}
			rows, err := p.QueryContext(d.ctx, `SELECT tbl_name, name, sql FROM sqlite_master WHERE type = ? AND sql IS NOT NULL ORDER BY rowid`, kind)
			if err != nil {
				return err
			}
			var stmts []string
			for rows.Next() {
				var tbl, name string
				var def sql.NullString
				if err := rows.Scan(&tbl, &name, &def); err != nil {
					rows.Close()
					return err
				}
				include := false
				if kind == "view" {
					include = contains(d.views, name)
				} else {
					include = contains(scope, tbl)
				}
				if include {
					stmts = append(stmts, strings.TrimSpace(def.String)+";")
				}
			}
			rows.Close()
			for _, s := range stmts {
				d.printf("%s\n", s)
			}
			if len(stmts) > 0 {
				d.printf("\n")
			}
		}
	}
	d.printf("COMMIT;\nPRAGMA foreign_keys=ON;\n")
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- MySQL / MariaDB ------------------------------------------------------------------

var definerRe = regexp.MustCompile("(?i)\\sDEFINER\\s*=\\s*(?:CURRENT_USER(?:\\(\\))?|(?:`[^`]*`|'[^']*'|\"[^\"]*\"|[^\\s@]+)@(?:`[^`]*`|'[^']*'|\"[^\"]*\"|[^\\s]+))")

// stripDefiner removes DEFINER=... so a dump can be restored by another account.
func stripDefiner(s string) string { return definerRe.ReplaceAllString(s, "") }

// mysqlValueExpr renders one column as a literal on the server.
func mysqlValueExpr(col Column) string {
	q := "`" + strings.ReplaceAll(col.Name, "`", "``") + "`"
	base := strings.ToLower(col.Type)
	if i := strings.IndexAny(base, "( "); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "blob", "tinyblob", "mediumblob", "longblob", "binary", "varbinary":
		return "IF(" + q + " IS NULL, 'NULL', CONCAT('0x', HEX(" + q + ")))"
	case "bit":
		return "IF(" + q + " IS NULL, 'NULL', CONCAT('b''', BIN(" + q + " + 0), ''''))"
	}
	return "QUOTE(" + q + ")"
}

func (d *dumper) mysql(p *sql.DB) error {
	q := d.c.d.quote
	d.printf("SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS=0;\nSET SQL_MODE='NO_AUTO_VALUE_ON_ZERO';\n\n")
	for _, t := range d.tables {
		if d.o.Structure {
			if d.o.Drop {
				d.printf("DROP TABLE IF EXISTS %s;\n", q(t))
			}
			ddl, err := d.c.d.ddl(d.ctx, p, Ref{Database: d.r.Database, Table: t})
			if err != nil {
				return err
			}
			d.printf("%s\n", ddl)
		}
		if d.o.Data {
			cols, err := d.dataColumns(p, t)
			if err != nil {
				return err
			}
			exprs := make([]string, len(cols))
			for i, c := range cols {
				exprs[i] = mysqlValueExpr(c)
			}
			if err := d.data(p, q(t), names(cols), exprs, q(d.r.Database)+"."+q(t)); err != nil {
				return err
			}
		}
		d.printf("\n")
	}
	if d.o.Structure {
		for _, v := range d.views {
			if d.o.Drop {
				d.printf("DROP VIEW IF EXISTS %s;\n", q(v))
			}
			ddl, err := d.c.d.ddl(d.ctx, p, Ref{Database: d.r.Database, Table: v})
			if err != nil {
				return err
			}
			// SHOW CREATE VIEW qualifies every name with the source database; drop
			// that so the view is created in the database being restored into.
			d.printf("%s\n\n", strings.ReplaceAll(stripDefiner(ddl), q(d.r.Database)+".", ""))
		}
	}
	if d.o.Structure && d.o.Objects {
		if err := d.mysqlObjects(p); err != nil {
			return err
		}
	}
	d.printf("SET FOREIGN_KEY_CHECKS=1;\n")
	return nil
}

func (d *dumper) mysqlObjects(p *sql.DB) error {
	var blocks []string
	trg, err := d.c.d.triggerList(d.ctx, p, d.r)
	if err != nil {
		return err
	}
	for _, t := range trg {
		if !contains(d.tables, t.Table) {
			continue
		}
		ddl, err := d.c.d.objectDDL(d.ctx, p, d.r, KindTrigger, t.ID)
		if err != nil {
			return err
		}
		blocks = append(blocks, "DROP TRIGGER IF EXISTS "+d.c.d.quote(t.Name)+";\n"+stripDefiner(strings.TrimSuffix(ddl, ";")))
	}
	rts, err := d.c.d.routineList(d.ctx, p, d.r)
	if err != nil {
		return err
	}
	for _, x := range rts {
		ddl, err := d.c.d.objectDDL(d.ctx, p, d.r, x.Kind, x.ID)
		if err != nil {
			return err
		}
		word := "FUNCTION"
		if x.Kind == KindProcedure {
			word = "PROCEDURE"
		}
		blocks = append(blocks, "DROP "+word+" IF EXISTS "+d.c.d.quote(x.Name)+";\n"+stripDefiner(strings.TrimSuffix(ddl, ";")))
	}
	if len(blocks) == 0 {
		return nil
	}
	d.printf("DELIMITER ;;\n")
	for _, b := range blocks {
		// the DROP is a separate statement; the body keeps its inner semicolons
		drop, body, _ := strings.Cut(b, ";\n")
		d.printf("%s;;\n%s;;\n", drop, body)
	}
	d.printf("DELIMITER ;\n\n")
	return nil
}

// ---- PostgreSQL -------------------------------------------------------------------------

var serialRe = regexp.MustCompile(`^nextval\('.*'::regclass\)$`)

type pgSerial struct{ table, col string }

func (d *dumper) postgres() error {
	conn, err := d.c.session(d.ctx, d.r) // search_path = the schema, so names print unqualified
	if err != nil {
		return err
	}
	defer conn.Close()
	q := d.c.d.quote
	d.printf("BEGIN;\n\n")
	if d.o.Structure && d.o.Drop {
		if len(d.views) > 0 {
			d.printf("DROP VIEW IF EXISTS %s;\n", d.quoteList(d.views))
		}
		if len(d.tables) > 0 {
			d.printf("DROP TABLE IF EXISTS %s;\n", d.quoteList(d.tables))
		}
		d.printf("\n")
	}

	var serials []pgSerial
	var fks []string
	for _, t := range d.tables {
		qt := q(t)
		if d.o.Structure {
			create, tfks, ser, err := d.pgTable(conn, t)
			if err != nil {
				return err
			}
			d.printf("%s\n", create)
			fks = append(fks, tfks...)
			serials = append(serials, ser...)
		}
		if d.o.Data {
			cols, err := d.pgDataColumns(conn, t)
			if err != nil {
				return err
			}
			exprs := make([]string, len(cols))
			for i, c := range cols {
				exprs[i] = "quote_nullable(" + q(c) + "::text)"
			}
			var always bool
			if err := conn.QueryRowContext(d.ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass($1) AND attidentity = 'a' AND NOT attisdropped)`, d.pgRegclass(t)).Scan(&always); err != nil {
				return err
			}
			d.override = ""
			if always {
				d.override = " OVERRIDING SYSTEM VALUE"
			}
			if err := d.data(conn, qt, cols, exprs, q(d.r.Schema)+"."+qt); err != nil {
				return err
			}
		}
		d.printf("\n")
	}
	if d.o.Structure && d.o.Data {
		for _, s := range serials {
			seq := "'" + strings.ReplaceAll(q(s.table), "'", "''") + "'"
			col := "'" + strings.ReplaceAll(s.col, "'", "''") + "'"
			d.printf("SELECT setval(pg_get_serial_sequence(%s, %s), COALESCE(MAX(%s), 1), MAX(%s) IS NOT NULL) FROM %s;\n",
				seq, col, q(s.col), q(s.col), q(s.table))
		}
		if len(serials) > 0 {
			d.printf("\n")
		}
	}
	if d.o.Structure {
		for _, f := range fks {
			d.printf("%s\n", f)
		}
		if len(fks) > 0 {
			d.printf("\n")
		}
		if err := d.pgIndexes(conn); err != nil {
			return err
		}
		if err := d.pgViews(conn); err != nil {
			return err
		}
		if d.o.Objects {
			if err := d.pgObjects(conn); err != nil {
				return err
			}
		}
	}
	d.printf("COMMIT;\n")
	return nil
}

func (d *dumper) pgRegclass(table string) string {
	return d.c.d.quote(d.r.Schema) + "." + d.c.d.quote(table)
}

func (d *dumper) pgDataColumns(conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(d.ctx, `SELECT attname FROM pg_attribute
		WHERE attrelid = to_regclass($1) AND attnum > 0 AND NOT attisdropped AND attgenerated = '' ORDER BY attnum`, d.pgRegclass(table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// pgTable builds CREATE TABLE without foreign keys (they are added after the data).
func (d *dumper) pgTable(conn *sql.Conn, table string) (create string, fks []string, serials []pgSerial, err error) {
	q := d.c.d.quote
	rel := d.pgRegclass(table)
	rows, err := conn.QueryContext(d.ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
		       coalesce(pg_get_expr(d.adbin, d.adrelid), ''), a.attidentity::text, a.attgenerated::text
		FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, rel)
	if err != nil {
		return "", nil, nil, err
	}
	var lines []string
	for rows.Next() {
		var name, typ, def, ident, gen string
		var notNull bool
		if err := rows.Scan(&name, &typ, &notNull, &def, &ident, &gen); err != nil {
			rows.Close()
			return "", nil, nil, err
		}
		line := "  " + q(name) + " "
		switch {
		case ident == "a" || ident == "d":
			// ALWAYS stays ALWAYS: the data is loaded with OVERRIDING SYSTEM VALUE
			line += typ + " GENERATED " + map[string]string{"a": "ALWAYS", "d": "BY DEFAULT"}[ident] + " AS IDENTITY"
			serials = append(serials, pgSerial{table, name})
		case gen == "s":
			line += typ + " GENERATED ALWAYS AS (" + def + ") STORED"
		case serialRe.MatchString(def) && (typ == "integer" || typ == "bigint" || typ == "smallint"):
			line += map[string]string{"integer": "serial", "bigint": "bigserial", "smallint": "smallserial"}[typ]
			serials = append(serials, pgSerial{table, name})
		default:
			line += typ
			if notNull {
				line += " NOT NULL"
			}
			if def != "" {
				line += " DEFAULT " + def
			}
		}
		lines = append(lines, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, nil, err
	}
	crows, err := conn.QueryContext(d.ctx, `SELECT conname, contype::text, pg_get_constraintdef(oid, true) FROM pg_constraint
		WHERE conrelid = to_regclass($1) AND contype IN ('p','u','c','f') ORDER BY contype = 'p' DESC, contype, conname`, rel)
	if err != nil {
		return "", nil, nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var name, typ, def string
		if err := crows.Scan(&name, &typ, &def); err != nil {
			return "", nil, nil, err
		}
		if typ == "f" {
			fks = append(fks, "ALTER TABLE "+q(table)+" ADD CONSTRAINT "+q(name)+" "+def+";")
			continue
		}
		lines = append(lines, "  CONSTRAINT "+q(name)+" "+def)
	}
	if err := crows.Err(); err != nil {
		return "", nil, nil, err
	}
	return "CREATE TABLE " + q(table) + " (\n" + strings.Join(lines, ",\n") + "\n);", fks, serials, nil
}

func (d *dumper) pgIndexes(conn *sql.Conn) error {
	if len(d.tables) == 0 {
		return nil
	}
	rows, err := conn.QueryContext(d.ctx, `SELECT tablename, indexname, replace(indexdef, ' ON ' || quote_ident(schemaname) || '.', ' ON ') FROM pg_indexes WHERE schemaname = $1
		AND indexname NOT IN (SELECT conname FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = $1)
		ORDER BY tablename, indexname`, d.r.Schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	wrote := false
	for rows.Next() {
		var tbl, name, def string
		if err := rows.Scan(&tbl, &name, &def); err != nil {
			return err
		}
		if contains(d.tables, tbl) {
			d.printf("%s;\n", def)
			wrote = true
		}
	}
	if wrote {
		d.printf("\n")
	}
	return rows.Err()
}

func (d *dumper) pgViews(conn *sql.Conn) error {
	if len(d.views) == 0 {
		return nil
	}
	// creation (oid) order approximates dependency order between views
	rows, err := conn.QueryContext(d.ctx, `SELECT c.relname, c.relkind::text, pg_get_viewdef(c.oid, true) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind IN ('v','m') ORDER BY c.oid`, d.r.Schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, kind, def string
		if err := rows.Scan(&name, &kind, &def); err != nil {
			return err
		}
		if !contains(d.views, name) {
			continue
		}
		kw := "CREATE VIEW "
		if kind == "m" {
			kw = "CREATE MATERIALIZED VIEW "
		}
		d.printf("%s%s AS\n%s\n\n", kw, d.c.d.quote(name), strings.TrimSpace(def))
	}
	return rows.Err()
}

// pgStripSchema removes the schema qualifier pg_get_functiondef always prints.
func pgStripSchema(def, schema string) string {
	for _, pre := range []string{`"` + strings.ReplaceAll(schema, `"`, `""`) + `".`, schema + "."} {
		for _, kw := range []string{"FUNCTION ", "PROCEDURE "} {
			def = strings.Replace(def, kw+pre, kw, 1)
		}
	}
	return def
}

func (d *dumper) pgObjects(conn *sql.Conn) error {
	// functions first (triggers call them)
	frows, err := conn.QueryContext(d.ctx, `SELECT pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.prokind IN ('f','p') AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
		ORDER BY p.oid`, d.r.Schema)
	if err != nil {
		return err
	}
	var defs []string
	for frows.Next() {
		var def string
		if err := frows.Scan(&def); err != nil {
			frows.Close()
			return err
		}
		defs = append(defs, strings.TrimRight(strings.TrimSpace(pgStripSchema(def, d.r.Schema)), ";")+";")
	}
	frows.Close()
	for _, s := range defs {
		d.printf("%s\n\n", s)
	}
	trows, err := conn.QueryContext(d.ctx, `SELECT c.relname, pg_get_triggerdef(tg.oid, true) FROM pg_trigger tg
		JOIN pg_class c ON c.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT tg.tgisinternal AND n.nspname = $1 ORDER BY c.relname, tg.tgname`, d.r.Schema)
	if err != nil {
		return err
	}
	defer trows.Close()
	for trows.Next() {
		var tbl, def string
		if err := trows.Scan(&tbl, &def); err != nil {
			return err
		}
		if contains(d.tables, tbl) {
			d.printf("%s;\n", def)
		}
	}
	return trows.Err()
}

// ---- restore ---------------------------------------------------------------------------

// ExecSummary reports what running a script file did.
type ExecSummary struct {
	Statements int // statements run (including failed ones)
	Failed     int
	Total      int // statements in the file
	Errors     []string
	Stopped    bool // stopped at the first error
	Elapsed    time.Duration
}

const maxReportedErrors = 20

// ExecScript runs every statement of script on one session (so SET, USE,
// PRAGMA and BEGIN/COMMIT in the file apply to what follows). With
// continueOnError it keeps going after a failing statement. The session is
// always discarded afterwards so no state from the file leaks into the pool.
func (c *Conn) ExecScript(ctx context.Context, r Ref, script string, continueOnError bool) (*ExecSummary, error) {
	stmts := SplitStatements(c.Config.Kind, script)
	if len(stmts) == 0 {
		return nil, errors.New("실행할 SQL이 없습니다")
	}
	conn, err := c.session(ctx, r)
	if err != nil {
		return nil, err
	}
	defer discard(conn)
	start := time.Now()
	sum := &ExecSummary{Total: len(stmts)}
	for _, s := range stmts {
		sum.Statements++
		if _, err := conn.ExecContext(ctx, s); err != nil {
			sum.Failed++
			if len(sum.Errors) < maxReportedErrors {
				snippet := strings.Join(strings.Fields(s), " ")
				if len(snippet) > 140 {
					snippet = snippet[:140] + "…"
				}
				sum.Errors = append(sum.Errors, fmt.Sprintf("#%d %s\n  문: %s", sum.Statements, err, snippet))
			}
			if !continueOnError {
				sum.Stopped = true
				break
			}
		}
		if ctx.Err() != nil {
			sum.Stopped = true
			sum.Errors = append(sum.Errors, "시간 제한 또는 취소로 중단되었습니다: "+ctx.Err().Error())
			break
		}
	}
	sum.Elapsed = time.Since(start)
	return sum, nil
}
