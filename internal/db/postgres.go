package db

import (
	"context"
	"database/sql"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type postgresDialect struct{}

func (postgresDialect) open(cfg Config, database string) (*sql.DB, error) {
	host := cfg.Host
	if host == "" {
		host = "localhost"
	}
	port := cfg.Port
	if port == 0 {
		port = 5432
	}
	q := url.Values{"connect_timeout": {"10"}}
	switch cfg.SSLMode {
	case SSLDisable:
		q.Set("sslmode", "disable")
	case SSLRequire:
		q.Set("sslmode", "require")
	case SSLVerify:
		q.Set("sslmode", "verify-full")
	default:
		q.Set("sslmode", "prefer")
	}
	if cfg.SSLCA != "" {
		q.Set("sslrootcert", expandHome(cfg.SSLCA))
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/" + database,
		RawQuery: q.Encode(),
	}
	cc, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, err
	}
	if cfg.dial != nil {
		cc.DialFunc = cfg.dial
		// Do not resolve the host here: through a tunnel the name only exists on
		// the SSH server's side, which resolves it when asked to connect.
		cc.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
	}
	return stdlib.OpenDB(*cc), nil
}

func (postgresDialect) poolKey(database string) string { return database }
func (postgresDialect) hasSchemas() bool               { return true }
func (postgresDialect) defaultDatabase(cfg Config) string {
	if cfg.Database != "" {
		return cfg.Database
	}
	return "postgres"
}
func (postgresDialect) quote(s string) string    { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (postgresDialect) placeholder(n int) string { return "$" + strconv.Itoa(n) }
func (d postgresDialect) qualified(r Ref) string {
	return joinNonEmpty(".", d.quote(r.Schema), d.quote(r.Table))
}

func (postgresDialect) useSchema(ctx context.Context, c *sql.Conn, r Ref) error {
	if r.Schema == "" {
		return nil
	}
	_, err := c.ExecContext(ctx, "SELECT set_config('search_path', $1, false)", r.Schema)
	return err
}

func (postgresDialect) databases(ctx context.Context, p *sql.DB) ([]string, error) {
	return queryStrings(ctx, p, `SELECT datname FROM pg_database
		WHERE NOT datistemplate AND datallowconn ORDER BY 1`)
}

func (postgresDialect) schemas(ctx context.Context, p *sql.DB, _ string) ([]string, error) {
	return queryStrings(ctx, p, `SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('pg_catalog','information_schema')
		  AND schema_name NOT LIKE 'pg\_toast%' AND schema_name NOT LIKE 'pg\_temp%' ORDER BY 1`)
}

func (postgresDialect) tables(ctx context.Context, p *sql.DB, r Ref) ([]Table, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT c.relname, c.relkind IN ('v','m'),
		       CASE WHEN c.relkind IN ('r','p','m') THEN c.reltuples::bigint ELSE -1 END,
		       coalesce(obj_description(c.oid,'pg_class'),'')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','f') ORDER BY 1`, r.Schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Table
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Name, &t.IsView, &t.Rows, &t.Comment); err != nil {
			return nil, err
		}
		if t.Rows < 0 {
			t.Rows = -1
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (postgresDialect) schemaColumns(ctx context.Context, p *sql.DB, r Ref, limit int) (map[string][]string, error) {
	rows, err := p.QueryContext(ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = $1 ORDER BY table_name, ordinal_position`, r.Schema)
	if err != nil {
		return nil, err
	}
	return collectColumns(rows, limit)
}

func (postgresDialect) foreignKeys(ctx context.Context, p *sql.DB, r Ref, all bool) ([]ForeignKey, error) {
	table := r.Table
	if all {
		table = ""
	}
	rows, err := p.QueryContext(ctx, `
		SELECT con.conname, cl.relname, rn.nspname, rcl.relname,
		       (SELECT string_agg(a.attname, chr(31) ORDER BY k.ord)
		          FROM unnest(con.conkey) WITH ORDINALITY k(attnum, ord)
		          JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum),
		       (SELECT string_agg(a.attname, chr(31) ORDER BY k.ord)
		          FROM unnest(con.confkey) WITH ORDINALITY k(attnum, ord)
		          JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.attnum),
		       con.confupdtype::text, con.confdeltype::text
		FROM pg_constraint con
		JOIN pg_class cl ON cl.oid = con.conrelid
		JOIN pg_namespace ns ON ns.oid = cl.relnamespace
		JOIN pg_class rcl ON rcl.oid = con.confrelid
		JOIN pg_namespace rn ON rn.oid = rcl.relnamespace
		WHERE con.contype = 'f' AND ns.nspname = $1 AND ($2::text = '' OR cl.relname = $2)
		ORDER BY cl.relname, con.conname`, r.Schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ForeignKey
	for rows.Next() {
		var fk ForeignKey
		var cols, refCols, upd, del string
		if err := rows.Scan(&fk.Name, &fk.Table, &fk.RefSchema, &fk.RefTable, &cols, &refCols, &upd, &del); err != nil {
			return nil, err
		}
		fk.Columns, fk.RefColumns = strings.Split(cols, "\x1f"), strings.Split(refCols, "\x1f")
		fk.OnUpdate, fk.OnDelete = pgAction(upd), pgAction(del)
		out = append(out, fk)
	}
	return out, rows.Err()
}

func (postgresDialect) columns(ctx context.Context, p *sql.DB, r Ref) ([]Column, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
		       coalesce(pg_get_expr(d.adbin, d.adrelid),''),
		       EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = a.attrelid
		               AND i.indisprimary AND a.attnum = ANY(i.indkey)),
		       coalesce(col_description(a.attrelid, a.attnum),'')
		FROM pg_attribute a
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = (quote_ident($1) || '.' || quote_ident($2))::regclass
		  AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, r.Schema, r.Table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.Default, &c.PK, &c.Comment); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (postgresDialect) indexes(ctx context.Context, p *sql.DB, r Ref) ([]Index, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT ic.relname, i.indisunique, i.indisprimary,
		       array_to_string(array(
		         SELECT pg_get_indexdef(i.indexrelid, k + 1, true)
		         FROM generate_subscripts(i.indkey, 1) AS k ORDER BY k), ', ')
		FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
		WHERE i.indrelid = (quote_ident($1) || '.' || quote_ident($2))::regclass
		ORDER BY 1`, r.Schema, r.Table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Index
	for rows.Next() {
		var x Index
		if err := rows.Scan(&x.Name, &x.Unique, &x.Primary, &x.Columns); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func queryStrings(ctx context.Context, p *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := p.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
