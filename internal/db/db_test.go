package db

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *Conn {
	t.Helper()
	ctx := context.Background()
	c, err := Open(ctx, Config{Kind: SQLite, Database: filepath.Join(t.TempDir(), "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	for _, q := range []string{
		`CREATE TABLE users(id INTEGER PRIMARY KEY, name TEXT NOT NULL, age INTEGER)`,
		`INSERT INTO users(name, age) VALUES ('a', 1), ('b', NULL), ('c', 3)`,
		`CREATE TABLE nopk(x TEXT)`,
		`CREATE VIEW v AS SELECT name FROM users`,
	} {
		if _, err := c.Run(ctx, Ref{}, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return c
}

func TestMetadata(t *testing.T) {
	ctx := context.Background()
	c := openTestDB(t)
	tabs, err := c.Tables(ctx, Ref{})
	if err != nil {
		t.Fatal(err)
	}
	views := 0
	for _, tb := range tabs {
		if tb.IsView {
			views++
		}
	}
	if len(tabs) != 3 || views != 1 {
		t.Fatalf("tables=%+v", tabs)
	}
	cols, err := c.Columns(ctx, Ref{Table: "users"})
	if err != nil || len(cols) != 3 || !cols[0].PK || cols[1].Nullable {
		t.Fatalf("cols=%+v err=%v", cols, err)
	}
}

func TestTablePageSortFilterPaging(t *testing.T) {
	ctx := context.Background()
	c := openTestDB(t)
	r := Ref{Table: "users"}

	p, err := c.TablePage(ctx, r, PageOpts{Sort: "name", Desc: true, Limit: 2})
	if err != nil || len(p.Rows) != 2 || !p.More || p.Rows[0][1].V != "c" {
		t.Fatalf("page=%+v err=%v", p, err)
	}
	p, err = c.TablePage(ctx, r, PageOpts{Limit: 2, Offset: 2})
	if err != nil || len(p.Rows) != 1 || p.More {
		t.Fatalf("page2=%+v err=%v", p, err)
	}
	p, _ = c.TablePage(ctx, r, PageOpts{Where: "age IS NULL", Limit: 10})
	if len(p.Rows) != 1 || !p.Rows[0][2].Null {
		t.Fatalf("null filter=%+v", p)
	}
	// Unknown sort column is ignored instead of being spliced into SQL.
	if _, err := c.TablePage(ctx, r, PageOpts{Sort: "name; DROP TABLE users", Limit: 10}); err != nil {
		t.Fatalf("bad sort should be ignored: %v", err)
	}
	if _, err := c.TablePage(ctx, r, PageOpts{Where: "1=1; DROP TABLE users", Limit: 10}); err == nil {
		t.Fatal("semicolon in WHERE must be rejected")
	}
	if n, err := c.Count(ctx, r, "age > 0"); err != nil || n != 2 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}

func TestUpdateAndDeleteByPK(t *testing.T) {
	ctx := context.Background()
	c := openTestDB(t)
	r := Ref{Table: "users"}
	pk := map[string]string{"id": "1"}

	if err := c.UpdateCell(ctx, r, pk, "name", Cell{V: "z"}); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateCell(ctx, r, pk, "age", Cell{Null: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateCell(ctx, r, pk, "name", Cell{Null: true}); err == nil {
		t.Fatal("NOT NULL violation should fail")
	}
	if err := c.UpdateCell(ctx, r, pk, "nope", Cell{V: "x"}); err == nil {
		t.Fatal("unknown column must be rejected")
	}
	if err := c.UpdateCell(ctx, r, map[string]string{}, "name", Cell{V: "x"}); err == nil {
		t.Fatal("missing pk must be rejected")
	}
	if err := c.UpdateCell(ctx, r, map[string]string{"id": "999"}, "name", Cell{V: "x"}); err == nil {
		t.Fatal("update of a non-existent row must fail")
	}
	if err := c.UpdateCell(ctx, Ref{Table: "nopk"}, pk, "x", Cell{V: "x"}); err == nil {
		t.Fatal("table without pk must be read-only")
	}
	if err := c.DeleteRow(ctx, r, pk); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.Count(ctx, r, ""); n != 2 {
		t.Fatalf("count after delete = %d", n)
	}
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	c := openTestDB(t)
	res, err := c.Run(ctx, Ref{}, "SELECT id, name FROM users ORDER BY id")
	if err != nil || !res.IsQuery || len(res.Rows) != 3 || len(res.Columns) != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	res, err = c.Run(ctx, Ref{}, "UPDATE users SET age = 9")
	if err != nil || res.IsQuery || res.Affected != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := c.Run(ctx, Ref{}, "  "); err == nil {
		t.Fatal("empty statement must fail")
	}
	if _, err := c.Run(ctx, Ref{}, "SELEC 1"); err == nil {
		t.Fatal("syntax error must surface")
	}
}

func TestReturnsRows(t *testing.T) {
	type tc struct {
		kind Kind
		stmt string
		want bool
	}
	for _, c := range []tc{
		{SQLite, "select 1", true}, {SQLite, "  WITH x AS (select 1) select * from x", true}, {SQLite, "(select 1)", true},
		{SQLite, "PRAGMA table_info(t)", true}, {SQLite, "insert into t values (1)", false}, {SQLite, "", false}, {SQLite, "(", false},
		{SQLite, "-- note\nSELECT 1", true}, {SQLite, "/* hi */ select 1", true},
		{Postgres, "INSERT INTO t VALUES (1) RETURNING id", true}, {Postgres, "insert into t values (1)", false},
		{SQLite, "DELETE FROM t RETURNING *", true}, {MySQL, "INSERT INTO t VALUES (1)", false},
		{MySQL, "# c\nSHOW TABLES", true}, {MySQL, "SELECT 1", true}, {MySQL, "/*! SET x=1 */", false},
	} {
		if got := returnsRows(c.kind, c.stmt); got != c.want {
			t.Errorf("returnsRows(%v, %q) = %v, want %v", c.kind, c.stmt, got, c.want)
		}
	}
}

func TestAltersSession(t *testing.T) {
	for stmt, want := range map[string]bool{
		"BEGIN": true, "begin;": true, "START TRANSACTION": true, "SET search_path = x": true, "-- c\nPRAGMA foreign_keys=1": true,
		"SELECT 1": false, "UPDATE t SET a = 1": false, "": false,
	} {
		if got := altersSession(SQLite, stmt); got != want {
			t.Errorf("altersSession(%q) = %v, want %v", stmt, got, want)
		}
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, Config{Kind: SQLite}); err == nil {
		t.Error("sqlite without path must fail")
	}
	if _, err := Open(ctx, Config{Kind: "oracle"}); err == nil {
		t.Error("unknown kind must fail")
	}
}

func TestInsertRow(t *testing.T) {
	ctx := context.Background()
	c := openTestDB(t)
	r := Ref{Table: "users"}

	// id omitted -> rowid alias fills it; NULL age; explicit name.
	err := c.InsertRow(ctx, r, []InsertVal{{Col: "name", Cell: Cell{V: "d'Artagnan"}}, {Col: "age", Cell: Cell{Null: true}}})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.TablePage(ctx, r, PageOpts{Where: "name LIKE 'd%'", Limit: 5})
	if len(p.Rows) != 1 || p.Rows[0][1].V != "d'Artagnan" || !p.Rows[0][2].Null {
		t.Fatalf("inserted row = %+v", p.Rows)
	}
	if err := c.InsertRow(ctx, r, []InsertVal{{Col: "nope", Cell: Cell{V: "x"}}}); err == nil {
		t.Error("unknown column must be rejected")
	}
	if err := c.InsertRow(ctx, r, []InsertVal{{Col: "name", Cell: Cell{V: "a"}}, {Col: "name", Cell: Cell{V: "b"}}}); err == nil {
		t.Error("duplicate column must be rejected")
	}
	if err := c.InsertRow(ctx, r, nil); err == nil {
		t.Error("NOT NULL name without value must fail")
	}
	if err := c.InsertRow(ctx, Ref{Table: "nopk"}, nil); err != nil { // DEFAULT VALUES
		t.Errorf("default values insert: %v", err)
	}
}

func TestDesignerCreateAndAlterSQLite(t *testing.T) {
	ctx := context.Background()
	c := openTestDB(t)
	r := Ref{Table: "items"}

	stmts, err := c.PlanCreate(r, []FieldSpec{
		{Name: "id", Type: "INTEGER", NotNull: true, PK: true},
		{Name: "title", Type: "TEXT", NotNull: true, Default: "'untitled'"},
	}, []IndexSpec{{Name: "idx_title", Columns: []string{"title"}, Unique: true}}, nil)
	if err != nil || len(stmts) != 2 {
		t.Fatalf("create plan = %v, %v", stmts, err)
	}
	if n, err := c.ExecDDL(ctx, r, stmts); err != nil || n != 2 {
		t.Fatalf("exec = %d, %v", n, err)
	}

	// add + rename + drop index, all in one plan
	stmts, err = c.PlanAlter(ctx, r, []FieldSpec{
		{Orig: "id", Name: "id", Type: "INTEGER", NotNull: true, PK: true},
		{Orig: "title", Name: "name", Type: "TEXT", NotNull: true, Default: "'untitled'"},
		{Name: "qty", Type: "INTEGER", Default: "0"},
	}, []IndexSpec{{Orig: "idx_title", Drop: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 3 || stmts[0] != `DROP INDEX "idx_title"` {
		t.Fatalf("alter plan = %v", stmts)
	}
	if _, err := c.ExecDDL(ctx, r, stmts); err != nil {
		t.Fatal(err)
	}
	cols, _ := c.Columns(ctx, r)
	if len(cols) != 3 || cols[1].Name != "name" || cols[2].Name != "qty" {
		t.Fatalf("columns after alter = %+v", cols)
	}

	// SQLite cannot change attributes of existing columns: must be refused, not guessed.
	if _, err := c.PlanAlter(ctx, r, []FieldSpec{
		{Orig: "id", Name: "id", Type: "INTEGER", NotNull: true, PK: true},
		{Orig: "name", Name: "name", Type: "VARCHAR(10)", NotNull: true, Default: "'untitled'"},
		{Orig: "qty", Name: "qty", Type: "INTEGER", Default: "0"},
	}, nil, nil); err == nil {
		t.Error("type change on SQLite must be rejected")
	}
	// PK changes are refused everywhere.
	if _, err := c.PlanAlter(ctx, r, []FieldSpec{
		{Orig: "id", Name: "id", Type: "INTEGER", NotNull: true, PK: false},
		{Orig: "name", Name: "name", Type: "TEXT", NotNull: true, Default: "'untitled'"},
		{Orig: "qty", Name: "qty", Type: "INTEGER", Default: "0"},
	}, nil, nil); err == nil {
		t.Error("PK change must be rejected")
	}
	// unchanged -> empty plan
	stmts, err = c.PlanAlter(ctx, r, []FieldSpec{
		{Orig: "id", Name: "id", Type: "INTEGER", NotNull: true, PK: true},
		{Orig: "name", Name: "name", Type: "TEXT", NotNull: true, Default: "'untitled'"},
		{Orig: "qty", Name: "qty", Type: "INTEGER", Default: "0"},
	}, nil, nil)
	if err != nil || len(stmts) != 0 {
		t.Errorf("no-op plan = %v, %v", stmts, err)
	}
}

func TestDesignerRejectsBadInput(t *testing.T) {
	c := &Conn{Config: Config{Kind: Postgres}, d: postgresDialect{}}
	ok := FieldSpec{Name: "a", Type: "integer"}
	for name, f := range map[string]FieldSpec{
		"empty name":    {Name: " ", Type: "integer"},
		"injected type": {Name: "a", Type: "int); DROP TABLE x; --"},
		"empty type":    {Name: "a"},
		"bad default":   {Name: "a", Type: "int", Default: "1; DROP TABLE x"},
	} {
		if _, err := c.PlanCreate(Ref{Schema: "public", Table: "t"}, []FieldSpec{f}, nil, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := c.PlanCreate(Ref{Table: "t"}, []FieldSpec{ok, {Name: "A", Type: "int"}}, nil, nil); err == nil {
		t.Error("duplicate (case-insensitive) names accepted")
	}
	if _, err := c.PlanCreate(Ref{Table: ""}, []FieldSpec{ok}, nil, nil); err == nil {
		t.Error("empty table name accepted")
	}
	if _, err := c.PlanCreate(Ref{Table: "t"}, []FieldSpec{ok}, []IndexSpec{{Name: "i", Columns: []string{"zzz"}}}, nil); err == nil {
		t.Error("index on unknown column accepted")
	}
}

func TestDialectSQLGeneration(t *testing.T) {
	spec := []FieldSpec{{Name: "id", Type: "int", NotNull: true, PK: true}, {Name: "n", Type: "varchar(20)"}}
	pg := &Conn{Config: Config{Kind: Postgres}, d: postgresDialect{}}
	my := &Conn{Config: Config{Kind: MySQL}, d: mysqlDialect{}}

	got, _ := pg.PlanCreate(Ref{Schema: "public", Table: "t"}, spec, nil, nil)
	want := "CREATE TABLE \"public\".\"t\" (\n  \"id\" int NOT NULL,\n  \"n\" varchar(20),\n  PRIMARY KEY (\"id\")\n)"
	if got[0] != want {
		t.Errorf("pg create:\n%s\nwant:\n%s", got[0], want)
	}
	got, _ = my.PlanCreate(Ref{Database: "shop", Table: "t"}, spec, nil, nil)
	if want := "CREATE TABLE `shop`.`t` (\n  `id` int NOT NULL,\n  `n` varchar(20),\n  PRIMARY KEY (`id`)\n)"; got[0] != want {
		t.Errorf("mysql create:\n%s", got[0])
	}
	if s := pg.dropIndexSQL(Ref{Schema: "public", Table: "t"}, "ix"); s != `DROP INDEX "public"."ix"` {
		t.Errorf("pg drop index = %s", s)
	}
	if s := my.dropIndexSQL(Ref{Database: "shop", Table: "t"}, "ix"); s != "DROP INDEX `ix` ON `shop`.`t`" {
		t.Errorf("mysql drop index = %s", s)
	}
	if !pg.CanAlterFields() || my.CanAlterFields() {
		t.Error("only PostgreSQL may alter field attributes")
	}
}

func TestExportTable(t *testing.T) {
	ctx := context.Background()
	c := openTestDB(t)
	var header []string
	var rows [][]Cell
	err := c.ExportTable(ctx, Ref{Table: "users"}, PageOpts{Sort: "id", Where: "age IS NOT NULL"}, func(cols []string, row []Cell) error {
		if row == nil {
			header = cols
			return nil
		}
		rows = append(rows, row)
		return nil
	})
	if err != nil || len(header) != 3 || len(rows) != 2 {
		t.Fatalf("export: header=%v rows=%d err=%v", header, len(rows), err)
	}
	if !rows[0][0].Lit || rows[0][1].Lit {
		t.Errorf("numbers must be literals and text quoted: %+v", rows[0])
	}
}
