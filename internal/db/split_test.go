package db

import (
	"context"
	"reflect"
	"testing"
)

func TestSplitStatements(t *testing.T) {
	type tc struct {
		name string
		kind Kind
		in   string
		want []string
	}
	for _, c := range []tc{
		{"basic", SQLite, "SELECT 1; SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"no trailing semicolon", SQLite, "SELECT 1;\nSELECT 2", []string{"SELECT 1", "SELECT 2"}},
		{"empty and whitespace", SQLite, " ;; \n ; ", nil},
		{"semicolon in string", SQLite, "INSERT INTO t VALUES ('a;b'); SELECT 1", []string{"INSERT INTO t VALUES ('a;b')", "SELECT 1"}},
		{"escaped quote", SQLite, "SELECT 'it''s; ok'; SELECT 2", []string{"SELECT 'it''s; ok'", "SELECT 2"}},
		{"quoted identifier", Postgres, `SELECT "a;b" FROM t; SELECT 2`, []string{`SELECT "a;b" FROM t`, "SELECT 2"}},
		{"backtick identifier", MySQL, "SELECT `a;b` FROM t; SELECT 2", []string{"SELECT `a;b` FROM t", "SELECT 2"}},
		{"line comment", SQLite, "SELECT 1 -- a;b\n; SELECT 2", []string{"SELECT 1 -- a;b", "SELECT 2"}},
		{"block comment", SQLite, "SELECT /* ; */ 1; SELECT 2", []string{"SELECT /* ; */ 1", "SELECT 2"}},
		{"comment-only chunks dropped", SQLite, "-- just a note\n; /* x */ ; SELECT 1", []string{"SELECT 1"}},
		{"leading comment stripped", SQLite, "-- first\nSELECT 1; /* two */ SELECT 2", []string{"SELECT 1", "SELECT 2"}},

		// MySQL specifics
		{"mysql backslash escape", MySQL, `SELECT 'a\'; b'; SELECT 2`, []string{`SELECT 'a\'; b'`, "SELECT 2"}},
		{"mysql double-quoted string", MySQL, `SELECT "x\";y"; SELECT 2`, []string{`SELECT "x\";y"`, "SELECT 2"}},
		{"mysql hash comment", MySQL, "SELECT 1 # a;b\n; SELECT 2", []string{"SELECT 1 # a;b", "SELECT 2"}},
		{"mysql -- needs a space", MySQL, "SELECT 1--1; SELECT 2", []string{"SELECT 1--1", "SELECT 2"}},
		{"mysql -- with space", MySQL, "SELECT 1 -- x;y\n; SELECT 2", []string{"SELECT 1 -- x;y", "SELECT 2"}},
		{"mysql executable comment kept", MySQL, "/*!40101 SET NAMES utf8 */; SELECT 1", []string{"/*!40101 SET NAMES utf8 */", "SELECT 1"}},
		{"sqlite backslash is literal", SQLite, `SELECT '\'; SELECT 2`, []string{`SELECT '\'`, "SELECT 2"}},
		{"hash is not a comment elsewhere", Postgres, "SELECT '#' ; SELECT 2", []string{"SELECT '#'", "SELECT 2"}},

		// PostgreSQL specifics
		{"pg dollar quote", Postgres, "CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql; SELECT f()",
			[]string{"CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql", "SELECT f()"}},
		{"pg tagged dollar quote", Postgres, "SELECT $fn$ a; $$ b; $fn$; SELECT 2", []string{"SELECT $fn$ a; $$ b; $fn$", "SELECT 2"}},
		{"pg positional parameter is not a quote", Postgres, "SELECT $1; SELECT $2", []string{"SELECT $1", "SELECT $2"}},
		{"pg identifier with dollar", Postgres, "SELECT a$b$ FROM t; SELECT 2", []string{"SELECT a$b$ FROM t", "SELECT 2"}},
		{"pg nested block comment", Postgres, "SELECT /* a /* ; */ ; */ 1; SELECT 2", []string{"SELECT /* a /* ; */ ; */ 1", "SELECT 2"}},
		{"mysql block comment does not nest", MySQL, "SELECT /* a /* ; */ 1; SELECT 2", []string{"SELECT /* a /* ; */ 1", "SELECT 2"}},
		{"pg E string backslash", Postgres, `SELECT E'a\'; b'; SELECT 2`, []string{`SELECT E'a\'; b'`, "SELECT 2"}},
		{"pg plain string backslash is literal", Postgres, `SELECT 'a\'; SELECT 2`, []string{`SELECT 'a\'`, "SELECT 2"}},

		// SQLite trigger body
		{"sqlite trigger", SQLite,
			"CREATE TRIGGER trg AFTER INSERT ON t BEGIN UPDATE c SET n = n + 1; INSERT INTO log VALUES (1); END; SELECT 1",
			[]string{"CREATE TRIGGER trg AFTER INSERT ON t BEGIN UPDATE c SET n = n + 1; INSERT INTO log VALUES (1); END", "SELECT 1"}},
		{"sqlite temp trigger with CASE", SQLite,
			"CREATE TEMP TRIGGER x AFTER INSERT ON t BEGIN UPDATE c SET n = CASE WHEN 1 THEN 2 ELSE 3 END; END; SELECT 2",
			[]string{"CREATE TEMP TRIGGER x AFTER INSERT ON t BEGIN UPDATE c SET n = CASE WHEN 1 THEN 2 ELSE 3 END; END", "SELECT 2"}},
		{"BEGIN outside a trigger splits normally", SQLite, "BEGIN; INSERT INTO t VALUES (1); COMMIT", []string{"BEGIN", "INSERT INTO t VALUES (1)", "COMMIT"}},

		// MySQL DELIMITER command (mysqldump style)
		{"mysql delimiter block", MySQL,
			"CREATE TABLE t (a int);\nDELIMITER ;;\nCREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW BEGIN SET NEW.a = 1; SET @x = 2; END;;\nDELIMITER ;\nSELECT 1;",
			[]string{"CREATE TABLE t (a int)", "CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW BEGIN SET NEW.a = 1; SET @x = 2; END", "SELECT 1"}},
		{"mysql delimiter //", MySQL, "delimiter //\nCREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END//\nDELIMITER ;\nSELECT 3", []string{"CREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END", "SELECT 3"}},
		{"delimiter after a comment", MySQL, "-- header\nDELIMITER $$\nSELECT 1$$\nSELECT '$$' $$", []string{"SELECT 1", "SELECT '$$'"}},
		{"delimiter is only a command at statement start", MySQL, "SELECT 'x' AS delimiter; SELECT 2", []string{"SELECT 'x' AS delimiter", "SELECT 2"}},
		{"delimiter is MySQL only", Postgres, "SELECT 1; DELIMITER ;; SELECT 2", []string{"SELECT 1", "DELIMITER", "SELECT 2"}},
		{"unterminated string runs to the end", SQLite, "SELECT 'oops; SELECT 2", []string{"SELECT 'oops; SELECT 2"}},
		{"unterminated dollar quote", Postgres, "SELECT $$ a; b", []string{"SELECT $$ a; b"}},
		{"non-ascii text", MySQL, "SELECT '한글;🙂'; SELECT '다음'", []string{"SELECT '한글;🙂'", "SELECT '다음'"}},
	} {
		got := SplitStatements(c.kind, c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n  in:   %q\n  got:  %q\n  want: %q", c.name, c.in, got, c.want)
		}
	}
}

func TestRunScriptSQLite(t *testing.T) {
	ctx := testCtx()
	c := openTestDB(t)
	r := Ref{}

	res, skipped, err := c.RunScript(ctx, r, `
		-- seed
		INSERT INTO users(name, age) VALUES ('x;y', 1);
		UPDATE users SET age = age + 10 WHERE name = 'x;y';
		SELECT name, age FROM users WHERE name = 'x;y';
		SELECT count(*) FROM users`)
	if err != nil || skipped != 0 || len(res) != 4 {
		t.Fatalf("results = %+v skipped=%d err=%v", res, skipped, err)
	}
	if res[0].Result.IsQuery || res[0].Result.Affected != 1 || res[1].Result.Affected != 1 {
		t.Errorf("exec results = %+v %+v", res[0].Result, res[1].Result)
	}
	if !res[2].Result.IsQuery || res[2].Result.Rows[0][1].V != "11" || res[2].Result.Rows[0][0].V != "x;y" {
		t.Errorf("select result = %+v", res[2].Result)
	}

	// stops at the first error, reports how many were skipped, earlier effects stay
	res, skipped, err = c.RunScript(ctx, r, "INSERT INTO users(name) VALUES ('kept'); SELEC 1; INSERT INTO users(name) VALUES ('never'); SELECT 1")
	if err != nil || len(res) != 2 || res[1].Err == nil || res[1].Result != nil || skipped != 2 {
		t.Fatalf("error run = %+v skipped=%d err=%v", res, skipped, err)
	}
	if n, _ := c.Count(ctx, Ref{Table: "users"}, "name IN ('kept','never')"); n != 1 {
		t.Errorf("rows after failed script = %d, want 1 (only 'kept')", n)
	}

	// one session: an explicit transaction spans statements and can be rolled back
	if _, _, err := c.RunScript(ctx, r, "BEGIN; INSERT INTO users(name) VALUES ('tx'); ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.Count(ctx, Ref{Table: "users"}, "name = 'tx'"); n != 0 {
		t.Errorf("rolled back row visible: %d", n)
	}

	if _, _, err := c.RunScript(ctx, r, " -- nothing\n ; "); err == nil {
		t.Error("comment-only script must fail")
	}
	big := ""
	for i := 0; i <= MaxScriptStatements; i++ {
		big += "SELECT 1;"
	}
	if _, _, err := c.RunScript(ctx, r, big); err == nil {
		t.Error("too many statements accepted")
	}
}

// A transaction left open by a script must not leak to the next request.
func TestRunScriptDoesNotLeakOpenTransaction(t *testing.T) {
	ctx := testCtx()
	c := openTestDB(t)
	if _, _, err := c.RunScript(ctx, Ref{}, "BEGIN; INSERT INTO users(name) VALUES ('dangling')"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // would see (or block on) the open transaction if the connection were reused
		if n, err := c.Count(ctx, Ref{Table: "users"}, "name = 'dangling'"); err != nil || n != 0 {
			t.Fatalf("leaked transaction: count=%d err=%v", n, err)
		}
	}
}

func testCtx() context.Context { return context.Background() }
