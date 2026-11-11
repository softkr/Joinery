package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ForeignKey is one foreign-key constraint (possibly over several columns).
type ForeignKey struct {
	Table      string // table that owns the constraint
	Name       string // empty for unnamed SQLite constraints
	Columns    []string
	RefSchema  string // schema (PostgreSQL) / database (MySQL) of the referenced table; empty for SQLite
	RefTable   string
	RefColumns []string // may be empty when SQLite refers to the primary key implicitly
	OnUpdate   string
	OnDelete   string
}

// FKSpec is one row of the designer's foreign-key tab. Existing constraints can
// only be dropped; new ones (Orig == "") are created.
type FKSpec struct {
	Orig       string
	Name       string
	Columns    []string
	RefTable   string // "table" or "schema.table"
	RefColumns []string
	OnUpdate   string
	OnDelete   string
	Drop       bool
}

// FKActions are the referential actions the designer offers ("" = database default).
var FKActions = []string{"", "NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT"}

func validFKAction(a string) bool {
	for _, x := range FKActions {
		if x == a {
			return true
		}
	}
	return false
}

// CanAlterFK reports whether constraints can be added to / dropped from an
// existing table. SQLite can only declare them when creating the table.
func (c *Conn) CanAlterFK() bool { return c.Config.Kind != SQLite }

// ForeignKeys lists the foreign keys of one table.
func (c *Conn) ForeignKeys(ctx context.Context, r Ref) ([]ForeignKey, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	return c.d.foreignKeys(ctx, p, r, false)
}

// SchemaForeignKeys lists the foreign keys of every table of r's database/schema.
func (c *Conn) SchemaForeignKeys(ctx context.Context, r Ref) ([]ForeignKey, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	return c.d.foreignKeys(ctx, p, r, true)
}

// fkKey groups the rows of a multi-column constraint.
type fkKey struct{ table, name string }

func scanForeignKeys(rows *sql.Rows, scan func() (fkKey, string, string, ForeignKey, error)) ([]ForeignKey, error) {
	defer rows.Close()
	var out []ForeignKey
	idx := map[fkKey]int{}
	for rows.Next() {
		k, col, refCol, base, err := scan()
		if err != nil {
			return nil, err
		}
		i, ok := idx[k]
		if !ok {
			base.Table, base.Name = k.table, k.name
			out = append(out, base)
			i = len(out) - 1
			idx[k] = i
		}
		out[i].Columns = append(out[i].Columns, col)
		if refCol != "" {
			out[i].RefColumns = append(out[i].RefColumns, refCol)
		}
	}
	return out, rows.Err()
}

func pgAction(c string) string {
	switch c {
	case "r":
		return "RESTRICT"
	case "c":
		return "CASCADE"
	case "n":
		return "SET NULL"
	case "d":
		return "SET DEFAULT"
	}
	return "NO ACTION"
}

// parseRefTable splits "schema.table" (or "table") into its parts.
func parseRefTable(s string) (qualifier, table string) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "."); i > 0 && i < len(s)-1 {
		return s[:i], s[i+1:]
	}
	return "", s
}

// fkName builds a default constraint name like fk_orders_customer_id.
func fkName(table string, cols []string) string {
	var sb strings.Builder
	for _, r := range "fk_" + table + "_" + strings.Join(cols, "_") {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r > 127 {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	n := sb.String()
	if len(n) > 60 {
		n = n[:60]
	}
	return n
}

// fkClause renders "CONSTRAINT name FOREIGN KEY (...) REFERENCES t (...) ON ...",
// validating the spec against the fields the table will have.
func (c *Conn) fkClause(r Ref, fk FKSpec, live []FieldSpec) (string, error) {
	if len(fk.Columns) == 0 {
		return "", errors.New("외래 키: 필드를 지정하세요")
	}
	for _, col := range fk.Columns {
		ok := false
		for _, f := range live {
			if f.Name == col {
				ok = true
				break
			}
		}
		if !ok {
			return "", fmt.Errorf("외래 키: 알 수 없는 필드 %q", col)
		}
	}
	qualifier, refTable := parseRefTable(fk.RefTable)
	if err := validIdent("참조 테이블", refTable); err != nil {
		return "", err
	}
	if len(fk.RefColumns) != len(fk.Columns) {
		return "", fmt.Errorf("외래 키: 필드(%d개)와 참조 필드(%d개)의 수가 같아야 합니다", len(fk.Columns), len(fk.RefColumns))
	}
	for _, col := range fk.RefColumns {
		if err := validIdent("참조 필드", col); err != nil {
			return "", err
		}
	}
	if !validFKAction(fk.OnDelete) || !validFKAction(fk.OnUpdate) {
		return "", errors.New("외래 키: 알 수 없는 참조 동작입니다")
	}
	name := strings.TrimSpace(fk.Name)
	if name == "" {
		name = fkName(r.Table, fk.Columns)
	}
	if err := validIdent("외래 키", name); err != nil {
		return "", err
	}

	ref := Ref{Database: r.Database, Schema: r.Schema, Table: refTable}
	if qualifier != "" {
		switch c.Config.Kind {
		case Postgres:
			ref = Ref{Schema: qualifier, Table: refTable}
		case MySQL:
			ref = Ref{Database: qualifier, Table: refTable}
		default:
			return "", errors.New("SQLite에서는 다른 스키마의 테이블을 참조할 수 없습니다")
		}
	}
	quote := func(cols []string) string {
		q := make([]string, len(cols))
		for i, col := range cols {
			q[i] = c.d.quote(col)
		}
		return strings.Join(q, ", ")
	}
	s := "CONSTRAINT " + c.d.quote(name) + " FOREIGN KEY (" + quote(fk.Columns) + ") REFERENCES " +
		c.d.qualified(ref) + " (" + quote(fk.RefColumns) + ")"
	if fk.OnDelete != "" {
		s += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" {
		s += " ON UPDATE " + fk.OnUpdate
	}
	return s, nil
}

func (c *Conn) dropFKSQL(r Ref, name string) string {
	if c.Config.Kind == MySQL {
		return "ALTER TABLE " + c.d.qualified(r) + " DROP FOREIGN KEY " + c.d.quote(name)
	}
	return "ALTER TABLE " + c.d.qualified(r) + " DROP CONSTRAINT " + c.d.quote(name)
}
