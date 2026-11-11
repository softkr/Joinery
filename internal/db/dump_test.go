package db

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var dumpHeaderRe = regexp.MustCompile(`(?m)^-- Joinery dump .*$`)

func dumpString(t *testing.T, c *Conn, o DumpOptions) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Dump(context.Background(), Ref{}, o, &buf); err != nil {
		t.Fatalf("dump: %v", err)
	}
	return buf.String()
}

func normalizeDump(s string) string { return dumpHeaderRe.ReplaceAllString(s, "-- header") }

func newSQLite(t *testing.T) *Conn {
	t.Helper()
	c, err := Open(context.Background(), Config{Kind: SQLite, Database: filepath.Join(t.TempDir(), "x.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestDumpRestoreSQLiteRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := newSQLite(t)
	if _, _, err := src.RunScript(ctx, Ref{}, `
		CREATE TABLE authors(id INTEGER PRIMARY KEY, name TEXT NOT NULL, bio TEXT, avatar BLOB, score REAL, born DATE);
		CREATE TABLE books(id INTEGER PRIMARY KEY, author_id INTEGER NOT NULL REFERENCES authors(id) ON DELETE CASCADE, title TEXT UNIQUE, price NUMERIC);
		CREATE TABLE log(msg TEXT, at TEXT DEFAULT (datetime('now')));
		CREATE INDEX idx_books_author ON books(author_id, price);
		CREATE VIEW v_books AS SELECT b.title, a.name FROM books b JOIN authors a ON a.id = b.author_id;
		CREATE TRIGGER trg_books AFTER INSERT ON books BEGIN INSERT INTO log(msg) VALUES ('new ' || NEW.title); UPDATE log SET msg = msg || '!' WHERE rowid = last_insert_rowid(); END;
		INSERT INTO authors VALUES (1, 'O''Brien; "quoted"', NULL, X'00FF10', 3.14159265358979, '1999-12-31'), (2, '한글 😀', 'line1
line2', NULL, -0.5, NULL), (3, 'back\slash', '', zeroblob(0), 1e300, '2000-01-01');
		INSERT INTO books VALUES (1, 1, 'A;B', 9.99), (2, 1, NULL, 10), (3, 2, 'Third', 1234567890123.5);
	`); err != nil {
		t.Fatal(err)
	}

	full := dumpString(t, src, DumpOptions{Structure: true, Data: true, Drop: true, Views: true, Objects: true})
	for _, want := range []string{"PRAGMA foreign_keys=OFF;", "BEGIN;", "CREATE TABLE authors", "X'00FF10'", "'O''Brien; \"quoted\"'", "CREATE INDEX idx_books_author", "CREATE VIEW v_books", "CREATE TRIGGER trg_books", "COMMIT;", "DROP TABLE IF EXISTS \"books\""} {
		if !strings.Contains(full, want) {
			t.Errorf("dump misses %q", want)
		}
	}

	// restore into an empty database and dump again: identical
	dst := newSQLite(t)
	sum, err := dst.ExecScript(ctx, Ref{}, full, false)
	if err != nil || sum.Failed != 0 || sum.Statements != sum.Total {
		t.Fatalf("restore: %+v, %v", sum, err)
	}
	again := dumpString(t, dst, DumpOptions{Structure: true, Data: true, Drop: true, Views: true, Objects: true})
	if normalizeDump(full) != normalizeDump(again) {
		t.Fatalf("round trip differs:\n--- original\n%s\n--- restored\n%s", full, again)
	}
	// the restored trigger still works and data is intact
	if _, _, err := dst.RunScript(ctx, Ref{}, "INSERT INTO books VALUES (9, 3, 'Fresh', 1)"); err != nil {
		t.Fatal(err)
	}
	if res, _, _ := dst.RunScript(ctx, Ref{}, "SELECT msg FROM log WHERE msg LIKE 'new Fresh%'"); len(res) != 1 || len(res[0].Result.Rows) != 1 {
		t.Errorf("trigger did not fire after restore: %+v", res)
	}
	// foreign keys are back on after the restore script
	if _, err := dst.Run(ctx, Ref{}, "INSERT INTO books VALUES (10, 99, 'orphan', 1)"); err == nil {
		t.Error("foreign key enforcement was left off")
	}

	// structure only / data only / table subset
	structOnly := dumpString(t, src, DumpOptions{Structure: true})
	if strings.Contains(structOnly, "INSERT INTO") || !strings.Contains(structOnly, "CREATE TABLE books") {
		t.Errorf("structure-only dump:\n%s", structOnly)
	}
	dataOnly := dumpString(t, src, DumpOptions{Data: true, Tables: []string{"authors"}})
	if strings.Contains(dataOnly, "CREATE ") || !strings.Contains(dataOnly, "INSERT INTO \"authors\"") || strings.Contains(dataOnly, "\"books\"") {
		t.Errorf("data-only subset dump:\n%s", dataOnly)
	}
	var sink bytes.Buffer
	if err := src.Dump(ctx, Ref{}, DumpOptions{Structure: true}, &sink); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]DumpOptions{
		"nothing selected": {},
		"unknown table":    {Structure: true, Tables: []string{"nope"}},
		"a view as table":  {Structure: true, Tables: []string{"v_books"}},
	} {
		if err := src.Dump(ctx, Ref{}, o, &sink); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDumpBatchesRows(t *testing.T) {
	ctx := context.Background()
	c := newSQLite(t)
	if _, _, err := c.RunScript(ctx, Ref{}, "CREATE TABLE n(i INTEGER PRIMARY KEY, s TEXT)"); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO n VALUES ")
	for i := 1; i <= 250; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		sb.WriteString("(" + strconv.Itoa(i) + ", 'row " + strconv.Itoa(i) + "')")
	}
	if _, _, err := c.RunScript(ctx, Ref{}, sb.String()); err != nil {
		t.Fatal(err)
	}
	out := dumpString(t, c, DumpOptions{Structure: true, Data: true})
	if n := strings.Count(out, "INSERT INTO"); n != 3 {
		t.Errorf("250 rows in batches of %d => 3 statements, got %d", dumpBatch, n)
	}
	dst := newSQLite(t)
	if _, err := dst.ExecScript(ctx, Ref{}, out, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := dst.Count(ctx, Ref{Table: "n"}, ""); n != 250 {
		t.Errorf("restored %d rows", n)
	}
}

func TestExecScriptErrorHandling(t *testing.T) {
	ctx := context.Background()
	c := newSQLite(t)
	script := "CREATE TABLE t(a INTEGER PRIMARY KEY);\nINSERT INTO t VALUES (1);\nINSERT INTO t VALUES (1);\nINSERT INTO t VALUES (2);\nSELEC 1;\nINSERT INTO t VALUES (3);"

	sum, err := c.ExecScript(ctx, Ref{}, script, false)
	if err != nil || !sum.Stopped || sum.Failed != 1 || sum.Statements != 3 || sum.Total != 6 || len(sum.Errors) != 1 || !strings.Contains(sum.Errors[0], "#3") {
		t.Fatalf("stop on error: %+v %v", sum, err)
	}
	if n, _ := c.Count(ctx, Ref{Table: "t"}, ""); n != 1 {
		t.Errorf("rows after stop = %d, want 1", n)
	}
	sum, err = c.ExecScript(ctx, Ref{}, "INSERT INTO t VALUES (1);\nINSERT INTO t VALUES (2);\nSELEC 1;\nINSERT INTO t VALUES (3);", true)
	if err != nil || sum.Stopped || sum.Failed != 2 || sum.Statements != 4 {
		t.Fatalf("continue on error: %+v %v", sum, err)
	}
	if n, _ := c.Count(ctx, Ref{Table: "t"}, ""); n != 3 {
		t.Errorf("rows after continue = %d, want 3", n)
	}
	if _, err := c.ExecScript(ctx, Ref{}, " -- nothing\n;", false); err == nil {
		t.Error("empty script must fail")
	}
}

func TestStripDefiner(t *testing.T) {
	for in, want := range map[string]string{
		"CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `v` AS select 1": "CREATE ALGORITHM=UNDEFINED SQL SECURITY DEFINER VIEW `v` AS select 1",
		"CREATE DEFINER=`app`@`%` TRIGGER t BEFORE INSERT ON x FOR EACH ROW SET NEW.a=1":                  "CREATE TRIGGER t BEFORE INSERT ON x FOR EACH ROW SET NEW.a=1",
		"CREATE DEFINER=CURRENT_USER PROCEDURE p() SELECT 1":                                              "CREATE PROCEDURE p() SELECT 1",
		"CREATE DEFINER='u'@'h' FUNCTION f() RETURNS int RETURN 1":                                        "CREATE FUNCTION f() RETURNS int RETURN 1",
		"SELECT 'DEFINER=x@y' AS s":                                                                       "SELECT 'DEFINER=x@y' AS s",
	} {
		if got := stripDefiner(in); got != want {
			t.Errorf("stripDefiner(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}
