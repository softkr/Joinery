package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// IndexInfo is one index of a schema (not only the ones of a single table).
type IndexInfo struct {
	ID      string // opaque id used to fetch the definition
	Name    string
	Table   string
	Columns string
	Unique  bool
	Primary bool
}

// TriggerInfo is one trigger; Event reads like "AFTER INSERT OR UPDATE".
type TriggerInfo struct {
	ID    string
	Name  string
	Table string
	Event string
}

// RoutineInfo is a stored function or procedure.
type RoutineInfo struct {
	ID       string
	Name     string
	Kind     string // "function" or "procedure"
	Args     string
	Returns  string
	Language string
}

// Object kinds accepted by ObjectDDL.
const (
	KindIndex     = "index"
	KindTrigger   = "trigger"
	KindFunction  = "function"
	KindProcedure = "procedure"
)

// HasRoutines reports whether the engine has stored functions/procedures.
func (c *Conn) HasRoutines() bool { return c.Config.Kind != SQLite }

func (c *Conn) IndexList(ctx context.Context, r Ref) ([]IndexInfo, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	return c.d.indexList(ctx, p, r)
}

func (c *Conn) TriggerList(ctx context.Context, r Ref) ([]TriggerInfo, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	return c.d.triggerList(ctx, p, r)
}

func (c *Conn) RoutineList(ctx context.Context, r Ref) ([]RoutineInfo, error) {
	if !c.HasRoutines() {
		return nil, nil
	}
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	return c.d.routineList(ctx, p, r)
}

// ObjectDDL returns the definition of an index, trigger, function or procedure.
func (c *Conn) ObjectDDL(ctx context.Context, r Ref, kind, id string) (string, error) {
	switch kind {
	case KindIndex, KindTrigger, KindFunction, KindProcedure:
	default:
		return "", fmt.Errorf("알 수 없는 오브젝트 종류: %q", kind)
	}
	if (kind == KindFunction || kind == KindProcedure) && !c.HasRoutines() {
		return "", fmt.Errorf("%s는 함수/프로시저를 지원하지 않습니다", c.Config.Kind.Label())
	}
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return "", err
	}
	return c.d.objectDDL(ctx, p, r, kind, id)
}

var triggerEventRe = regexp.MustCompile(`(?is)\b(BEFORE|AFTER|INSTEAD\s+OF)\s+(INSERT|UPDATE|DELETE|TRUNCATE)(\s+OF\s+[\w\s,"` + "`" + `]+?)?((?:\s+OR\s+(?:INSERT|UPDATE|DELETE|TRUNCATE))*)\s+ON\b`)

// triggerEvent extracts "AFTER INSERT OR UPDATE" from a trigger definition.
func triggerEvent(def string) string {
	m := triggerEventRe.FindStringSubmatch(def)
	if m == nil {
		return ""
	}
	ev := strings.Join(strings.Fields(m[1]), " ") + " " + m[2]
	if extra := strings.Join(strings.Fields(m[4]), " "); extra != "" {
		ev += " " + extra
	}
	return strings.ToUpper(ev)
}

func synthIndexDDL(ix IndexInfo, quote func(string) string) string {
	if ix.Primary {
		return "ALTER TABLE " + quote(ix.Table) + " ADD PRIMARY KEY (" + ix.Columns + ");"
	}
	kw := "CREATE INDEX "
	if ix.Unique {
		kw = "CREATE UNIQUE INDEX "
	}
	return kw + quote(ix.Name) + " ON " + quote(ix.Table) + " (" + ix.Columns + ");"
}

// ---- SQLite ---------------------------------------------------------------------

func (sqliteDialect) indexList(ctx context.Context, p *sql.DB, _ Ref) ([]IndexInfo, error) {
	rows, err := p.QueryContext(ctx, `SELECT m.name, il.name, il."unique",
		(SELECT group_concat(name, ', ') FROM pragma_index_info(il.name))
		FROM sqlite_master m, pragma_index_list(m.name) il
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND il.origin <> 'pk'
		ORDER BY m.name, il.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexInfo
	for rows.Next() {
		var ix IndexInfo
		var uniq int
		var cols sql.NullString
		if err := rows.Scan(&ix.Table, &ix.Name, &uniq, &cols); err != nil {
			return nil, err
		}
		ix.ID, ix.Unique, ix.Columns = ix.Name, uniq == 1, cols.String
		out = append(out, ix)
	}
	return out, rows.Err()
}

func (sqliteDialect) triggerList(ctx context.Context, p *sql.DB, _ Ref) ([]TriggerInfo, error) {
	rows, err := p.QueryContext(ctx, `SELECT name, tbl_name, sql FROM sqlite_master WHERE type = 'trigger' ORDER BY tbl_name, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriggerInfo
	for rows.Next() {
		var t TriggerInfo
		var def sql.NullString
		if err := rows.Scan(&t.Name, &t.Table, &def); err != nil {
			return nil, err
		}
		t.ID, t.Event = t.Name, triggerEvent(def.String)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (sqliteDialect) routineList(context.Context, *sql.DB, Ref) ([]RoutineInfo, error) {
	return nil, nil
}

func (d sqliteDialect) objectDDL(ctx context.Context, p *sql.DB, r Ref, kind, id string) (string, error) {
	var def sql.NullString
	err := p.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = ? AND name = ?`, kind, id).Scan(&def)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("%s: 정의를 찾을 수 없습니다", id)
	}
	if err != nil {
		return "", err
	}
	if !def.Valid {
		return "-- 제약 조건에서 자동으로 만들어진 인덱스입니다 (별도 정의 없음)", nil
	}
	return strings.TrimSpace(def.String) + ";", nil
}

// ---- PostgreSQL -------------------------------------------------------------------

func (postgresDialect) indexList(ctx context.Context, p *sql.DB, r Ref) ([]IndexInfo, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT ic.relname, t.relname, ix.indisunique, ix.indisprimary,
		       array_to_string(array(SELECT pg_get_indexdef(ix.indexrelid, k + 1, true)
		         FROM generate_subscripts(ix.indkey, 1) AS k ORDER BY k), ', ')
		FROM pg_index ix
		JOIN pg_class ic ON ic.oid = ix.indexrelid
		JOIN pg_class t ON t.oid = ix.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1 ORDER BY t.relname, ic.relname`, r.Schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexInfo
	for rows.Next() {
		var ix IndexInfo
		if err := rows.Scan(&ix.Name, &ix.Table, &ix.Unique, &ix.Primary, &ix.Columns); err != nil {
			return nil, err
		}
		ix.ID = ix.Name
		out = append(out, ix)
	}
	return out, rows.Err()
}

func (postgresDialect) triggerList(ctx context.Context, p *sql.DB, r Ref) ([]TriggerInfo, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT tg.tgname, c.relname, pg_get_triggerdef(tg.oid), tg.oid::bigint
		FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT tg.tgisinternal AND n.nspname = $1 ORDER BY c.relname, tg.tgname`, r.Schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriggerInfo
	for rows.Next() {
		var t TriggerInfo
		var def string
		var oid int64
		if err := rows.Scan(&t.Name, &t.Table, &def, &oid); err != nil {
			return nil, err
		}
		t.ID, t.Event = strconv.FormatInt(oid, 10), triggerEvent(def)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (postgresDialect) routineList(ctx context.Context, p *sql.DB, r Ref) ([]RoutineInfo, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT p.proname, p.prokind::text, pg_get_function_identity_arguments(p.oid),
		       coalesce(pg_get_function_result(p.oid), ''), l.lanname, p.oid::bigint
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace JOIN pg_language l ON l.oid = p.prolang
		WHERE n.nspname = $1 AND p.prokind IN ('f', 'p')
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
		ORDER BY p.proname, p.oid`, r.Schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutineInfo
	for rows.Next() {
		var x RoutineInfo
		var kind string
		var oid int64
		if err := rows.Scan(&x.Name, &kind, &x.Args, &x.Returns, &x.Language, &oid); err != nil {
			return nil, err
		}
		x.Kind = KindFunction
		if kind == "p" {
			x.Kind = KindProcedure
		}
		x.ID = strconv.FormatInt(oid, 10)
		out = append(out, x)
	}
	return out, rows.Err()
}

func (postgresDialect) objectDDL(ctx context.Context, p *sql.DB, r Ref, kind, id string) (string, error) {
	var def sql.NullString
	var err error
	switch kind {
	case KindIndex:
		err = p.QueryRowContext(ctx, `SELECT pg_get_indexdef(ic.oid) FROM pg_class ic JOIN pg_namespace n ON n.oid = ic.relnamespace
			WHERE n.nspname = $1 AND ic.relname = $2 AND ic.relkind IN ('i','I')`, r.Schema, id).Scan(&def)
	default:
		oid, perr := strconv.ParseInt(id, 10, 64)
		if perr != nil {
			return "", fmt.Errorf("잘못된 오브젝트 id: %q", id)
		}
		q := "SELECT pg_get_functiondef($1::oid)"
		if kind == KindTrigger {
			q = "SELECT pg_get_triggerdef($1::oid, true)"
		}
		err = p.QueryRowContext(ctx, q, oid).Scan(&def)
	}
	if err == sql.ErrNoRows || (err == nil && !def.Valid) {
		return "", fmt.Errorf("%s: 정의를 찾을 수 없습니다", id)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(strings.TrimSpace(def.String), ";") + ";", nil
}

// ---- MySQL / MariaDB ----------------------------------------------------------------

const mysqlIDSep = "\x1f"

func (mysqlDialect) indexList(ctx context.Context, p *sql.DB, r Ref) ([]IndexInfo, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT INDEX_NAME, TABLE_NAME, MAX(NON_UNIQUE) = 0,
		       GROUP_CONCAT(CONCAT('`+"`"+`', COLUMN_NAME, '`+"`"+`') ORDER BY SEQ_IN_INDEX SEPARATOR ', ')
		FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ?
		GROUP BY TABLE_NAME, INDEX_NAME ORDER BY TABLE_NAME, INDEX_NAME`, r.Database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexInfo
	for rows.Next() {
		var ix IndexInfo
		var cols sql.NullString
		if err := rows.Scan(&ix.Name, &ix.Table, &ix.Unique, &cols); err != nil {
			return nil, err
		}
		ix.Primary = ix.Name == "PRIMARY"
		ix.Columns = cols.String
		ix.ID = ix.Table + mysqlIDSep + ix.Name
		out = append(out, ix)
	}
	return out, rows.Err()
}

func (mysqlDialect) triggerList(ctx context.Context, p *sql.DB, r Ref) ([]TriggerInfo, error) {
	rows, err := p.QueryContext(ctx, `SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE, ACTION_TIMING, EVENT_MANIPULATION
		FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY EVENT_OBJECT_TABLE, TRIGGER_NAME`, r.Database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriggerInfo
	for rows.Next() {
		var t TriggerInfo
		var timing, event string
		if err := rows.Scan(&t.Name, &t.Table, &timing, &event); err != nil {
			return nil, err
		}
		t.ID, t.Event = t.Name, timing+" "+event
		out = append(out, t)
	}
	return out, rows.Err()
}

func (mysqlDialect) routineList(ctx context.Context, p *sql.DB, r Ref) ([]RoutineInfo, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT r.ROUTINE_NAME, r.ROUTINE_TYPE, COALESCE(r.DTD_IDENTIFIER, ''), COALESCE(r.ROUTINE_BODY, ''),
		       (SELECT GROUP_CONCAT(CONCAT(COALESCE(pa.PARAMETER_MODE, ''), ' ', pa.PARAMETER_NAME, ' ', pa.DTD_IDENTIFIER)
		                 ORDER BY pa.ORDINAL_POSITION SEPARATOR ', ')
		          FROM information_schema.PARAMETERS pa
		         WHERE pa.SPECIFIC_SCHEMA = r.ROUTINE_SCHEMA AND pa.SPECIFIC_NAME = r.SPECIFIC_NAME AND pa.ORDINAL_POSITION > 0)
		FROM information_schema.ROUTINES r WHERE r.ROUTINE_SCHEMA = ? ORDER BY r.ROUTINE_NAME`, r.Database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutineInfo
	for rows.Next() {
		var x RoutineInfo
		var typ string
		var args sql.NullString
		if err := rows.Scan(&x.Name, &typ, &x.Returns, &x.Language, &args); err != nil {
			return nil, err
		}
		x.Kind = KindFunction
		if strings.EqualFold(typ, "PROCEDURE") {
			x.Kind = KindProcedure
		}
		x.Args = strings.Join(strings.Fields(args.String), " ")
		x.ID = x.Name
		out = append(out, x)
	}
	return out, rows.Err()
}

// showCreate runs a SHOW CREATE statement and returns the column at pos.
func showCreate(ctx context.Context, p *sql.DB, q string, pos int) (string, error) {
	rows, err := p.QueryContext(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if !rows.Next() {
		return "", fmt.Errorf("정의를 찾을 수 없습니다")
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}
	if pos >= len(vals) {
		return "", fmt.Errorf("예상하지 못한 SHOW CREATE 결과")
	}
	return string(vals[pos]), nil
}

func (d mysqlDialect) objectDDL(ctx context.Context, p *sql.DB, r Ref, kind, id string) (string, error) {
	switch kind {
	case KindIndex:
		table, name, ok := strings.Cut(id, mysqlIDSep)
		if !ok {
			return "", fmt.Errorf("잘못된 오브젝트 id")
		}
		list, err := d.indexList(ctx, p, r)
		if err != nil {
			return "", err
		}
		for _, ix := range list {
			if ix.Table == table && ix.Name == name {
				return synthIndexDDL(IndexInfo{Name: ix.Name, Table: table, Columns: ix.Columns, Unique: ix.Unique, Primary: ix.Primary}, d.quote), nil
			}
		}
		return "", fmt.Errorf("%s: 정의를 찾을 수 없습니다", name)
	case KindTrigger:
		s, err := showCreate(ctx, p, "SHOW CREATE TRIGGER "+d.qualified(Ref{Database: r.Database, Table: id}), 2)
		return strings.TrimSpace(s) + ";", err
	default:
		word := "FUNCTION"
		if kind == KindProcedure {
			word = "PROCEDURE"
		}
		s, err := showCreate(ctx, p, "SHOW CREATE "+word+" "+d.qualified(Ref{Database: r.Database, Table: id}), 2)
		return strings.TrimSpace(s) + ";", err
	}
}
