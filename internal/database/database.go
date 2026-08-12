package database

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
)

const EntDialect = dialect.Postgres

const messageSearchIndexSQL = `
CREATE INDEX IF NOT EXISTS messages_search_vector_idx
ON messages USING GIN (to_tsvector('english', coalesce(subject, '') || ' ' || coalesce(from_addresses::text, '') || ' ' || coalesce(to_addresses::text, '') || ' ' || coalesce(cc_addresses::text, '') || ' ' || coalesce(text_body_extract, '') || ' ' || coalesce(html_body_extract, '') || ' ' || coalesce(attachments::text, '')))
`

type Client struct {
	db             *sql.DB
	ent            *ent.Client
	migrationsDone atomic.Bool
}

func Open(ctx context.Context, cfg config.PostgresConfig) (*Client, error) {
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}

	driver := entsql.OpenDB(dialect.Postgres, db)
	return &Client{db: db, ent: ent.NewClient(ent.Driver(driver))}, nil
}

func (c *Client) RunMigrations(ctx context.Context) error {
	if err := c.ent.Schema.Create(ctx); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, messageSearchIndexSQL); err != nil {
		return err
	}
	c.migrationsDone.Store(true)
	return nil
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
