package db

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

type mysqlDialect struct{}

func (mysqlDialect) open(cfg Config, _ string) (*sql.DB, error) {
	host := cfg.Host
	if host == "" {
		host = "localhost"
	}
	port := cfg.Port
	if port == 0 {
		port = 3306
	}
	mc := mysql.NewConfig()
	mc.User = cfg.User
	mc.Passwd = cfg.Password
	mc.Net = "tcp"
	mc.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	mc.DBName = cfg.Database
	mc.ParseTime = true
	mc.ClientFoundRows = true // UPDATE reports matched rows, not changed rows
	mc.Timeout = 10 * time.Second
	mc.Collation = "utf8mb4_unicode_ci" // sets the connection charset during the handshake
	tlsConf, mode, err := mysqlTLS(cfg, host)
	if err != nil {
		return nil, err
	}
	mc.TLS, mc.TLSConfig = tlsConf, mode
	if cfg.dial != nil {
		// Register a per-connection dialer; the driver looks it up by network name.
		name := "dbcat-" + cfg.ID
		if cfg.ID == "" {
			name = "dbcat-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		mysql.RegisterDialContext(name, func(ctx context.Context, addr string) (net.Conn, error) {
			return cfg.dial(ctx, "tcp", addr)
		})
		mc.Net = name
	}
	connector, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(connector), nil
}

// mysqlTLS maps the generic mode onto the driver: either a ready tls.Config
// (verify) or one of the driver's named modes.
func mysqlTLS(cfg Config, host string) (*tls.Config, string, error) {
	switch cfg.SSLMode {
	case SSLDisable:
		return nil, "false", nil
	case SSLRequire:
		return nil, "skip-verify", nil
	case SSLVerify:
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if cfg.SSLCA != "" {
			pem, err := os.ReadFile(expandHome(cfg.SSLCA))
			if err != nil {
				return nil, "", fmt.Errorf("CA 파일을 읽을 수 없습니다: %w", err)
			}
			pool = x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, "", fmt.Errorf("CA 파일에서 인증서를 찾을 수 없습니다: %s", cfg.SSLCA)
			}
		}
		return &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12}, "", nil
	}
	return nil, "preferred", nil
}

func (mysqlDialect) poolKey(string) string { return "" }
func (mysqlDialect) hasSchemas() bool      { return false }
func (mysqlDialect) defaultDatabase(cfg Config) string {
	return cfg.Database
}
func (mysqlDialect) quote(s string) string  { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
func (mysqlDialect) placeholder(int) string { return "?" }
func (d mysqlDialect) qualified(r Ref) string {
	return joinNonEmpty(".", d.quote(r.Database), d.quote(r.Table))
}

func (d mysqlDialect) useSchema(ctx context.Context, c *sql.Conn, r Ref) error {
	if r.Database == "" {
		return nil
	}
	_, err := c.ExecContext(ctx, "USE "+d.quote(r.Database))
	return err
}

func (mysqlDialect) databases(ctx context.Context, p *sql.DB) ([]string, error) {
	return queryStrings(ctx, p, "SHOW DATABASES")
}

func (mysqlDialect) schemas(context.Context, *sql.DB, string) ([]string, error) { return nil, nil }

func (mysqlDialect) tables(ctx context.Context, p *sql.DB, r Ref) ([]Table, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT TABLE_NAME, TABLE_TYPE LIKE '%VIEW', COALESCE(TABLE_ROWS, -1), COALESCE(TABLE_COMMENT, '')
		FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME`, r.Database)
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
		out = append(out, t)
	}
	return out, rows.Err()
}

func (mysqlDialect) schemaColumns(ctx context.Context, p *sql.DB, r Ref, limit int) (map[string][]string, error) {
	rows, err := p.QueryContext(ctx, `SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME, ORDINAL_POSITION`, r.Database)
	if err != nil {
		return nil, err
	}
	return collectColumns(rows, limit)
}

func (mysqlDialect) foreignKeys(ctx context.Context, p *sql.DB, r Ref, all bool) ([]ForeignKey, error) {
	table := r.Table
	if all {
		table = ""
	}
	rows, err := p.QueryContext(ctx, `
		SELECT k.TABLE_NAME, k.CONSTRAINT_NAME, k.REFERENCED_TABLE_SCHEMA, k.REFERENCED_TABLE_NAME,
		       k.COLUMN_NAME, k.REFERENCED_COLUMN_NAME, r.UPDATE_RULE, r.DELETE_RULE
		FROM information_schema.KEY_COLUMN_USAGE k
		JOIN information_schema.REFERENTIAL_CONSTRAINTS r
		  ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA AND r.CONSTRAINT_NAME = k.CONSTRAINT_NAME AND r.TABLE_NAME = k.TABLE_NAME
		WHERE k.TABLE_SCHEMA = ? AND k.REFERENCED_TABLE_NAME IS NOT NULL AND (? = '' OR k.TABLE_NAME = ?)
		ORDER BY k.TABLE_NAME, k.CONSTRAINT_NAME, k.ORDINAL_POSITION`, r.Database, table, table)
	if err != nil {
		return nil, err
	}
	return scanForeignKeys(rows, func() (fkKey, string, string, ForeignKey, error) {
		var tbl, name, refSchema, refTable, col, refCol, upd, del string
		if err := rows.Scan(&tbl, &name, &refSchema, &refTable, &col, &refCol, &upd, &del); err != nil {
			return fkKey{}, "", "", ForeignKey{}, err
		}
		return fkKey{tbl, name}, col, refCol,
			ForeignKey{RefSchema: refSchema, RefTable: refTable, OnUpdate: upd, OnDelete: del}, nil
	})
}

func (mysqlDialect) columns(ctx context.Context, p *sql.DB, r Ref) ([]Column, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE = 'YES', COALESCE(COLUMN_DEFAULT, ''),
		       COLUMN_KEY = 'PRI', EXTRA, COLUMN_COMMENT
		FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, r.Database, r.Table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.Default, &c.PK, &c.Extra, &c.Comment); err != nil {
			return nil, err
		}
		// MariaDB reports "no default" on a nullable column as the text NULL.
		if c.Nullable && c.Default == "NULL" {
			c.Default = ""
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (mysqlDialect) indexes(ctx context.Context, p *sql.DB, r Ref) ([]Index, error) {
	rows, err := p.QueryContext(ctx, `
		SELECT INDEX_NAME, NON_UNIQUE = 0, GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX SEPARATOR ', ')
		FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		GROUP BY INDEX_NAME, NON_UNIQUE ORDER BY INDEX_NAME`, r.Database, r.Table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Index
	for rows.Next() {
		var x Index
		if err := rows.Scan(&x.Name, &x.Unique, &x.Columns); err != nil {
			return nil, err
		}
		x.Primary = x.Name == "PRIMARY"
		out = append(out, x)
	}
	return out, rows.Err()
}
