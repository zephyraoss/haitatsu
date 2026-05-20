package database

import (
	"context"
	"sync/atomic"

	"entgo.io/ent/dialect"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zephyraoss/haitatsu/internal/config"
)

const EntDialect = dialect.Postgres

type Client struct {
	pool           *pgxpool.Pool
	migrationsDone atomic.Bool
}

func Open(ctx context.Context, cfg config.PostgresConfig) (*Client, error) {
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Client{pool: pool}, nil
}

func (c *Client) RunMigrations(context.Context) error {
	c.migrationsDone.Store(true)
	return nil
}

func (c *Client) Health(ctx context.Context) error {
	return c.pool.Ping(ctx)
}

func (c *Client) MigrationsDone() bool {
	return c.migrationsDone.Load()
}

func (c *Client) Close() {
	if c != nil && c.pool != nil {
		c.pool.Close()
	}
}
