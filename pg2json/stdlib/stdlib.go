// Package stdlib registers pg2json as a database/sql driver named
// "pg2json". Import it for side effects:
//
//	import (
//	    "database/sql"
//	    _ "github.com/arturoeanton/pg2json/pg2json/stdlib"
//	)
//
//	db, err := sql.Open("pg2json", "postgres://user:pass@host/db?sslmode=disable")
//	rows, err := db.Query("SELECT id, name FROM users")
//
// Scope:
//   - Read-only. Exec / ExecContext / Begin / BeginTx return an error.
//     Writes must go through pgx (e.g. github.com/jackc/pgx/v5/stdlib)
//     in a separate *sql.DB pool pointing at the same Postgres.
//   - Query / QueryContext / QueryRow / Prepare / Ping all work.
//
// Performance note: going through database/sql adds interface boxing
// (driver.Value per cell, sql.Rows.Scan conversion). The native API
// (pg2json.ScanStruct, pg2json.StreamNDJSON, etc.) is 10-30 % faster
// and has 2-3x fewer allocs. Pick this adapter for drop-in compat
// with existing code (sqlc, goose, etc.); pick the native API when
// a read-path hot spot shows up in profiles.
package stdlib

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/arturoeanton/pg2json/internal/rows"
	"github.com/arturoeanton/pg2json/pg2json"
)

func init() {
	sql.Register("pg2json", &Driver{})
}

// Driver is the registered database/sql driver. Users typically do
// not reference this type directly — sql.Open("pg2json", ...) is all
// that is required.
type Driver struct{}

// Open parses dsn and connects. Used by sql.DB when no Connector is
// registered. Prefer NewConnector if you need to customise Config
// fields that ParseDSN does not expose (TLS callbacks, RuntimeParams,
// etc.).
func (Driver) Open(dsn string) (driver.Conn, error) {
	cfg, err := pg2json.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	c, err := pg2json.Open(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	return &Conn{client: c}, nil
}

// OpenConnector returns a database/sql Connector bound to dsn.
// sql.DB uses this to open new connections on demand.
func (Driver) OpenConnector(dsn string) (driver.Connector, error) {
	cfg, err := pg2json.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return &connector{cfg: cfg}, nil
}

// NewConnector builds a Connector from an already-constructed Config.
// Useful when ParseDSN cannot express the settings you need (for
// example, a TLS config with a custom RootCAs pool).
func NewConnector(cfg pg2json.Config) driver.Connector { return &connector{cfg: cfg} }

type connector struct {
	cfg pg2json.Config
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	cl, err := pg2json.Open(ctx, c.cfg)
	if err != nil {
		return nil, err
	}
	return &Conn{client: cl}, nil
}

func (c *connector) Driver() driver.Driver { return Driver{} }

// errReadOnly is returned for any write-path method.
var errReadOnly = errors.New("pg2json: driver is read-only (SELECT only); use pgx for writes — two sql.DB pools to the same Postgres coexist cleanly")

// Conn implements driver.Conn / ConnPrepareContext / ConnBeginTx /
// ExecerContext / QueryerContext / Pinger / Validator.
type Conn struct {
	client *pg2json.Client
	bad    bool
}

func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *Conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return &Stmt{c: c, query: query, numInput: -1}, nil
}

func (c *Conn) Close() error {
	if c.client == nil {
		return nil
	}
	err := c.client.Close()
	c.client = nil
	return err
}

// Begin implements driver.Conn but always returns errReadOnly.
// database/sql calls this for legacy BeginTx-less code paths.
func (c *Conn) Begin() (driver.Tx, error) { return nil, errReadOnly }

// BeginTx implements driver.ConnBeginTx. We reject any transaction
// attempt — pg2json is read-only and transactions imply a write-path
// contract users will expect us to honour.
func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return nil, errReadOnly
}

// ExecContext always returns errReadOnly. Provided so database/sql
// routes writes to this error rather than falling back to the
// Prepare+Exec dance and failing opaquely.
func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return nil, errReadOnly
}

// QueryContext is the fast path for database/sql.Query without a
// separate Prepare step. args are converted via namedToAny.
func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	anyArgs, err := namedToAny(args)
	if err != nil {
		return nil, err
	}
	it, err := c.client.RawQuery(ctx, query, anyArgs...)
	if err != nil {
		return nil, err
	}
	return newRows(it)
}

// Ping forwards to the underlying client.
func (c *Conn) Ping(ctx context.Context) error {
	if c.client == nil {
		return driver.ErrBadConn
	}
	return c.client.Ping(ctx)
}

// IsValid is called by sql.DB's connection validator.
func (c *Conn) IsValid() bool { return c.client != nil && !c.bad }

// Stmt implements driver.Stmt / StmtQueryContext / StmtExecContext.
type Stmt struct {
	c        *Conn
	query    string
	numInput int // -1 means unknown; database/sql tolerates that.
}

func (s *Stmt) Close() error { return nil } // nothing to release; real caching lives in pg2json.stmts

func (s *Stmt) NumInput() int { return s.numInput }

func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, errReadOnly
}

func (s *Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return nil, errReadOnly
}

func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamed(args))
}

func (s *Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	anyArgs, err := namedToAny(args)
	if err != nil {
		return nil, err
	}
	it, err := s.c.client.RawQuery(ctx, s.query, anyArgs...)
	if err != nil {
		return nil, err
	}
	return newRows(it)
}

// --- Rows ---------------------------------------------------------------

type pgRows struct {
	it       *pg2json.Iterator
	plan     *rows.Plan
	decoders []func([]byte) any
	names    []string
}

func newRows(it *pg2json.Iterator) (*pgRows, error) {
	plan := it.Plan()
	if plan == nil {
		// Shouldn't happen — RawQuery resolves the plan before return.
		_ = it.Close()
		return nil, fmt.Errorf("pg2json/stdlib: no plan resolved for query")
	}
	names := make([]string, len(plan.Columns))
	decoders := make([]func([]byte) any, len(plan.Columns))
	for i, col := range plan.Columns {
		names[i] = col.Name
		decoders[i] = pg2json.DriverValueDecoder(uint32(col.TypeOID), col.Format)
	}
	return &pgRows{it: it, plan: plan, decoders: decoders, names: names}, nil
}

func (r *pgRows) Columns() []string { return r.names }

func (r *pgRows) Close() error { return r.it.Close() }

func (r *pgRows) Next(dest []driver.Value) error {
	body, err := r.it.NextRaw()
	if err == io.EOF {
		return io.EOF
	}
	if err != nil {
		return err
	}
	if len(body) < 2 {
		return fmt.Errorf("pg2json/stdlib: short DataRow")
	}
	n := int(int16(binary.BigEndian.Uint16(body[0:2])))
	if n != len(r.decoders) {
		return fmt.Errorf("pg2json/stdlib: column count mismatch (got %d, plan %d)", n, len(r.decoders))
	}
	body = body[2:]
	for i := 0; i < n; i++ {
		if len(body) < 4 {
			return fmt.Errorf("pg2json/stdlib: short column header")
		}
		l := int32(uint32(body[0])<<24 | uint32(body[1])<<16 |
			uint32(body[2])<<8 | uint32(body[3]))
		body = body[4:]
		if l == -1 {
			dest[i] = nil
			continue
		}
		if l < 0 || int(l) > len(body) {
			return fmt.Errorf("pg2json/stdlib: bad column length %d", l)
		}
		raw := body[:l]
		body = body[l:]
		dest[i] = r.decoders[i](raw)
	}
	return nil
}

// --- helpers ------------------------------------------------------------

func namedToAny(args []driver.NamedValue) ([]any, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make([]any, len(args))
	for i, a := range args {
		if a.Name != "" {
			// Named parameters would require pg2json to expose the
			// $name → $N translation; database/sql passes them through
			// only when the driver opts in via NamedValueChecker. We
			// do not today, so a non-empty Name means the caller tried
			// to use named params — surface it rather than mis-bind.
			return nil, fmt.Errorf("pg2json/stdlib: named parameters not supported (got %q); use positional $1..$N", a.Name)
		}
		out[i] = a.Value
	}
	return out, nil
}

func valuesToNamed(args []driver.Value) []driver.NamedValue {
	if len(args) == 0 {
		return nil
	}
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

// Compile-time interface assertions so a missing method surfaces at
// build time rather than as a confusing runtime error.
var (
	_ driver.Driver          = Driver{}
	_ driver.DriverContext   = Driver{}
	_ driver.Connector       = (*connector)(nil)
	_ driver.Conn            = (*Conn)(nil)
	_ driver.ConnBeginTx     = (*Conn)(nil)
	_ driver.ConnPrepareContext = (*Conn)(nil)
	_ driver.ExecerContext   = (*Conn)(nil)
	_ driver.QueryerContext  = (*Conn)(nil)
	_ driver.Pinger          = (*Conn)(nil)
	_ driver.Validator       = (*Conn)(nil)
	_ driver.Stmt            = (*Stmt)(nil)
	_ driver.StmtExecContext = (*Stmt)(nil)
	_ driver.StmtQueryContext = (*Stmt)(nil)
	_ driver.Rows            = (*pgRows)(nil)
)
