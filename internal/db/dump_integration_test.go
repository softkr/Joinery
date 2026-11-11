package db

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Backup/restore round trips against real servers (same env vars as
// integration_test.go). A source schema/database is filled with mixed
// objects, dumped, restored into a fresh one, and both are dumped again: the
// two scripts must match, and a few values are checked directly.

const (
	dumpSrcName = "it_dump_src"
	dumpDstName = "it_dump_dst"
)

const pgDumpSetup = `
CREATE TABLE parent (
  id serial PRIMARY KEY,
  name text NOT NULL UNIQUE,
  created timestamptz NOT NULL DEFAULT '2024-01-02 03:04:05+00',
  blob bytea,
  amount numeric(10,2),
  flag boolean DEFAULT true,
  doc jsonb,
  tags text[]
);
CREATE TABLE child (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  parent_id int NOT NULL REFERENCES parent(id) ON DELETE CASCADE,
  note text
);
CREATE INDEX child_note_idx ON child (lower(note));
INSERT INTO parent (name, blob, amount, flag, doc, tags) VALUES
  ('it''s a "test" \ back', '\xdeadbeef', 12.50, false, '{"a": [1, 2]}', '{x,"y z"}'),
  ('유니코드 ☃' || E'\n' || 'line2', NULL, NULL, NULL, NULL, NULL);
INSERT INTO child (parent_id, note) VALUES (1, 'one'), (1, NULL), (2, 'two');
CREATE VIEW parent_names AS SELECT id, name FROM parent;
CREATE FUNCTION touch_note() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.note := coalesce(NEW.note, '') || '!'; RETURN NEW; END $$;
CREATE TRIGGER child_touch BEFORE INSERT ON child FOR EACH ROW EXECUTE FUNCTION touch_note();
CREATE FUNCTION add_one(i int) RETURNS int LANGUAGE sql AS 'SELECT i + 1';
`

const mysqlDumpSetup = `
CREATE TABLE parent (
  id INT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(100) NOT NULL,
  created DATETIME DEFAULT '2024-01-02 03:04:05',
  blob_col BLOB,
  amount DECIMAL(10,2),
  flag TINYINT(1) DEFAULT 1,
  doc JSON,
  kind ENUM('a','b') DEFAULT 'a',
  UNIQUE KEY uq_name (name)
) ENGINE=InnoDB;
CREATE TABLE child (
  id INT AUTO_INCREMENT PRIMARY KEY,
  parent_id INT NOT NULL,
  note TEXT,
  KEY idx_parent (parent_id),
  CONSTRAINT fk_child_parent FOREIGN KEY (parent_id) REFERENCES parent(id) ON DELETE CASCADE
) ENGINE=InnoDB;
INSERT INTO parent (name, blob_col, amount, flag, doc, kind) VALUES
  ('it''s a "test" \\ back', 0xDEADBEEF, 12.50, 0, '{"a": [1, 2]}', 'b'),
  (CONCAT('유니코드 ☃', CHAR(10), 'line2'), NULL, NULL, NULL, NULL, 'a');
INSERT INTO child (parent_id, note) VALUES (1, 'one'), (1, NULL), (2, 'two');
CREATE VIEW parent_names AS SELECT id, name FROM parent;
DELIMITER ;;
CREATE TRIGGER child_touch BEFORE INSERT ON child FOR EACH ROW BEGIN
  SET NEW.note = CONCAT(IFNULL(NEW.note, ''), '!');
END;;
CREATE FUNCTION add_one(i INT) RETURNS INT DETERMINISTIC BEGIN
  RETURN i + 1;
END;;
CREATE PROCEDURE bump(IN pid INT) BEGIN
  UPDATE parent SET amount = IFNULL(amount, 0) + 1 WHERE id = pid;
END;;
DELIMITER ;
`

func TestIntegrationDumpPostgres(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_PG", Postgres)
	cfg.Database = "postgres"
	src := Ref{Database: "postgres", Schema: dumpSrcName}
	dst := Ref{Database: "postgres", Schema: dumpDstName}
	runDumpRoundTrip(t, cfg, src, dst, pgDumpSetup, func(n string) string { return "CREATE SCHEMA " + n }, func(n string) string { return "DROP SCHEMA IF EXISTS " + n + " CASCADE" },
		"SELECT name FROM parent WHERE id = 1", "SELECT encode(blob, 'hex') FROM parent WHERE id = 1")
}

func TestIntegrationDumpMySQL(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_MYSQL", MySQL)
	cfg.Database = "shop"
	runMySQLDump(t, cfg)
}

func TestIntegrationDumpMariaDB(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_MARIADB", MySQL)
	cfg.Database = "shop"
	runMySQLDump(t, cfg)
}

func runMySQLDump(t *testing.T, cfg Config) {
	runDumpRoundTrip(t, cfg, Ref{Database: dumpSrcName}, Ref{Database: dumpDstName}, mysqlDumpSetup,
		func(n string) string { return "CREATE DATABASE " + n + " CHARACTER SET utf8mb4" },
		func(n string) string { return "DROP DATABASE IF EXISTS " + n },
		"SELECT name FROM parent WHERE id = 1", "SELECT LOWER(HEX(blob_col)) FROM parent WHERE id = 1")
}

func runDumpRoundTrip(t *testing.T, cfg Config, src, dst Ref, setup string, create, drop func(string) string, nameQuery, blobQuery string) {
	t.Helper()
	ctx := context.Background()
	c := openWithRetry(t, cfg)
	admin := Ref{Database: cfg.Database}
	name := func(r Ref) string { return orDefault(r.Schema, r.Database) }
	for _, r := range []Ref{src, dst} {
		if _, err := c.Run(ctx, admin, drop(name(r))); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = c.Run(ctx, admin, drop(name(r))) })
		if _, err := c.Run(ctx, admin, create(name(r))); err != nil {
			t.Fatal(err)
		}
	}
	if sum, err := c.ExecScript(ctx, src, setup, false); err != nil || sum.Failed != 0 {
		t.Fatalf("setup: %+v %v", sum, err)
	}

	checkObjectLists(t, c, src)

	opts := DumpOptions{Structure: true, Data: true, Views: true, Objects: true}
	dump := func(r Ref, o DumpOptions) string {
		var buf bytes.Buffer
		if err := c.Dump(ctx, r, o, &buf); err != nil {
			t.Fatalf("dump %s: %v", name(r), err)
		}
		return buf.String()
	}
	first := dump(src, opts)
	for _, want := range []string{"parent", "child", "child_touch", "add_one", "parent_names"} {
		if !strings.Contains(first, want) {
			t.Errorf("dump lacks %q", want)
		}
	}
	if strings.Contains(first, "DEFINER=") {
		t.Error("DEFINER=user@host must be stripped from the dump")
	}

	sum, err := c.ExecScript(ctx, dst, first, false)
	if err != nil || sum.Failed != 0 || sum.Stopped {
		t.Fatalf("restore: %+v %v\n%s", sum, err, first)
	}
	second := dump(dst, opts)
	if a, b := normalizeDump(first), normalizeDump(second); a != b {
		t.Errorf("restored dump differs from the source dump\n--- source\n%s\n--- restored\n%s", a, b)
	}

	// The source must be untouched (a schema-qualified function must not have
	// been recreated in the source schema by the restore).
	if again := dump(src, opts); normalizeDump(again) != normalizeDump(first) {
		t.Errorf("restoring changed the source\n--- before\n%s\n--- after\n%s", first, again)
	}

	value := func(q string) string {
		res, err := c.Run(ctx, dst, q)
		if err != nil || len(res.Rows) != 1 {
			t.Fatalf("%s: %+v %v", q, res, err)
		}
		return res.Rows[0][0].V
	}
	if got := value(nameQuery); got != `it's a "test" \ back` {
		t.Errorf("restored name = %q", got)
	}
	if got := value(blobQuery); got != "deadbeef" {
		t.Errorf("restored blob = %q", got)
	}
	if got := value("SELECT COUNT(*) FROM child"); got != "3" {
		t.Errorf("restored child rows = %s", got)
	}
	if got := value("SELECT note FROM child WHERE id = (SELECT MIN(id) FROM child)"); got != "one" {
		t.Errorf("first child note = %q (trigger must not have run during restore)", got)
	}

	// Sequences / AUTO_INCREMENT continue after the restored rows, the restored
	// trigger and function work, and the foreign key is enforced.
	if _, err := c.Run(ctx, dst, "INSERT INTO parent (name) VALUES ('next')"); err != nil {
		t.Fatalf("insert after restore: %v", err)
	}
	if got := value("SELECT MAX(id) FROM parent"); got != "3" {
		t.Errorf("next parent id = %s, want 3", got)
	}
	if _, err := c.Run(ctx, dst, "INSERT INTO child (parent_id, note) VALUES (3, 'x')"); err != nil {
		t.Fatalf("insert child: %v", err)
	}
	if got := value("SELECT note FROM child WHERE id = (SELECT MAX(id) FROM child)"); got != "x!" {
		t.Errorf("trigger result = %q, want x!", got)
	}
	if got := value("SELECT add_one(41)"); got != "42" {
		t.Errorf("add_one(41) = %q", got)
	}
	if _, err := c.Run(ctx, dst, "INSERT INTO child (parent_id, note) VALUES (999, 'orphan')"); err == nil {
		t.Error("foreign key was not restored")
	}

	// DROP IF EXISTS + restore over an existing copy works (idempotent).
	withDrop := dump(src, DumpOptions{Structure: true, Data: true, Drop: true, Views: true, Objects: true})
	if sum, err := c.ExecScript(ctx, dst, withDrop, false); err != nil || sum.Failed != 0 {
		t.Fatalf("restore over existing: %+v %v", sum, err)
	}
	if a, b := normalizeDump(first), normalizeDump(dump(dst, opts)); a != b {
		t.Errorf("after DROP+restore the copy differs\n--- source\n%s\n--- copy\n%s", a, b)
	}

	// Selecting one table dumps only that table (and its data).
	only := dump(src, DumpOptions{Structure: true, Data: true, Tables: []string{"child"}})
	if strings.Contains(only, "parent_names") {
		t.Errorf("single-table dump included a view:\n%s", only)
	}
	for _, line := range strings.Split(only, "\n") {
		if strings.HasPrefix(line, "INSERT INTO `parent`") || strings.HasPrefix(line, `INSERT INTO "parent"`) {
			t.Errorf("single-table dump included data of another table: %s", line)
		}
	}
}

// checkObjectLists covers the tree/objects-panel lists against a real server.
func checkObjectLists(t *testing.T, c *Conn, r Ref) {
	t.Helper()
	ctx := context.Background()
	pg := c.Config.Kind == Postgres

	idx, err := c.IndexList(ctx, r)
	if err != nil {
		t.Fatalf("IndexList: %v", err)
	}
	wantIdx := "idx_parent"
	if pg {
		wantIdx = "child_note_idx"
	}
	var found *IndexInfo
	var primary, unique bool
	for i, x := range idx {
		if x.Name == wantIdx {
			found = &idx[i]
		}
		primary = primary || (x.Primary && x.Table == "parent")
		unique = unique || (x.Unique && !x.Primary && x.Table == "parent")
	}
	if found == nil || found.Table != "child" || found.Unique || found.Primary || found.ID == "" || !primary || !unique {
		t.Errorf("IndexList = %+v", idx)
	}
	if found != nil {
		ddl, err := c.ObjectDDL(ctx, r, KindIndex, found.ID)
		if err != nil || !strings.Contains(strings.ToUpper(ddl), "INDEX") || !strings.Contains(ddl, wantIdx) {
			t.Errorf("index DDL = %q, %v", ddl, err)
		}
	}

	trg, err := c.TriggerList(ctx, r)
	if err != nil || len(trg) != 1 || trg[0].Name != "child_touch" || trg[0].Table != "child" || trg[0].Event != "BEFORE INSERT" {
		t.Fatalf("TriggerList = %+v, %v", trg, err)
	}
	if ddl, err := c.ObjectDDL(ctx, r, KindTrigger, trg[0].ID); err != nil || !strings.Contains(strings.ToUpper(ddl), "CREATE") || !strings.Contains(ddl, "child_touch") {
		t.Errorf("trigger DDL = %q, %v", ddl, err)
	}

	routines, err := c.RoutineList(ctx, r)
	if err != nil {
		t.Fatalf("RoutineList: %v", err)
	}
	byName := map[string]RoutineInfo{}
	for _, x := range routines {
		byName[x.Name] = x
	}
	if byName["add_one"].Kind != KindFunction || !strings.Contains(strings.ToLower(byName["add_one"].Returns), "int") || !strings.Contains(strings.ToLower(byName["add_one"].Args), "int") {
		t.Errorf("add_one = %+v (all: %+v)", byName["add_one"], routines)
	}
	if !pg && byName["bump"].Kind != KindProcedure {
		t.Errorf("bump = %+v", byName["bump"])
	}
	for _, n := range []string{"add_one", "touch_note"} {
		if x, ok := byName[n]; ok {
			if ddl, err := c.ObjectDDL(ctx, r, x.Kind, x.ID); err != nil || !strings.Contains(strings.ToUpper(ddl), "FUNCTION") || !strings.Contains(ddl, n) {
				t.Errorf("%s DDL = %q, %v", n, ddl, err)
			}
		}
	}
}
