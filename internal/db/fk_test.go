package db

import (
	"context"
	"strings"
	"testing"
)

func TestForeignKeysSQLite(t *testing.T) {
	ctx := context.Background()
	c := openTestDB(t)

	// parent + child with a named, cascading FK declared at CREATE time
	for _, spec := range []struct {
		table  string
		fields []FieldSpec
		fks    []FKSpec
	}{
		{"authors", []FieldSpec{{Name: "id", Type: "INTEGER", NotNull: true, PK: true}, {Name: "name", Type: "TEXT"}}, nil},
		{"books", []FieldSpec{{Name: "id", Type: "INTEGER", NotNull: true, PK: true}, {Name: "author_id", Type: "INTEGER"}},
			[]FKSpec{{Name: "fk_books_author", Columns: []string{"author_id"}, RefTable: "authors", RefColumns: []string{"id"}, OnDelete: "CASCADE"}}},
	} {
		stmts, err := c.PlanCreate(Ref{Table: spec.table}, spec.fields, nil, spec.fks)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.ExecDDL(ctx, Ref{Table: spec.table}, stmts); err != nil {
			t.Fatalf("%s: %v\n%v", spec.table, err, stmts)
		}
	}

	fks, err := c.ForeignKeys(ctx, Ref{Table: "books"})
	if err != nil || len(fks) != 1 {
		t.Fatalf("fks = %+v, %v", fks, err)
	}
	fk := fks[0]
	if fk.Name != "" {
		t.Errorf("SQLite constraints are unnamed, got %q", fk.Name)
	}
	if fk.Table != "books" || fk.RefTable != "authors" || fk.OnDelete != "CASCADE" ||
		strings.Join(fk.Columns, ",") != "author_id" || strings.Join(fk.RefColumns, ",") != "id" {
		t.Fatalf("fk = %+v", fk)
	}
	if none, _ := c.ForeignKeys(ctx, Ref{Table: "authors"}); len(none) != 0 {
		t.Fatalf("authors has no FK: %+v", none)
	}
	all, err := c.SchemaForeignKeys(ctx, Ref{})
	if err != nil || len(all) != 1 || all[0].Table != "books" {
		t.Fatalf("schema-wide = %+v, %v", all, err)
	}

	// the constraint is enforced and cascades (foreign_keys pragma is on)
	for _, q := range []string{"INSERT INTO authors VALUES (1, 'a')", "INSERT INTO books VALUES (1, 1)"} {
		if _, err := c.Run(ctx, Ref{}, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Run(ctx, Ref{}, "INSERT INTO books VALUES (2, 99)"); err == nil {
		t.Error("orphan insert must violate the foreign key")
	}
	if _, err := c.Run(ctx, Ref{}, "DELETE FROM authors WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.Count(ctx, Ref{Table: "books"}, ""); n != 0 {
		t.Errorf("ON DELETE CASCADE left %d books", n)
	}

	// existing SQLite tables cannot get or lose constraints
	cols := []FieldSpec{{Orig: "id", Name: "id", Type: "INTEGER", NotNull: true, PK: true}, {Orig: "author_id", Name: "author_id", Type: "INTEGER"}}
	if _, err := c.PlanAlter(ctx, Ref{Table: "books"}, cols, nil, []FKSpec{{Name: "x", Columns: []string{"author_id"}, RefTable: "authors", RefColumns: []string{"id"}}}); err == nil {
		t.Error("adding an FK to an existing SQLite table must be refused")
	}
	if _, err := c.PlanAlter(ctx, Ref{Table: "books"}, cols, nil, []FKSpec{{Orig: "fk_books_author", Drop: true}}); err == nil {
		t.Error("dropping an FK from an existing SQLite table must be refused")
	}
	// leaving existing FKs untouched is a no-op
	if stmts, err := c.PlanAlter(ctx, Ref{Table: "books"}, cols, nil, nil); err != nil || len(stmts) != 0 {
		t.Errorf("no-op = %v, %v", stmts, err)
	}
}

func TestForeignKeyClauses(t *testing.T) {
	fields := []FieldSpec{{Name: "id", Type: "int", PK: true, NotNull: true}, {Name: "author_id", Type: "int"}, {Name: "editor_id", Type: "int"}}
	pg := &Conn{Config: Config{Kind: Postgres}, d: postgresDialect{}}
	my := &Conn{Config: Config{Kind: MySQL}, d: mysqlDialect{}}
	fk := FKSpec{Columns: []string{"author_id", "editor_id"}, RefTable: "authors", RefColumns: []string{"id", "ed"}, OnDelete: "CASCADE", OnUpdate: "SET NULL"}

	got, err := pg.fkClause(Ref{Schema: "public", Table: "books"}, fk, fields)
	want := `CONSTRAINT "fk_books_author_id_editor_id" FOREIGN KEY ("author_id", "editor_id") REFERENCES "public"."authors" ("id", "ed") ON DELETE CASCADE ON UPDATE SET NULL`
	if err != nil || got != want {
		t.Errorf("pg clause:\n got %q (%v)\nwant %q", got, err, want)
	}
	got, _ = my.fkClause(Ref{Database: "shop", Table: "books"}, fk, fields)
	if want := "CONSTRAINT `fk_books_author_id_editor_id` FOREIGN KEY (`author_id`, `editor_id`) REFERENCES `shop`.`authors` (`id`, `ed`) ON DELETE CASCADE ON UPDATE SET NULL"; got != want {
		t.Errorf("mysql clause:\n got %q\nwant %q", got, want)
	}

	// another schema / database is addressed explicitly
	other := FKSpec{Name: "fk_x", Columns: []string{"author_id"}, RefTable: "hr.people", RefColumns: []string{"id"}}
	if got, _ := pg.fkClause(Ref{Schema: "public", Table: "books"}, other, fields); !strings.Contains(got, `REFERENCES "hr"."people" ("id")`) {
		t.Errorf("pg cross-schema = %s", got)
	}
	if got, _ := my.fkClause(Ref{Database: "shop", Table: "books"}, other, fields); !strings.Contains(got, "REFERENCES `hr`.`people` (`id`)") {
		t.Errorf("mysql cross-db = %s", got)
	}
	lite := &Conn{Config: Config{Kind: SQLite}, d: sqliteDialect{}}
	if _, err := lite.fkClause(Ref{Table: "books"}, other, fields); err == nil {
		t.Error("SQLite must reject a schema-qualified target")
	}

	for name, bad := range map[string]FKSpec{
		"no columns":        {RefTable: "a", RefColumns: []string{"id"}},
		"unknown column":    {Columns: []string{"nope"}, RefTable: "a", RefColumns: []string{"id"}},
		"count mismatch":    {Columns: []string{"author_id"}, RefTable: "a", RefColumns: []string{"id", "x"}},
		"no ref table":      {Columns: []string{"author_id"}, RefColumns: []string{"id"}},
		"bad action":        {Columns: []string{"author_id"}, RefTable: "a", RefColumns: []string{"id"}, OnDelete: "CASCADE; DROP TABLE x"},
		"bad update action": {Columns: []string{"author_id"}, RefTable: "a", RefColumns: []string{"id"}, OnUpdate: "explode"},
		"empty ref column":  {Columns: []string{"author_id"}, RefTable: "a", RefColumns: []string{" "}},
	} {
		if _, err := pg.fkClause(Ref{Schema: "public", Table: "books"}, bad, fields); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	if s := pg.dropFKSQL(Ref{Schema: "public", Table: "books"}, "fk_a"); s != `ALTER TABLE "public"."books" DROP CONSTRAINT "fk_a"` {
		t.Errorf("pg drop = %s", s)
	}
	if s := my.dropFKSQL(Ref{Database: "shop", Table: "books"}, "fk_a"); s != "ALTER TABLE `shop`.`books` DROP FOREIGN KEY `fk_a`" {
		t.Errorf("mysql drop = %s", s)
	}
	if !pg.CanAlterFK() || !my.CanAlterFK() || lite.CanAlterFK() {
		t.Error("CanAlterFK: PG and MySQL yes, SQLite no")
	}
	if got := fkName("my table", []string{"a-b", "c"}); got != "fk_my_table_a_b_c" {
		t.Errorf("fkName = %q", got)
	}
	if n := fkName(strings.Repeat("t", 80), []string{"c"}); len(n) != 60 {
		t.Errorf("fkName not capped: %d", len(n))
	}

	// PlanCreate puts the constraint into the CREATE TABLE body
	stmts, err := pg.PlanCreate(Ref{Schema: "public", Table: "books"}, fields, nil, []FKSpec{other})
	if err != nil || !strings.Contains(stmts[0], "  CONSTRAINT \"fk_x\" FOREIGN KEY (\"author_id\") REFERENCES \"hr\".\"people\" (\"id\")\n)") {
		t.Errorf("create plan = %v, %v", stmts, err)
	}
}
