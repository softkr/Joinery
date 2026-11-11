package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// InsertVal is an explicit value for one column. Columns left out of the
// slice passed to InsertRow take their DEFAULT.
type InsertVal struct {
	Col  string
	Cell Cell
}

// InsertRow inserts one row. Column names are validated against metadata and
// values are always bound as parameters.
func (c *Conn) InsertRow(ctx context.Context, r Ref, vals []InsertVal) error {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return err
	}
	cols, err := c.d.columns(ctx, p, r)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(vals))
	holders := make([]string, 0, len(vals))
	args := make([]any, 0, len(vals))
	for _, v := range vals {
		if _, ok := findColumn(cols, v.Col); !ok {
			return fmt.Errorf("알 수 없는 컬럼: %s", v.Col)
		}
		if seen[v.Col] {
			return fmt.Errorf("컬럼이 중복되었습니다: %s", v.Col)
		}
		seen[v.Col] = true
		names = append(names, c.d.quote(v.Col))
		holders = append(holders, c.d.placeholder(len(args)+1))
		if v.Cell.Null {
			args = append(args, nil)
		} else {
			args = append(args, v.Cell.V)
		}
	}
	q := "INSERT INTO " + c.d.qualified(r)
	switch {
	case len(names) > 0:
		q += " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(holders, ", ") + ")"
	case c.Config.Kind == MySQL:
		q += " () VALUES ()"
	default:
		q += " DEFAULT VALUES"
	}
	res, err := p.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("행이 추가되지 않았습니다")
	}
	return nil
}

// AutoDefault reports whether the database fills the column on its own
// (default value, auto increment, rowid alias), so the insert form can
// preselect "default" for it.
func (c *Conn) AutoDefault(col Column) bool {
	if col.Default != "" || strings.Contains(strings.ToLower(col.Extra), "auto_increment") ||
		strings.Contains(strings.ToLower(col.Extra), "generated") {
		return true
	}
	return c.Config.Kind == SQLite && col.PK && strings.EqualFold(col.Type, "INTEGER")
}
