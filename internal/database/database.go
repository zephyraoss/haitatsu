package database

import (
	"context"
	"database/sql"
	"sync/atomic"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/database/ent"
)

const EntDialect = dialect.Postgres

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
