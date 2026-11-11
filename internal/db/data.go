package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const MaxQueryRows = 1000

// PageOpts controls a table data page. Where is a raw condition typed by the
// user into the filter box (the same trust level as the SQL editor).
type PageOpts struct {
	Sort   string
	Desc   bool
	Where  string
	Limit  int
	Offset int
}

type Page struct {
	Columns []Column
	Rows    [][]Cell
	More    bool
	SQL     string // statement that produced the page, shown in the status bar
}

func findColumn(cols []Column, name string) (Column, bool) {
	for _, c := range cols {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

func (c *Conn) colList(cols []Column) string {
	names := make([]string, len(cols))
	for i, col := range cols {
		names[i] = c.d.quote(col.Name)
	}
	return strings.Join(names, ", ")
}

var errWhereSemicolon = errors.New("WHERE 조건에는 ';'를 사용할 수 없습니다")

// cleanWhere trims the user's filter. A ';' is rejected so the filter box can
// never smuggle a second statement into the generated SELECT.
func cleanWhere(w string) (string, error) {
	w = strings.TrimSpace(w)
	w = strings.TrimSpace(strings.TrimSuffix(w, ";"))
	if strings.Contains(w, ";") {
		return "", errWhereSemicolon
	}
	return w, nil
}

// selectSQL builds "SELECT cols FROM t [WHERE ..] [ORDER BY ..]" without paging.
// The sort column is only used when it is a real column of the table.
func (c *Conn) selectSQL(cols []Column, r Ref, o PageOpts) (string, error) {
	w, err := cleanWhere(o.Where)
	if err != nil {
		return "", err
	}
	q := "SELECT " + c.colList(cols) + " FROM " + c.d.qualified(r)
	if w != "" {
		q += " WHERE " + w
	}
	if o.Sort != "" {
		if _, ok := findColumn(cols, o.Sort); ok {
			q += " ORDER BY " + c.d.quote(o.Sort)
			if o.Desc {
				q += " DESC"
			}
		}
	}
	return q, nil
}

// TablePage reads one page of a table or view.
func (c *Conn) TablePage(ctx context.Context, r Ref, o PageOpts) (*Page, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	cols, err := c.d.columns(ctx, p, r)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("%s: 컬럼을 찾을 수 없습니다", r.Table)
	}
	q, err := c.selectSQL(cols, r, o)
	if err != nil {
		return nil, err
	}
	shown := q + fmt.Sprintf(" LIMIT %d OFFSET %d", o.Limit, max(o.Offset, 0))
	// Fetch one extra row so the UI can tell whether a next page exists.
	q += fmt.Sprintf(" LIMIT %d OFFSET %d", o.Limit+1, max(o.Offset, 0))
	rows, err := p.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	_, data, more, err := scanRows(rows, o.Limit)
	if err != nil {
		return nil, err
	}
	return &Page{Columns: cols, Rows: data, More: more, SQL: shown}, nil
}

// Count returns the number of rows matching where.
func (c *Conn) Count(ctx context.Context, r Ref, where string) (int64, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return 0, err
	}
	w, err := cleanWhere(where)
	if err != nil {
		return 0, err
	}
	q := "SELECT COUNT(*) FROM " + c.d.qualified(r)
	if w != "" {
		q += " WHERE " + w
	}
	var n int64
	err = p.QueryRowContext(ctx, q).Scan(&n)
	return n, err
}

// pkWhere builds "pk1 = ? AND pk2 = ?" validating every key against metadata.
func (c *Conn) pkWhere(cols []Column, pk map[string]string, start int) (string, []any, error) {
	var parts []string
	var args []any
	n := start
	for _, col := range cols {
		if !col.PK {
			continue
		}
		v, ok := pk[col.Name]
		if !ok {
			return "", nil, fmt.Errorf("기본키 값 누락: %s", col.Name)
		}
		parts = append(parts, c.d.quote(col.Name)+" = "+c.d.placeholder(n))
		args = append(args, v)
		n++
	}
	if len(parts) == 0 {
		return "", nil, errors.New("기본키가 없는 테이블은 편집할 수 없습니다")
	}
	return strings.Join(parts, " AND "), args, nil
}

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("영향받은 행이 %d개입니다 (1개 예상)", n)
	}
	return nil
}

// UpdateCell sets one column of the row identified by its primary key.
func (c *Conn) UpdateCell(ctx context.Context, r Ref, pk map[string]string, col string, val Cell) error {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return err
	}
	cols, err := c.d.columns(ctx, p, r)
	if err != nil {
		return err
	}
	if _, ok := findColumn(cols, col); !ok {
		return fmt.Errorf("알 수 없는 컬럼: %s", col)
	}
	var arg any = val.V
	if val.Null {
		arg = nil
	}
	where, wargs, err := c.pkWhere(cols, pk, 2)
	if err != nil {
		return err
	}
	q := "UPDATE " + c.d.qualified(r) + " SET " + c.d.quote(col) + " = " + c.d.placeholder(1) + " WHERE " + where
	res, err := p.ExecContext(ctx, q, append([]any{arg}, wargs...)...)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// DeleteRow removes the row identified by its primary key.
func (c *Conn) DeleteRow(ctx context.Context, r Ref, pk map[string]string) error {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return err
	}
	cols, err := c.d.columns(ctx, p, r)
	if err != nil {
		return err
	}
	where, args, err := c.pkWhere(cols, pk, 1)
	if err != nil {
		return err
	}
	res, err := p.ExecContext(ctx, "DELETE FROM "+c.d.qualified(r)+" WHERE "+where, args...)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// Result is the outcome of one statement run from the SQL editor.
type Result struct {
	Columns  []string
	Rows     [][]Cell
	More     bool
	IsQuery  bool
	Affected int64
	Elapsed  time.Duration
}

var returningRe = regexp.MustCompile(`(?i)\bRETURNING\b`)

// returnsRows reports whether stmt produces a result set.
func returnsRows(kind Kind, stmt string) bool {
	stmt = trimLeadingComments(kind, stmt)
	words := strings.FieldsFunc(stmt, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ';' || r == '('
	})
	if len(words) == 0 {
		return false
	}
	switch strings.ToUpper(words[0]) {
	case "SELECT", "WITH", "SHOW", "EXPLAIN", "PRAGMA", "DESCRIBE", "DESC", "VALUES", "TABLE":
		return true
	case "INSERT", "UPDATE", "DELETE", "MERGE":
		return kind != MySQL && returningRe.MatchString(stmt)
	}
	return false
}

// altersSession reports statements that change per-connection state (a
// transaction, SET, PRAGMA, ...). The pooled connection is discarded afterwards
// so that state cannot leak into the next request.
func altersSession(kind Kind, stmt string) bool {
	words := strings.Fields(trimLeadingComments(kind, stmt))
	if len(words) == 0 {
		return false
	}
	switch strings.ToUpper(strings.TrimRight(words[0], ";")) {
	case "BEGIN", "START", "SAVEPOINT", "SET", "LOCK", "PRAGMA", "USE", "DISCARD", "RESET", "LISTEN", "PREPARE":
		return true
	}
	return false
}

// runOn executes one statement on conn.
func (c *Conn) runOn(ctx context.Context, conn *sql.Conn, stmt string) (*Result, error) {
	start := time.Now()
	res := &Result{}
	if returnsRows(c.Config.Kind, stmt) {
		rows, err := conn.QueryContext(ctx, stmt)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		res.IsQuery = true
		res.Columns, res.Rows, res.More, err = scanRows(rows, MaxQueryRows)
		if err != nil {
			return nil, err
		}
	} else {
		out, err := conn.ExecContext(ctx, stmt)
		if err != nil {
			return nil, err
		}
		res.Affected, _ = out.RowsAffected()
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

// session leases a dedicated connection pointed at r's database/schema.
func (c *Conn) session(ctx context.Context, r Ref) (*sql.Conn, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	conn, err := p.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.d.useSchema(ctx, conn, r); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// discard closes conn and makes the pool drop it instead of reusing it.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	conn.Close()
}

// Run executes a single statement with the given database/schema as context.
func (c *Conn) Run(ctx context.Context, r Ref, stmt string) (*Result, error) {
	stmt = strings.TrimSpace(stmt)
	if stmt == "" {
		return nil, errors.New("실행할 SQL이 없습니다")
	}
	conn, err := c.session(ctx, r)
	if err != nil {
		return nil, err
	}
	dirty := altersSession(c.Config.Kind, stmt)
	defer func() {
		if dirty {
			discard(conn)
		} else {
			conn.Close()
		}
	}()
	return c.runOn(ctx, conn, stmt)
}

// MaxScriptStatements bounds how many statements one editor run may contain.
const MaxScriptStatements = 500

// StmtResult is the outcome of one statement of a script.
type StmtResult struct {
	SQL    string
	Result *Result // nil when Err is set
	Err    error
}

// RunScript splits script into statements and runs them in order on one
// session (so USE, SET and transactions apply to what follows). It stops at
// the first error; the failed statement is the last element. skipped counts
// the statements that were not run because of it.
func (c *Conn) RunScript(ctx context.Context, r Ref, script string) (results []StmtResult, skipped int, err error) {
	stmts := SplitStatements(c.Config.Kind, script)
	if len(stmts) == 0 {
		return nil, 0, errors.New("실행할 SQL이 없습니다")
	}
	if len(stmts) > MaxScriptStatements {
		return nil, 0, fmt.Errorf("한 번에 실행할 수 있는 문장은 %d개까지입니다 (%d개)", MaxScriptStatements, len(stmts))
	}
	conn, err := c.session(ctx, r)
	if err != nil {
		return nil, 0, err
	}
	dirty := false
	defer func() {
		if dirty {
			discard(conn)
		} else {
			conn.Close()
		}
	}()
	for i, stmt := range stmts {
		dirty = dirty || altersSession(c.Config.Kind, stmt)
		res, err := c.runOn(ctx, conn, stmt)
		if err != nil {
			results = append(results, StmtResult{SQL: stmt, Err: err})
			return results, len(stmts) - i - 1, nil
		}
		results = append(results, StmtResult{SQL: stmt, Result: res})
	}
	return results, 0, nil
}
