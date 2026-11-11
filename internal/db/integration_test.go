package db

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Integration tests run only when a server is provided, e.g.
//
//	DBCAT_IT_PG=postgres:pw@127.0.0.1:15432 \
//	DBCAT_IT_MYSQL=root:pw@127.0.0.1:13306 \
//	DBCAT_IT_MARIADB=root:pw@127.0.0.1:13307 go test ./internal/db -run Integration -v
//
// MySQL/MariaDB servers need a database named "shop".
func itConfig(t *testing.T, env string, kind Kind) Config {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		t.Skipf("%s not set", env)
	}
	u, err := url.Parse("x://" + v)
	if err != nil {
		t.Fatalf("%s: %v", env, err)
	}
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)
	pw, _ := u.User.Password()
	return Config{Kind: kind, Host: host, Port: port, User: u.User.Username(), Password: pw}
}

func TestIntegrationPostgres(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_PG", Postgres)
	cfg.Database = "postgres"
	runIntegration(t, cfg, Ref{Database: "postgres", Schema: "public", Table: "it_items"}, "serial")
}

func TestIntegrationMySQL(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_MYSQL", MySQL)
	cfg.Database = "shop"
	runIntegration(t, cfg, Ref{Database: "shop", Table: "it_items"}, "int AUTO_INCREMENT")
}

func TestIntegrationMariaDB(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_MARIADB", MySQL)
	cfg.Database = "shop"
	runIntegration(t, cfg, Ref{Database: "shop", Table: "it_items"}, "int AUTO_INCREMENT")
}

func openWithRetry(t *testing.T, cfg Config) *Conn {
	t.Helper()
	var err error
	for i := 0; i < 60; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var c *Conn
		c, err = Open(ctx, cfg)
		cancel()
		if err == nil {
			t.Cleanup(c.Close)
			return c
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("server not reachable: %v", err)
	return nil
}

func runIntegration(t *testing.T, cfg Config, r Ref, idType string) {
	ctx := context.Background()
	c := openWithRetry(t, cfg)
	pg := cfg.Kind == Postgres

	dbs, err := c.Databases(ctx)
	if err != nil || !slices.Contains(dbs, r.Database) {
		t.Fatalf("databases = %v, %v", dbs, err)
	}
	if pg {
		schemas, err := c.Schemas(ctx, r.Database)
		if err != nil || !slices.Contains(schemas, "public") {
			t.Fatalf("schemas = %v, %v", schemas, err)
		}
	}

	drop := "DROP TABLE IF EXISTS " + c.Qualified(r)
	if _, err := c.Run(ctx, r, drop); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.Run(ctx, r, drop) })

	// ---- create ----
	stmts, err := c.PlanCreate(r, []FieldSpec{
		{Name: "id", Type: idType, NotNull: true, PK: true},
		{Name: "name", Type: "varchar(50)", NotNull: true},
		{Name: "qty", Type: "int", Default: "0"},
	}, []IndexSpec{{Name: "idx_it_name", Columns: []string{"name"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.ExecDDL(ctx, r, stmts); err != nil || n != len(stmts) {
		t.Fatalf("create: %d, %v\n%v", n, err, stmts)
	}

	// ---- metadata ----
	tabs, err := c.Tables(ctx, r)
	if err != nil || !slices.ContainsFunc(tabs, func(x Table) bool { return x.Name == "it_items" && !x.IsView }) {
		t.Fatalf("tables = %+v, %v", tabs, err)
	}
	cols, err := c.Columns(ctx, r)
	if err != nil || len(cols) != 3 || !cols[0].PK || cols[1].Nullable || cols[2].Default != "0" || cols[1].Default != "" {
		t.Fatalf("columns = %+v, %v", cols, err)
	}
	idx, err := c.Indexes(ctx, r)
	if err != nil || !slices.ContainsFunc(idx, func(x Index) bool { return x.Name == "idx_it_name" && x.Columns == "name" }) ||
		!slices.ContainsFunc(idx, func(x Index) bool { return x.Primary }) {
		t.Fatalf("indexes = %+v, %v", idx, err)
	}

	// ---- insert (string params bound to int columns) ----
	if err := c.InsertRow(ctx, r, []InsertVal{{Col: "name", Cell: Cell{V: "x"}}}); err != nil {
		t.Fatalf("insert default: %v", err)
	}
	if err := c.InsertRow(ctx, r, []InsertVal{{Col: "name", Cell: Cell{V: "y"}}, {Col: "qty", Cell: Cell{V: "5"}}}); err != nil {
		t.Fatalf("insert explicit: %v", err)
	}
	if err := c.InsertRow(ctx, r, []InsertVal{{Col: "name", Cell: Cell{Null: true}}}); err == nil {
		t.Error("NULL into NOT NULL must fail")
	}
	// Non-ASCII (incl. 4-byte) must survive a round trip: connection charset check.
	const uni = "한글 😀 \"q\" 'q'"
	if err := c.InsertRow(ctx, r, []InsertVal{{Col: "name", Cell: Cell{V: uni}}}); err != nil {
		t.Fatalf("insert unicode: %v", err)
	}
	up, err := c.TablePage(ctx, r, PageOpts{Where: "qty = 0 AND id > 2", Limit: 5})
	if err != nil || len(up.Rows) != 1 || up.Rows[0][1].V != uni {
		t.Fatalf("unicode round trip = %+v, %v", up, err)
	}
	// (sequences are not rolled back by the failed insert above, so use the real id)
	if err := c.DeleteRow(ctx, r, map[string]string{"id": up.Rows[0][0].V}); err != nil {
		t.Fatalf("delete unicode row: %v", err)
	}

	// ---- read / sort / filter / count ----
	p, err := c.TablePage(ctx, r, PageOpts{Sort: "id", Desc: true, Limit: 1})
	if err != nil || len(p.Rows) != 1 || !p.More || p.Rows[0][1].V != "y" || p.Rows[0][2].V != "5" {
		t.Fatalf("page = %+v, %v", p, err)
	}
	if n, err := c.Count(ctx, r, "qty >= 5"); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}

	// ---- update / delete by primary key (PK passed as string) ----
	pk := map[string]string{"id": p.Rows[0][0].V}
	if err := c.UpdateCell(ctx, r, pk, "name", Cell{V: "z"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := c.UpdateCell(ctx, r, pk, "qty", Cell{Null: true}); err != nil {
		t.Fatalf("update null: %v", err)
	}
	if err := c.UpdateCell(ctx, r, pk, "qty", Cell{V: "7"}); err != nil {
		t.Fatalf("update int: %v", err)
	}
	if err := c.UpdateCell(ctx, r, map[string]string{"id": "999999"}, "name", Cell{V: "q"}); err == nil {
		t.Error("update of missing row must fail")
	}

	// ---- SQL editor with database/schema context ----
	res, err := c.Run(ctx, r, "SELECT name, qty FROM it_items ORDER BY id")
	if err != nil || !res.IsQuery || len(res.Rows) != 2 || res.Rows[1][0].V != "z" {
		t.Fatalf("run select = %+v, %v", res, err)
	}
	res, err = c.Run(ctx, r, "UPDATE it_items SET qty = qty + 1")
	if err != nil || res.IsQuery || res.Affected != 2 {
		t.Fatalf("run update = %+v, %v", res, err)
	}

	// ---- export ----
	rows := 0
	if err := c.ExportTable(ctx, r, PageOpts{Sort: "id"}, func(_ []string, row []Cell) error {
		if row != nil {
			rows++
		}
		return nil
	}); err != nil || rows != 2 {
		t.Fatalf("export rows = %d, %v", rows, err)
	}

	// ---- designer: add + rename + drop index (all engines) ----
	alter := []FieldSpec{
		{Orig: "id", Name: "id", Type: cols[0].Type, NotNull: true, PK: true, Default: cols[0].Default},
		{Orig: "name", Name: "title", Type: cols[1].Type, NotNull: true},
		{Orig: "qty", Name: "qty", Type: cols[2].Type, NotNull: !cols[2].Nullable, Default: cols[2].Default},
		{Name: "note", Type: "text"},
	}
	stmts, err = c.PlanAlter(ctx, r, alter, []IndexSpec{{Orig: "idx_it_name", Drop: true}}, nil)
	if err != nil {
		t.Fatalf("plan alter: %v", err)
	}
	if n, err := c.ExecDDL(ctx, r, stmts); err != nil || n != len(stmts) {
		t.Fatalf("alter: %d, %v\n%v", n, err, stmts)
	}
	cols, _ = c.Columns(ctx, r)
	names := []string{}
	for _, col := range cols {
		names = append(names, col.Name)
	}
	if !slices.Equal(names, []string{"id", "title", "qty", "note"}) {
		t.Fatalf("columns after alter = %v", names)
	}
	if idx, _ = c.Indexes(ctx, r); slices.ContainsFunc(idx, func(x Index) bool { return x.Name == "idx_it_name" }) {
		t.Error("index not dropped")
	}

	// ---- attribute changes: PostgreSQL applies them, others refuse ----
	alter[2] = FieldSpec{Orig: "qty", Name: "qty", Type: "bigint", NotNull: true, Default: "9"}
	alter[1].Orig = "title"
	alter[3].Orig = "note"
	stmts, err = c.PlanAlter(ctx, r, alter, nil, nil)
	if !pg {
		if err == nil {
			t.Fatalf("attribute change must be refused on %s, got %v", cfg.Kind, stmts)
		}
		return
	}
	if err != nil || len(stmts) != 3 {
		t.Fatalf("pg modify plan = %v, %v", stmts, err)
	}
	if _, err := c.ExecDDL(ctx, r, stmts); err != nil {
		t.Fatalf("pg modify: %v\n%v", err, stmts)
	}
	cols, _ = c.Columns(ctx, r)
	if cols[2].Type != "bigint" || cols[2].Nullable || cols[2].Default != "9" {
		t.Fatalf("pg qty after modify = %+v", cols[2])
	}
	// a failing statement rolls the whole transaction back
	bad := []string{"ALTER TABLE " + c.Qualified(r) + " ADD COLUMN ok_col int", "ALTER TABLE " + c.Qualified(r) + " ADD COLUMN id int"}
	if _, err := c.ExecDDL(ctx, r, bad); err == nil {
		t.Fatal("expected failure")
	}
	cols, _ = c.Columns(ctx, r)
	if slices.ContainsFunc(cols, func(x Column) bool { return x.Name == "ok_col" }) {
		t.Error("pg DDL transaction was not rolled back")
	}
}

// ---- TLS ----
//
//	DBCAT_IT_PG_TLS=postgres:pw@127.0.0.1:15433 DBCAT_IT_PG_CA=/path/server.crt   (server with ssl=on)
//	DBCAT_IT_MYSQL=root:pw@127.0.0.1:13306
func usesTLS(t *testing.T, c *Conn) bool {
	t.Helper()
	ctx := context.Background()
	if c.Config.Kind == Postgres {
		res, err := c.Run(ctx, Ref{Database: "postgres"}, "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()")
		if err != nil || len(res.Rows) != 1 {
			t.Fatalf("pg_stat_ssl = %+v, %v", res, err)
		}
		return res.Rows[0][0].V == "true"
	}
	res, err := c.Run(ctx, Ref{Database: "shop"}, "SHOW SESSION STATUS LIKE 'Ssl_cipher'")
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("Ssl_cipher = %+v, %v", res, err)
	}
	return res.Rows[0][1].V != ""
}

func TestIntegrationTLSPostgres(t *testing.T) {
	base := itConfig(t, "DBCAT_IT_PG_TLS", Postgres)
	base.Database = "postgres"
	ca := os.Getenv("DBCAT_IT_PG_CA")
	if ca == "" {
		t.Skip("DBCAT_IT_PG_CA not set")
	}
	open := func(mode, caFile string) (*Conn, error) {
		cfg := base
		cfg.SSLMode, cfg.SSLCA = mode, caFile
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return Open(ctx, cfg)
	}
	for _, tc := range []struct {
		mode, ca string
		wantTLS  bool
	}{
		{SSLDisable, "", false}, {"", "", true}, {SSLPrefer, "", true}, {SSLRequire, "", true}, {SSLVerify, ca, true},
	} {
		c, err := open(tc.mode, tc.ca)
		if err != nil {
			t.Fatalf("mode %q: %v", tc.mode, err)
		}
		if got := usesTLS(t, c); got != tc.wantTLS {
			t.Errorf("mode %q: tls = %v, want %v", tc.mode, got, tc.wantTLS)
		}
		c.Close()
	}
	// Verification must be strict: a self-signed server is refused without its CA.
	if c, err := open(SSLVerify, ""); err == nil {
		c.Close()
		t.Error("verify without the CA must fail for a self-signed certificate")
	}
	if c, err := open(SSLVerify, "/nonexistent/ca.pem"); err == nil {
		c.Close()
		t.Error("verify with a missing CA file must fail")
	}
}

func TestIntegrationTLSMySQL(t *testing.T) {
	base := itConfig(t, "DBCAT_IT_MYSQL", MySQL)
	base.Database = "shop"
	open := func(mode string) (*Conn, error) {
		cfg := base
		cfg.SSLMode = mode
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return Open(ctx, cfg)
	}
	for _, tc := range []struct {
		mode    string
		wantTLS bool
	}{{SSLDisable, false}, {"", true}, {SSLPrefer, true}, {SSLRequire, true}} {
		c, err := open(tc.mode)
		if err != nil {
			t.Fatalf("mode %q: %v", tc.mode, err)
		}
		if got := usesTLS(t, c); got != tc.wantTLS {
			t.Errorf("mode %q: tls = %v, want %v", tc.mode, got, tc.wantTLS)
		}
		c.Close()
	}
	// The server uses an auto-generated certificate: strict verification must refuse it.
	if c, err := open(SSLVerify); err == nil {
		c.Close()
		t.Error("verify must fail for an auto-generated certificate")
	}
}

// ---- SSH tunnel ----
//
//	DBCAT_IT_SSH=tun:pw@127.0.0.1:2222                 (sshd; its host key is in $DBCAT_KNOWN_HOSTS)
//	DBCAT_IT_SSH_PG=postgres:pw@dbcat-test-pgplain:5432 (database as seen from the SSH server, not published to the host)
func TestIntegrationSSHTunnelPostgres(t *testing.T) {
	sshU := itConfig(t, "DBCAT_IT_SSH", Postgres)
	target := itConfig(t, "DBCAT_IT_SSH_PG", Postgres)
	target.Database = "postgres"
	target.SSH = &SSHConfig{Host: sshU.Host, Port: sshU.Port, User: sshU.User, Password: sshU.Password}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Without the tunnel the target name does not even resolve from here.
	direct := target
	direct.SSH = nil
	if c, err := Open(ctx, direct); err == nil {
		c.Close()
		t.Fatal("target must not be reachable without the tunnel")
	}

	c, err := Open(ctx, target)
	if err != nil {
		t.Fatalf("open through tunnel: %v", err)
	}
	defer c.Close()
	res, err := c.Run(ctx, Ref{Database: "postgres"}, "SELECT 40 + 2")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0].V != "42" {
		t.Fatalf("query through tunnel = %+v, %v", res, err)
	}
	if dbs, err := c.Databases(ctx); err != nil || len(dbs) == 0 {
		t.Fatalf("databases through tunnel = %v, %v", dbs, err)
	}

	bad := target
	bad.SSH = &SSHConfig{Host: sshU.Host, Port: sshU.Port, User: sshU.User, Password: "wrong"}
	if c, err := Open(ctx, bad); err == nil {
		c.Close()
		t.Error("wrong SSH password accepted")
	}

	// A host that is not in known_hosts is refused (fingerprint in the message).
	empty := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(empty, nil, 0o600)
	t.Setenv("DBCAT_KNOWN_HOSTS", empty)
	if c, err := Open(ctx, target); err == nil {
		c.Close()
		t.Error("unknown SSH host accepted")
	} else if !strings.Contains(err.Error(), "SHA256:") {
		t.Errorf("unknown host error should carry the fingerprint: %v", err)
	}
}

// MySQL goes through a registered dial function instead of pgx's DialFunc.
//
//	DBCAT_IT_SSH_MYSQL=root:pw@dbcat-test-mysql:3306
func TestIntegrationSSHTunnelMySQL(t *testing.T) {
	sshU := itConfig(t, "DBCAT_IT_SSH", Postgres)
	target := itConfig(t, "DBCAT_IT_SSH_MYSQL", MySQL)
	target.Database = "shop"
	target.ID = "it-tunnel"
	target.SSH = &SSHConfig{Host: sshU.Host, Port: sshU.Port, User: sshU.User, Password: sshU.Password}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	direct := target
	direct.SSH = nil
	if c, err := Open(ctx, direct); err == nil {
		c.Close()
		t.Fatal("target must not be reachable without the tunnel")
	}
	c, err := Open(ctx, target)
	if err != nil {
		t.Fatalf("open through tunnel: %v", err)
	}
	defer c.Close()
	if res, err := c.Run(ctx, Ref{Database: "shop"}, "SELECT 40 + 2"); err != nil || res.Rows[0][0].V != "42" {
		t.Fatalf("query through tunnel = %+v, %v", res, err)
	}
	// TLS still works end to end inside the tunnel.
	target.SSLMode = SSLRequire
	c2, err := Open(ctx, target)
	if err != nil {
		t.Fatalf("tls through tunnel: %v", err)
	}
	defer c2.Close()
	if !usesTLS(t, c2) {
		t.Error("expected TLS inside the SSH tunnel")
	}
}

// Public-key login against a real OpenSSH server.
//
//	DBCAT_IT_SSH_KEY=/path/id_ed25519 [DBCAT_IT_SSH_KEY_LOCKED=/path/locked DBCAT_IT_SSH_KEY_PASS=pw]
func TestIntegrationSSHKeyAuth(t *testing.T) {
	sshU := itConfig(t, "DBCAT_IT_SSH", Postgres)
	key := os.Getenv("DBCAT_IT_SSH_KEY")
	if key == "" {
		t.Skip("DBCAT_IT_SSH_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	open := func(file, pass string) error {
		cl, err := dialSSH(ctx, &SSHConfig{Host: sshU.Host, Port: sshU.Port, User: sshU.User, KeyFile: file, Password: pass})
		if err == nil {
			cl.Close()
		}
		return err
	}
	if err := open(key, ""); err != nil {
		t.Fatalf("key login: %v", err)
	}
	if locked := os.Getenv("DBCAT_IT_SSH_KEY_LOCKED"); locked != "" {
		pass := os.Getenv("DBCAT_IT_SSH_KEY_PASS")
		if err := open(locked, ""); err == nil || !strings.Contains(err.Error(), "암호") {
			t.Errorf("locked key without passphrase = %v", err)
		}
		if err := open(locked, pass); err != nil {
			t.Fatalf("locked key login: %v", err)
		}
	}
}

// Scripts: statements run in order on one session; dollar-quoted bodies stay whole.
func TestIntegrationScriptPostgres(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_PG", Postgres)
	cfg.Database = "postgres"
	c := openWithRetry(t, cfg)
	ctx := context.Background()
	r := Ref{Database: "postgres", Schema: "public"}

	res, skipped, err := c.RunScript(ctx, r, `
		-- a function whose body contains semicolons
		CREATE OR REPLACE FUNCTION it_add(a int, b int) RETURNS int AS $body$
		BEGIN
		  RETURN a + b;
		END;
		$body$ LANGUAGE plpgsql;
		SELECT it_add(40, 2) AS answer;
		SELECT E'it\'s; fine' AS esc, $$a;b$$ AS dq, 'x;y' AS plain;
		DROP FUNCTION it_add(int, int)`)
	if err != nil || skipped != 0 || len(res) != 4 || res[3].Err != nil {
		t.Fatalf("script = %+v skipped=%d err=%v", res, skipped, err)
	}
	if got := res[1].Result.Rows[0][0].V; got != "42" {
		t.Errorf("answer = %q", got)
	}
	if row := res[2].Result.Rows[0]; row[0].V != "it's; fine" || row[1].V != "a;b" || row[2].V != "x;y" {
		t.Errorf("quoting row = %+v", row)
	}

	// session state spans statements, an open transaction does not leak afterwards
	res, _, err = c.RunScript(ctx, r, "SET LOCAL x.y = '1'; BEGIN; CREATE TEMP TABLE it_tmp(a int); INSERT INTO it_tmp VALUES (1); SELECT count(*) FROM it_tmp; ROLLBACK")
	if err != nil || res[len(res)-1].Err != nil {
		t.Fatalf("session script: %+v %v", res, err)
	}
	if _, _, err := c.RunScript(ctx, r, "BEGIN; CREATE TABLE it_dangling(a int)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		res, _, err := c.RunScript(ctx, r, "SELECT to_regclass('public.it_dangling') IS NULL AS gone")
		if err != nil || res[0].Err != nil || res[0].Result.Rows[0][0].V != "true" {
			t.Fatalf("dangling transaction leaked into a later request: %+v %v", res, err)
		}
	}

	// INSERT ... RETURNING produces a result set
	if _, _, err := c.RunScript(ctx, r, "CREATE TEMP TABLE it_ret(id serial, n int)"); err != nil {
		t.Fatal(err)
	}
	res, _, err = c.RunScript(ctx, r, "CREATE TEMP TABLE it_ret2(id serial, n int); INSERT INTO it_ret2(n) VALUES (7) RETURNING id, n")
	if err != nil || len(res) != 2 || !res[1].Result.IsQuery || res[1].Result.Rows[0][1].V != "7" {
		t.Fatalf("returning = %+v %v", res, err)
	}
}

func TestIntegrationScriptMySQL(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_MYSQL", MySQL)
	cfg.Database = "shop"
	c := openWithRetry(t, cfg)
	ctx := context.Background()
	r := Ref{Database: "shop"}

	res, skipped, err := c.RunScript(ctx, r, "SET @v = 41; SET @v = @v + 1; SELECT @v AS v; SELECT '#a;b' AS s, `we;ird` FROM (SELECT 1 AS `we;ird`) t # tail; comment\n; SELECT 3 -- x;y\n")
	if err != nil || skipped != 0 || len(res) != 5 || res[4].Err != nil {
		t.Fatalf("script = %+v skipped=%d err=%v", res, skipped, err)
	}
	if res[2].Result.Rows[0][0].V != "42" {
		t.Errorf("session variable did not persist across statements: %+v", res[2].Result.Rows)
	}
	if row := res[3].Result.Rows[0]; row[0].V != "#a;b" || row[1].V != "1" {
		t.Errorf("string row = %+v", row)
	}
	if res[4].Result.Rows[0][0].V != "3" {
		t.Errorf("last statement = %+v", res[4].Result.Rows)
	}
}

// Foreign keys, schema-wide metadata and DDL against a real server.
func runMetaIntegration(t *testing.T, cfg Config, r Ref, idType string) {
	ctx := context.Background()
	c := openWithRetry(t, cfg)
	parent, child, view := r, r, r
	parent.Table, child.Table, view.Table = "it_parent", "it_child", "it_view"
	drop := func() {
		for _, x := range []string{"DROP VIEW IF EXISTS " + c.Qualified(view), "DROP TABLE IF EXISTS " + c.Qualified(child), "DROP TABLE IF EXISTS " + c.Qualified(parent)} {
			_, _ = c.Run(ctx, r, x)
		}
	}
	drop()
	t.Cleanup(drop)

	mk := func(ref Ref, fields []FieldSpec, fks []FKSpec) {
		stmts, err := c.PlanCreate(ref, fields, nil, fks)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.ExecDDL(ctx, ref, stmts); err != nil {
			t.Fatalf("create %s: %v\n%v", ref.Table, err, stmts)
		}
	}
	mk(parent, []FieldSpec{{Name: "id", Type: idType, NotNull: true, PK: true}, {Name: "name", Type: "varchar(20)"}}, nil)
	mk(child, []FieldSpec{
		{Name: "id", Type: idType, NotNull: true, PK: true},
		{Name: "parent_id", Type: "int"},
	}, []FKSpec{{Name: "fk_it_child_parent", Columns: []string{"parent_id"}, RefTable: "it_parent", RefColumns: []string{"id"}, OnDelete: "CASCADE"}})
	if _, err := c.Run(ctx, r, "CREATE VIEW "+c.Qualified(view)+" AS SELECT id, name FROM "+c.Qualified(parent)); err != nil {
		t.Fatal(err)
	}

	// ---- listing ----
	fks, err := c.ForeignKeys(ctx, child)
	if err != nil || len(fks) != 1 {
		t.Fatalf("fks = %+v, %v", fks, err)
	}
	fk := fks[0]
	if fk.Name != "fk_it_child_parent" || fk.Table != "it_child" || fk.RefTable != "it_parent" || fk.OnDelete != "CASCADE" ||
		strings.Join(fk.Columns, ",") != "parent_id" || strings.Join(fk.RefColumns, ",") != "id" {
		t.Fatalf("fk = %+v", fk)
	}
	if all, err := c.SchemaForeignKeys(ctx, r); err != nil || !slices.ContainsFunc(all, func(x ForeignKey) bool { return x.Name == "fk_it_child_parent" }) {
		t.Fatalf("schema-wide fks = %+v, %v", all, err)
	}
	if none, _ := c.ForeignKeys(ctx, parent); len(none) != 0 {
		t.Errorf("parent has no FK: %+v", none)
	}

	// ---- schema-wide columns ----
	tables, truncated, err := c.SchemaColumnInfo(ctx, r, 100000)
	if err != nil || truncated {
		t.Fatalf("column info: %v truncated=%v", err, truncated)
	}
	var tc *TableCols
	for i := range tables {
		if tables[i].Name == "it_child" {
			tc = &tables[i]
		}
	}
	if tc == nil || len(tc.Columns) != 2 || !tc.Columns[0].PK || tc.Columns[1].PK || tc.Columns[0].Type == "" {
		t.Fatalf("it_child columns = %+v", tc)
	}
	if !slices.ContainsFunc(tables, func(x TableCols) bool { return x.Name == "it_view" && x.IsView }) {
		t.Error("the view is missing from the schema listing")
	}

	// ---- DDL ----
	ddl, err := c.DDL(ctx, child)
	if err != nil || !strings.Contains(strings.ToUpper(ddl), "CREATE TABLE") || !strings.Contains(ddl, "it_child") ||
		!strings.Contains(strings.ToUpper(ddl), "FOREIGN KEY") || !strings.Contains(strings.ToUpper(ddl), "REFERENCES") {
		t.Fatalf("table ddl = %q, %v", ddl, err)
	}
	if cfg.Kind == Postgres && (!strings.Contains(ddl, "PRIMARY KEY") || !strings.Contains(ddl, `CONSTRAINT "fk_it_child_parent"`)) {
		t.Errorf("pg ddl misses constraints:\n%s", ddl)
	}
	if vddl, err := c.DDL(ctx, view); err != nil || !strings.Contains(strings.ToUpper(vddl), "CREATE") || !strings.Contains(strings.ToUpper(vddl), "VIEW") {
		t.Errorf("view ddl = %q, %v", vddl, err)
	}
	if _, err := c.DDL(ctx, Ref{Database: r.Database, Schema: r.Schema, Table: "no_such_table"}); err == nil {
		t.Error("ddl of a missing table must fail")
	}

	// ---- constraint is enforced and cascades ----
	if _, err := c.Run(ctx, r, "INSERT INTO "+c.Qualified(parent)+" (id, name) VALUES (1, 'p')"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, r, "INSERT INTO "+c.Qualified(child)+" (id, parent_id) VALUES (1, 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, r, "INSERT INTO "+c.Qualified(child)+" (id, parent_id) VALUES (2, 99)"); err == nil {
		t.Error("orphan insert must violate the FK")
	}

	// ---- designer: add a column + a second FK in one plan, then drop the first FK ----
	cur, _ := c.Columns(ctx, child)
	fields := []FieldSpec{}
	for _, col := range cur {
		fields = append(fields, FieldSpec{Orig: col.Name, Name: col.Name, Type: col.Type, NotNull: !col.Nullable, PK: col.PK, Default: col.Default})
	}
	fields = append(fields, FieldSpec{Name: "alt_id", Type: "int"})
	if _, err := c.PlanAlter(ctx, child, fields, nil, []FKSpec{{Columns: []string{"nope"}, RefTable: "it_parent", RefColumns: []string{"id"}}}); err == nil {
		t.Error("FK on an unknown column must be rejected")
	}
	stmts, err := c.PlanAlter(ctx, child, fields, nil, []FKSpec{{Name: "fk_it_child_alt", Columns: []string{"alt_id"}, RefTable: "it_parent", RefColumns: []string{"id"}, OnDelete: "SET NULL"}})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.ExecDDL(ctx, child, stmts); err != nil || n != len(stmts) {
		t.Fatalf("add fk: %d, %v\n%v", n, err, stmts)
	}
	if fks, _ = c.ForeignKeys(ctx, child); len(fks) != 2 {
		t.Fatalf("after add: %+v", fks)
	}
	// Re-read the live columns, as the designer does, so types match what the server reports.
	cur, _ = c.Columns(ctx, child)
	fields = fields[:0:0]
	for _, col := range cur {
		fields = append(fields, FieldSpec{Orig: col.Name, Name: col.Name, Type: col.Type, NotNull: !col.Nullable, PK: col.PK, Default: col.Default})
	}
	stmts, err = c.PlanAlter(ctx, child, fields, nil, []FKSpec{{Orig: "fk_it_child_parent", Drop: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecDDL(ctx, child, stmts); err != nil {
		t.Fatalf("drop fk: %v\n%v", err, stmts)
	}
	if fks, _ = c.ForeignKeys(ctx, child); len(fks) != 1 || fks[0].Name != "fk_it_child_alt" {
		t.Fatalf("after drop: %+v", fks)
	}
	if _, err := c.PlanAlter(ctx, child, fields, nil, []FKSpec{{Orig: "does_not_exist", Drop: true}}); err == nil {
		t.Error("dropping an unknown FK must be rejected")
	}
}

func TestIntegrationMetaPostgres(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_PG", Postgres)
	cfg.Database = "postgres"
	runMetaIntegration(t, cfg, Ref{Database: "postgres", Schema: "public"}, "integer")
}

func TestIntegrationMetaMySQL(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_MYSQL", MySQL)
	cfg.Database = "shop"
	runMetaIntegration(t, cfg, Ref{Database: "shop"}, "int")
}

func TestIntegrationMetaMariaDB(t *testing.T) {
	cfg := itConfig(t, "DBCAT_IT_MARIADB", MySQL)
	cfg.Database = "shop"
	runMetaIntegration(t, cfg, Ref{Database: "shop"}, "int")
}
