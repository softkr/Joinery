// Package db wraps SQLite, PostgreSQL and MySQL/MariaDB behind one small
// metadata + query API used by the UI layer.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

type Kind string

const (
	SQLite   Kind = "sqlite"
	Postgres Kind = "postgres"
	MySQL    Kind = "mysql"
)

func (k Kind) Valid() bool { return k == SQLite || k == Postgres || k == MySQL }

func (k Kind) Label() string {
	switch k {
	case SQLite:
		return "SQLite"
	case Postgres:
		return "PostgreSQL"
	case MySQL:
		return "MySQL / MariaDB"
	}
	return string(k)
}

// DefaultPort returns the well-known port for the engine (0 for file based).
func (k Kind) DefaultPort() int {
	switch k {
	case Postgres:
		return 5432
	case MySQL:
		return 3306
	}
	return 0
}

// TLS modes for network databases. The empty string behaves like SSLPrefer.
const (
	SSLDisable = "disable" // never use TLS
	SSLPrefer  = "prefer"  // use TLS when the server offers it
	SSLRequire = "require" // always encrypt, certificate is not verified
	SSLVerify  = "verify"  // always encrypt and verify the certificate and host name
)

// ValidSSLMode reports whether m is an accepted TLS mode ("" included).
func ValidSSLMode(m string) bool {
	switch m {
	case "", SSLDisable, SSLPrefer, SSLRequire, SSLVerify:
		return true
	}
	return false
}

// SSHConfig describes an SSH tunnel the database connection goes through.
// Password is the account password, or the passphrase of KeyFile when one is
// set; like the database password it lives in the OS keychain only.
type SSHConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user"`
	KeyFile  string `json:"key_file,omitempty"`
	Password string `json:"-"`
}

// Config describes one saved connection. Password is never written to disk
// by the store; it lives in the OS keychain.
type Config struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     Kind   `json:"kind"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"-"`
	// Database is the file path for SQLite, or the initial database otherwise.
	Database string `json:"database,omitempty"`
	SSLMode  string `json:"ssl_mode,omitempty"`
	// SSLCA is an optional PEM file with the CA used when SSLMode is "verify".
	SSLCA string     `json:"ssl_ca,omitempty"`
	SSH   *SSHConfig `json:"ssh,omitempty"`

	// dial, when set, replaces the TCP dial (used to go through the SSH tunnel).
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

type Table struct {
	Name    string
	IsView  bool
	Rows    int64 // -1 when unknown
	Comment string
}

type Column struct {
	Name     string
	Type     string
	Nullable bool
	Default  string
	PK       bool
	Extra    string
	Comment  string
}

type Index struct {
	Name    string
	Columns string
	Unique  bool
	Primary bool
}

// Ref identifies one table-like object.
type Ref struct {
	Database string
	Schema   string
	Table    string
}

type dialect interface {
	open(cfg Config, database string) (*sql.DB, error)
	// poolKey lets engines that bind a pool to one database (PostgreSQL) get one pool each.
	poolKey(database string) string
	hasSchemas() bool
	defaultDatabase(cfg Config) string
	databases(ctx context.Context, p *sql.DB) ([]string, error)
	schemas(ctx context.Context, p *sql.DB, database string) ([]string, error)
	tables(ctx context.Context, p *sql.DB, r Ref) ([]Table, error)
	columns(ctx context.Context, p *sql.DB, r Ref) ([]Column, error)
	indexes(ctx context.Context, p *sql.DB, r Ref) ([]Index, error)
	quote(ident string) string
	placeholder(n int) string
	qualified(r Ref) string
	// schemaColumnInfo lists (table, column, type, pk) rows of the whole schema.
	schemaColumnInfo(ctx context.Context, p *sql.DB, r Ref, limit int) ([]tableColumn, error)
	indexList(ctx context.Context, p *sql.DB, r Ref) ([]IndexInfo, error)
	triggerList(ctx context.Context, p *sql.DB, r Ref) ([]TriggerInfo, error)
	routineList(ctx context.Context, p *sql.DB, r Ref) ([]RoutineInfo, error)
	objectDDL(ctx context.Context, p *sql.DB, r Ref, kind, id string) (string, error)
	// ddl returns the CREATE statement(s) of a table or view.
	ddl(ctx context.Context, p *sql.DB, r Ref) (string, error)
	// foreignKeys lists foreign keys of r.Table, or of every table when all is true.
	foreignKeys(ctx context.Context, p *sql.DB, r Ref, all bool) ([]ForeignKey, error)
	// schemaColumns lists table -> columns of the database/schema in one query,
	// for editor completion. It reads at most limit columns.
	schemaColumns(ctx context.Context, p *sql.DB, r Ref, limit int) (map[string][]string, error)
	// useSchema points a dedicated connection at the selected database/schema.
	useSchema(ctx context.Context, c *sql.Conn, r Ref) error
}

func dialectFor(k Kind) (dialect, error) {
	switch k {
	case SQLite:
		return sqliteDialect{}, nil
	case Postgres:
		return postgresDialect{}, nil
	case MySQL:
		return mysqlDialect{}, nil
	}
	return nil, fmt.Errorf("unsupported database kind %q", k)
}

// Conn is a live connection (one or more pools) for a Config.
type Conn struct {
	Config Config
	d      dialect
	mu     sync.Mutex
	pools  map[string]*sql.DB
	tunnel *ssh.Client // nil unless Config.SSH is set
}

// Open validates the config and pings the server.
func Open(ctx context.Context, cfg Config) (*Conn, error) {
	d, err := dialectFor(cfg.Kind)
	if err != nil {
		return nil, err
	}
	if !ValidSSLMode(cfg.SSLMode) {
		return nil, fmt.Errorf("알 수 없는 SSL 모드: %q", cfg.SSLMode)
	}
	c := &Conn{Config: cfg, d: d, pools: map[string]*sql.DB{}}
	if cfg.SSH != nil && cfg.Kind != SQLite {
		cl, err := dialSSH(ctx, cfg.SSH)
		if err != nil {
			return nil, err
		}
		c.tunnel = cl
		c.Config.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return cl.DialContext(ctx, network, addr)
		}
	}
	p, err := c.pool(ctx, "")
	if err != nil {
		c.Close()
		return nil, err
	}
	if err := p.PingContext(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) pool(ctx context.Context, database string) (*sql.DB, error) {
	if database == "" {
		database = c.d.defaultDatabase(c.Config)
	}
	key := c.d.poolKey(database)
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.pools[key]; ok {
		return p, nil
	}
	p, err := c.d.open(c.Config, database)
	if err != nil {
		return nil, err
	}
	if err := p.PingContext(ctx); err != nil {
		p.Close()
		return nil, err
	}
	c.pools[key] = p
	return p, nil
}

func (c *Conn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, p := range c.pools {
		p.Close()
		delete(c.pools, k)
	}
	if c.tunnel != nil {
		c.tunnel.Close()
		c.tunnel = nil
	}
}

func (c *Conn) HasSchemas() bool { return c.d.hasSchemas() }

func (c *Conn) Databases(ctx context.Context) ([]string, error) {
	p, err := c.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	return c.d.databases(ctx, p)
}

func (c *Conn) Schemas(ctx context.Context, database string) ([]string, error) {
	p, err := c.pool(ctx, database)
	if err != nil {
		return nil, err
	}
	return c.d.schemas(ctx, p, database)
}

func (c *Conn) Tables(ctx context.Context, r Ref) ([]Table, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	return c.d.tables(ctx, p, r)
}

func (c *Conn) Columns(ctx context.Context, r Ref) ([]Column, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	return c.d.columns(ctx, p, r)
}

func (c *Conn) Indexes(ctx context.Context, r Ref) ([]Index, error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, err
	}
	return c.d.indexes(ctx, p, r)
}

// Cell is a display value; Null distinguishes SQL NULL from an empty string.
type Cell struct {
	V    string
	Null bool
	Lit  bool // V is a number or boolean and can be written unquoted in JSON
}

func toCell(v any) Cell {
	switch x := v.(type) {
	case nil:
		return Cell{Null: true}
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, bool:
		return Cell{V: fmt.Sprint(x), Lit: true}
	case float32, float64:
		f := reflect.ValueOf(x).Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return Cell{V: fmt.Sprint(x)}
		}
		return Cell{V: fmt.Sprint(x), Lit: true}
	case []byte:
		if utf8.Valid(x) {
			return Cell{V: string(x)}
		}
		return Cell{V: fmt.Sprintf("[BLOB %d bytes]", len(x))}
	case string:
		return Cell{V: x}
	case interface{ Format(string) string }: // time.Time
		return Cell{V: x.Format("2006-01-02 15:04:05")}
	default:
		return Cell{V: fmt.Sprint(x)}
	}
}

// scanRows reads at most limit rows and reports whether more were available.
func scanRows(rows *sql.Rows, limit int) (cols []string, out [][]Cell, more bool, err error) {
	cols, err = rows.Columns()
	if err != nil {
		return nil, nil, false, err
	}
	for rows.Next() {
		if len(out) >= limit {
			more = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, false, err
		}
		row := make([]Cell, len(cols))
		for i, v := range vals {
			row[i] = toCell(v)
		}
		out = append(out, row)
	}
	return cols, out, more, rows.Err()
}

func (c *Conn) Qualified(r Ref) string { return c.d.qualified(r) }
func (c *Conn) Quote(s string) string  { return c.d.quote(s) }

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// MaxCompletionColumns bounds the schema sent to the editor for completion.
const MaxCompletionColumns = 20000

// SchemaColumns returns table -> column names (tables and views) of r's
// database/schema for editor autocompletion. truncated is true when the
// MaxCompletionColumns limit cut the listing short.
func (c *Conn) SchemaColumns(ctx context.Context, r Ref) (tables map[string][]string, truncated bool, err error) {
	p, err := c.pool(ctx, r.Database)
	if err != nil {
		return nil, false, err
	}
	tables, err = c.d.schemaColumns(ctx, p, r, MaxCompletionColumns+1)
	if err != nil {
		return nil, false, err
	}
	n := 0
	for _, cols := range tables {
		n += len(cols)
	}
	return tables, n > MaxCompletionColumns, nil
}

// collectColumns reads (table, column) rows into a map, stopping at limit rows.
func collectColumns(rows *sql.Rows, limit int) (map[string][]string, error) {
	defer rows.Close()
	out := map[string][]string{}
	n := 0
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			return nil, err
		}
		out[table] = append(out[table], col)
		if n++; n >= limit {
			break
		}
	}
	return out, rows.Err()
}
