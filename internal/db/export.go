package db

import (
	"context"
	"fmt"
)

// ExportTable streams every row matching the page options (paging is
// ignored) to emit. emit is first called with the header and a nil row, then
// once per row.
func (c *Conn) ExportTable(ctx context.Context, r Ref, o PageOpts, emit func(cols []string, row []Cell) error) error {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return err
	}
	cols, err := c.d.columns(ctx, p, r)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return fmt.Errorf("%s: 컬럼을 찾을 수 없습니다", r.Table)
	}
	q, err := c.selectSQL(cols, r, o)
	if err != nil {
		return err
	}
	rows, err := p.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return err
	}
	if err := emit(names, nil); err != nil {
		return err
	}
	vals := make([]any, len(names))
	ptrs := make([]any, len(names))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row := make([]Cell, len(names))
		for i, v := range vals {
			row[i] = toCell(v)
		}
		if err := emit(names, row); err != nil {
			return err
		}
	}
	return rows.Err()
}
