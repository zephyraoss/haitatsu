package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Migration struct {
	ID    string
	Apply func(ctx context.Context, tx *sql.Tx) error
}

func (c *Client) runVersionedMigrations(ctx context.Context, migrations []Migration) error {
	for _, migration := range migrations {
		applied, err := c.migrationApplied(ctx, migration.ID)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := migration.Apply(ctx, tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", migration.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (id, applied_at) VALUES ($1, $2)`, migration.ID, time.Now().UTC()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", migration.ID, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) migrationApplied(ctx context.Context, id string) (bool, error) {
	var count int
	if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE id = $1`, id).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func execAll(ctx context.Context, tx *sql.Tx, statements ...string) error {
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func versionedMigrations() []Migration {
	return []Migration{
		{
			ID: "0001_message_search_index",
			Apply: func(ctx context.Context, tx *sql.Tx) error {
				return execAll(ctx, tx, messageSearchIndexSQL)
			},
		},
		{
			ID: "0002_backfill_imap_uids",
			Apply: func(ctx context.Context, tx *sql.Tx) error {
				return execAll(ctx, tx,
					`UPDATE folders SET uid_validity = EXTRACT(EPOCH FROM created_at)::bigint WHERE uid_validity <= 1`,
					`UPDATE labels SET uid_validity = EXTRACT(EPOCH FROM created_at)::bigint WHERE uid_validity <= 1`,
					`WITH numbered AS (
					   SELECT id, folder_id, row_number() OVER (PARTITION BY folder_id ORDER BY created_at, id) AS seq
					   FROM mailbox_messages WHERE uid = 0
					 )
					 UPDATE mailbox_messages m SET uid = f.uid_next + n.seq - 1
					 FROM numbered n JOIN folders f ON f.id = n.folder_id
					 WHERE m.id = n.id`,
					`UPDATE folders f SET uid_next = COALESCE((SELECT max(uid) + 1 FROM mailbox_messages WHERE folder_id = f.id), 1)`,
					`WITH numbered AS (
					   SELECT id, label_id, row_number() OVER (PARTITION BY label_id ORDER BY created_at, id) AS seq
					   FROM mailbox_message_labels WHERE uid = 0
					 )
					 UPDATE mailbox_message_labels l SET uid = lb.uid_next + n.seq - 1
					 FROM numbered n JOIN labels lb ON lb.id = n.label_id
					 WHERE l.id = n.id`,
					`UPDATE labels l SET uid_next = COALESCE((SELECT max(uid) + 1 FROM mailbox_message_labels WHERE label_id = l.id), 1)`,
				)
			},
		},
		{
			ID: "0003_recompute_used_bytes",
			Apply: func(ctx context.Context, tx *sql.Tx) error {
				return execAll(ctx, tx,
					`UPDATE mailboxes mb SET used_bytes = COALESCE((
					   SELECT sum(m.size_bytes) FROM mailbox_messages mm JOIN messages m ON m.id = mm.message_id
					   WHERE mm.mailbox_id = mb.id AND mm.deleted_at IS NULL
					 ), 0)`,
				)
			},
		},
	}
}
