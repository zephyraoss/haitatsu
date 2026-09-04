package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	sqldriver "database/sql/driver"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
	libsqlclient "github.com/tursodatabase/libsql-client-go/libsql"
	_ "modernc.org/sqlite"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
)

type Backend string

const (
	BackendPostgres Backend = "postgres"
	BackendSQLite   Backend = "sqlite"
	BackendLibSQL   Backend = "libsql"
)

func (b Backend) SQLiteFamily() bool {
	return b == BackendSQLite || b == BackendLibSQL
}

type Client struct {
	db             *sql.DB
	ent            *ent.Client
	backend        Backend
	migrationsDone atomic.Bool
}

func Open(ctx context.Context, cfg config.DatabaseConfig) (*Client, error) {
	backend := Backend(strings.ToLower(strings.TrimSpace(cfg.Driver)))
	var db *sql.DB
	var err error
	switch backend {
	case BackendPostgres:
		db, err = sql.Open("pgx", cfg.DSN)
	case BackendSQLite:
		db, err = sql.Open("sqlite", sqliteDSN(cfg.DSN))
	case BackendLibSQL:
		options := make([]libsqlclient.Option, 0, 2)
		if cfg.AuthToken != "" {
			options = append(options, libsqlclient.WithAuthToken(cfg.AuthToken))
		}
		if cfg.Namespace != "" {
			options = append(options, libsqlclient.WithRequestHeaders(map[string]string{
				"x-namespace": cfg.Namespace,
			}))
		}
		connector, connectorErr := libsqlclient.NewConnector(cfg.DSN, options...)
		if connectorErr != nil {
			return nil, fmt.Errorf("configure libsql connector: %w", connectorErr)
		}
		db = sql.OpenDB(&foreignKeyConnector{Connector: connector})
	default:
		return nil, fmt.Errorf("unsupported database driver %q", cfg.Driver)
	}
	if err != nil {
		return nil, err
	}

	configurePool(db, backend)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}

	entDialect := dialect.Postgres
	if backend.SQLiteFamily() {
		entDialect = dialect.SQLite
	}
	driver := entsql.OpenDB(entDialect, db)
	return &Client{db: db, ent: ent.NewClient(ent.Driver(driver)), backend: backend}, nil
}

type foreignKeyConnector struct {
	sqldriver.Connector
}

func (c *foreignKeyConnector) Connect(ctx context.Context) (sqldriver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(sqldriver.ExecerContext); ok {
		if _, err := execer.ExecContext(ctx, `PRAGMA foreign_keys = ON`, nil); err != nil {
			conn.Close()
			return nil, fmt.Errorf("enable libsql foreign keys: %w", err)
		}
		return conn, nil
	}
	statement, err := conn.Prepare(`PRAGMA foreign_keys = ON`)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("prepare libsql foreign key pragma: %w", err)
	}
	defer statement.Close()
	if _, err := statement.Exec(nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enable libsql foreign keys: %w", err)
	}
	return conn, nil
}

func configurePool(db *sql.DB, backend Backend) {
	switch backend {
	case BackendPostgres:
		db.SetMaxOpenConns(50)
		db.SetMaxIdleConns(10)
	case BackendSQLite:
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(5)
	case BackendLibSQL:
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(5)
	}
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
}

func sqliteDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == ":memory:" {
		dsn = "file:haitatsu?mode=memory&cache=shared"
	} else if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_time_format=sqlite"
}

const migrationLockKey = 7250316

func (c *Client) RunMigrations(ctx context.Context) error {
	release, err := c.acquireMigrationLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	if err := c.ent.Schema.Create(ctx); err != nil {
		return err
	}
	if err := c.runVersionedMigrations(ctx, versionedMigrations(c.backend)); err != nil {
		return err
	}
	c.migrationsDone.Store(true)
	return nil
}

func (c *Client) acquireMigrationLock(ctx context.Context) (func(), error) {
	if c.backend == BackendPostgres {
		lockConn, err := c.db.Conn(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := lockConn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
			lockConn.Close()
			return nil, err
		}
		return func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = lockConn.ExecContext(releaseCtx, `SELECT pg_advisory_unlock($1)`, migrationLockKey)
			_ = lockConn.Close()
		}, nil
	}

	if _, err := c.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS haitatsu_migration_lock (
		id INTEGER PRIMARY KEY,
		owner TEXT NOT NULL,
		locked_until INTEGER NOT NULL
	)`); err != nil {
		return nil, err
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO haitatsu_migration_lock (id, owner, locked_until) VALUES (1, '', 0) ON CONFLICT(id) DO NOTHING`); err != nil {
		return nil, err
	}
	owner, err := randomLockOwner()
	if err != nil {
		return nil, err
	}
	for {
		now := time.Now().Unix()
		result, err := c.db.ExecContext(ctx, `UPDATE haitatsu_migration_lock SET owner = ?, locked_until = ? WHERE id = 1 AND locked_until < ?`, owner, now+300, now)
		if err == nil {
			changed, changedErr := result.RowsAffected()
			if changedErr != nil {
				return nil, fmt.Errorf("read migration lock result: %w", changedErr)
			}
			if changed == 1 {
				return func() {
					releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, _ = c.db.ExecContext(releaseCtx, `UPDATE haitatsu_migration_lock SET owner = '', locked_until = 0 WHERE id = 1 AND owner = ?`, owner)
				}, nil
			}
		} else if !sqliteBusy(err) {
			return nil, fmt.Errorf("acquire migration lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func sqliteBusy(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy")
}

func randomLockOwner() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (c *Client) Health(ctx context.Context) error {
	return c.db.PingContext(ctx)
}

func (c *Client) MigrationsDone() bool {
	return c.migrationsDone.Load()
}

func (c *Client) Close() {
	if c == nil {
		return
	}
	if c.ent != nil {
		c.ent.Close()
	}
	if c.db != nil {
		c.db.Close()
	}
}

func (c *Client) Ent() *ent.Client {
	return c.ent
}

func (c *Client) SQL() *sql.DB {
	return c.db
}

func (c *Client) Backend() Backend {
	return c.backend
}

func (c *Client) Dialect() string {
	if c.backend.SQLiteFamily() {
		return dialect.SQLite
	}
	return dialect.Postgres
}
