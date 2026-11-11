package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// FieldSpec is one row of the table designer.
type FieldSpec struct {
	Orig    string // existing column name, "" for a new field
	Name    string
	Type    string
	NotNull bool
	Default string // raw SQL expression, "" means no default
	PK      bool
	Drop    bool // existing columns only
}

// IndexSpec is one row of the designer's index tab. Existing indexes can only
// be dropped; new ones (Orig == "") are created.
type IndexSpec struct {
	Orig    string
	Name    string
	Columns []string
	Unique  bool
	Drop    bool
}

// CanAlterFields reports whether type / nullability / default of existing
// columns can be changed. Only PostgreSQL supports that with plain ALTER
// statements; SQLite needs a table rebuild and MySQL's MODIFY COLUMN silently
// loses attributes it is not told about, so both are left to the SQL editor.
func (c *Conn) CanAlterFields() bool { return c.Config.Kind == Postgres }

func validIdent(kind, s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s 이름을 입력하세요", kind)
	}
	if len(s) > 128 || strings.ContainsRune(s, 0) {
		return fmt.Errorf("%s 이름이 올바르지 않습니다: %q", kind, s)
	}
	return nil
}

// validType allows ordinary type spellings such as varchar(255), numeric(10,2),
// double precision, int[] or "INT AUTO_INCREMENT" and rejects everything else.
func validType(t string) error {
	t = strings.TrimSpace(t)
	if t == "" {
		return errors.New("필드 유형을 입력하세요")
	}
	if len(t) > 80 || !((t[0] >= 'a' && t[0] <= 'z') || (t[0] >= 'A' && t[0] <= 'Z') || t[0] == '_') {
		return fmt.Errorf("유형이 올바르지 않습니다: %q", t)
	}
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_ (),[]'", r):
		default:
			return fmt.Errorf("유형에 사용할 수 없는 문자가 있습니다: %q", t)
		}
	}
	return nil
}

// validDefault keeps the default expression to a single expression.
func validDefault(d string) error {
	if len(d) > 200 || strings.ContainsAny(d, ";\x00") || strings.Contains(d, "--") || strings.Contains(d, "/*") {
		return fmt.Errorf("기본값 표현식이 올바르지 않습니다: %q", d)
	}
	return nil
}

func (c *Conn) columnDef(f FieldSpec) string {
	def := c.d.quote(f.Name) + " " + strings.TrimSpace(f.Type)
	if f.NotNull {
		def += " NOT NULL"
	}
	if d := strings.TrimSpace(f.Default); d != "" {
		def += " DEFAULT " + d
	}
	return def
}

func (c *Conn) validateFields(fields []FieldSpec) ([]FieldSpec, error) {
	var live []FieldSpec
	seen := map[string]bool{}
	for _, f := range fields {
		if f.Drop {
			continue
		}
		if err := validIdent("필드", f.Name); err != nil {
			return nil, err
		}
		if err := validType(f.Type); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		if err := validDefault(f.Default); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		key := strings.ToLower(f.Name)
		if seen[key] {
			return nil, fmt.Errorf("필드 이름이 중복되었습니다: %s", f.Name)
		}
		seen[key] = true
		live = append(live, f)
	}
	return live, nil
}

func (c *Conn) validateNewIndex(x IndexSpec, live []FieldSpec) error {
	if err := validIdent("인덱스", x.Name); err != nil {
		return err
	}
	if len(x.Columns) == 0 {
		return fmt.Errorf("인덱스 %s: 필드를 지정하세요", x.Name)
	}
	for _, col := range x.Columns {
		ok := false
		for _, f := range live {
			if f.Name == col {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("인덱스 %s: 알 수 없는 필드 %q", x.Name, col)
		}
	}
	return nil
}

func (c *Conn) createIndexSQL(r Ref, x IndexSpec) string {
	cols := make([]string, len(x.Columns))
	for i, col := range x.Columns {
		cols[i] = c.d.quote(col)
	}
	kw := "CREATE INDEX "
	if x.Unique {
		kw = "CREATE UNIQUE INDEX "
	}
	return kw + c.d.quote(x.Name) + " ON " + c.d.qualified(r) + " (" + strings.Join(cols, ", ") + ")"
}

func (c *Conn) dropIndexSQL(r Ref, name string) string {
	switch c.Config.Kind {
	case MySQL:
		return "DROP INDEX " + c.d.quote(name) + " ON " + c.d.qualified(r)
	case Postgres:
		return "DROP INDEX " + joinNonEmpty(".", c.d.quote(r.Schema), c.d.quote(name))
	}
	return "DROP INDEX " + c.d.quote(name)
}

// PlanCreate returns the statements that create a new table.
func (c *Conn) PlanCreate(r Ref, fields []FieldSpec, indexes []IndexSpec, fks []FKSpec) ([]string, error) {
	if err := validIdent("테이블", r.Table); err != nil {
		return nil, err
	}
	live, err := c.validateFields(fields)
	if err != nil {
		return nil, err
	}
	if len(live) == 0 {
		return nil, errors.New("필드가 하나 이상 필요합니다")
	}
	var lines, pk []string
	for _, f := range live {
		lines = append(lines, "  "+c.columnDef(f))
		if f.PK {
			pk = append(pk, c.d.quote(f.Name))
		}
	}
	if len(pk) > 0 {
		lines = append(lines, "  PRIMARY KEY ("+strings.Join(pk, ", ")+")")
	}
	for _, fk := range fks {
		if fk.Drop || fk.Orig != "" {
			continue
		}
		clause, err := c.fkClause(r, fk, live)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "  "+clause)
	}
	stmts := []string{"CREATE TABLE " + c.d.qualified(r) + " (\n" + strings.Join(lines, ",\n") + "\n)"}
	for _, x := range indexes {
		if x.Drop || x.Orig != "" {
			continue
		}
		if err := c.validateNewIndex(x, live); err != nil {
			return nil, err
		}
		stmts = append(stmts, c.createIndexSQL(r, x))
	}
	return stmts, nil
}

func sameType(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

// PlanAlter diffs the designer state against the live table and returns the
// ALTER / CREATE INDEX / DROP INDEX statements to apply (empty when unchanged).
func (c *Conn) PlanAlter(ctx context.Context, r Ref, fields []FieldSpec, indexes []IndexSpec, fks []FKSpec) ([]string, error) {
	cur, err := c.Columns(ctx, r)
	if err != nil {
		return nil, err
	}
	curIdx, err := c.Indexes(ctx, r)
	if err != nil {
		return nil, err
	}
	live, err := c.validateFields(fields)
	if err != nil {
		return nil, err
	}
	qt := c.d.qualified(r)
	var dropFK, dropIdx, drops, renames, modifies, adds, creates, addFK []string

	var curFK []ForeignKey
	for _, fk := range fks {
		if fk.Drop && fk.Orig != "" {
			if !c.CanAlterFK() {
				return nil, errors.New("SQLite는 기존 테이블의 외래 키를 삭제할 수 없습니다 (새 테이블을 만들 때만 정의할 수 있습니다)")
			}
			if curFK == nil {
				if curFK, err = c.ForeignKeys(ctx, r); err != nil {
					return nil, err
				}
			}
			if !slices.ContainsFunc(curFK, func(x ForeignKey) bool { return x.Name == fk.Orig }) {
				return nil, fmt.Errorf("외래 키를 찾을 수 없습니다: %s", fk.Orig)
			}
			dropFK = append(dropFK, c.dropFKSQL(r, fk.Orig))
		}
	}

	for _, x := range indexes {
		if !x.Drop || x.Orig == "" {
			continue
		}
		found := false
		for _, ci := range curIdx {
			if ci.Name != x.Orig {
				continue
			}
			found = true
			if ci.Primary {
				return nil, errors.New("기본키 인덱스는 삭제할 수 없습니다")
			}
			if strings.HasPrefix(ci.Name, "sqlite_autoindex_") {
				return nil, fmt.Errorf("자동 생성 인덱스는 삭제할 수 없습니다: %s", ci.Name)
			}
		}
		if !found {
			return nil, fmt.Errorf("인덱스를 찾을 수 없습니다: %s", x.Orig)
		}
		dropIdx = append(dropIdx, c.dropIndexSQL(r, x.Orig))
	}

	for _, f := range fields {
		if f.Orig == "" {
			if f.Drop {
				continue
			}
			if f.PK {
				return nil, errors.New("기존 테이블에는 기본키를 추가할 수 없습니다")
			}
			adds = append(adds, "ALTER TABLE "+qt+" ADD COLUMN "+c.columnDef(f))
			continue
		}
		orig, ok := findColumn(cur, f.Orig)
		if !ok {
			return nil, fmt.Errorf("원본 필드를 찾을 수 없습니다: %s (다른 곳에서 변경되었을 수 있습니다)", f.Orig)
		}
		if f.Drop {
			drops = append(drops, "ALTER TABLE "+qt+" DROP COLUMN "+c.d.quote(f.Orig))
			continue
		}
		if f.PK != orig.PK {
			return nil, fmt.Errorf("%s: 기존 테이블의 기본키는 변경할 수 없습니다", f.Orig)
		}
		if f.Name != f.Orig {
			renames = append(renames, "ALTER TABLE "+qt+" RENAME COLUMN "+c.d.quote(f.Orig)+" TO "+c.d.quote(f.Name))
		}
		typeChanged := !sameType(f.Type, orig.Type)
		nullChanged := f.NotNull == orig.Nullable
		defChanged := strings.TrimSpace(f.Default) != strings.TrimSpace(orig.Default)
		if !(typeChanged || nullChanged || defChanged) {
			continue
		}
		if !c.CanAlterFields() {
			return nil, fmt.Errorf("%s: 이 DB에서는 필드 유형/NULL/기본값 변경을 지원하지 않습니다 (이름 변경, 추가, 삭제만 가능)", f.Orig)
		}
		col := c.d.quote(f.Name)
		alter := "ALTER TABLE " + qt + " ALTER COLUMN " + col
		if typeChanged {
			modifies = append(modifies, alter+" TYPE "+strings.TrimSpace(f.Type))
		}
		if nullChanged {
			if f.NotNull {
				modifies = append(modifies, alter+" SET NOT NULL")
			} else {
				modifies = append(modifies, alter+" DROP NOT NULL")
			}
		}
		if defChanged {
			if d := strings.TrimSpace(f.Default); d != "" {
				modifies = append(modifies, alter+" SET DEFAULT "+d)
			} else {
				modifies = append(modifies, alter+" DROP DEFAULT")
			}
		}
	}

	for _, x := range indexes {
		if x.Drop || x.Orig != "" {
			continue
		}
		if err := c.validateNewIndex(x, live); err != nil {
			return nil, err
		}
		creates = append(creates, c.createIndexSQL(r, x))
	}

	for _, fk := range fks {
		if fk.Drop || fk.Orig != "" {
			continue
		}
		if !c.CanAlterFK() {
			return nil, errors.New("SQLite는 기존 테이블에 외래 키를 추가할 수 없습니다 (새 테이블을 만들 때만 정의할 수 있습니다)")
		}
		clause, err := c.fkClause(r, fk, live)
		if err != nil {
			return nil, err
		}
		addFK = append(addFK, "ALTER TABLE "+qt+" ADD "+clause)
	}

	var stmts []string
	for _, group := range [][]string{dropFK, dropIdx, drops, renames, modifies, adds, creates, addFK} {
		stmts = append(stmts, group...)
	}
	return stmts, nil
}

// ExecDDL runs statements in order. PostgreSQL and SQLite run them in one
// transaction (all or nothing); MySQL commits DDL implicitly, so the number of
// statements already applied is reported on failure.
func (c *Conn) ExecDDL(ctx context.Context, r Ref, stmts []string) (applied int, err error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return 0, err
	}
	if c.Config.Kind == MySQL {
		for i, s := range stmts {
			if _, err := p.ExecContext(ctx, s); err != nil {
				return i, fmt.Errorf("%w\n문: %s", err, s)
			}
		}
		return len(stmts), nil
	}
	tx, err := p.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("%w\n문: %s\n(모든 변경이 취소되었습니다)", err, s)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(stmts), nil
}
