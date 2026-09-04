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

const messageSearchIndexSQL = `
CREATE INDEX IF NOT EXISTS messages_search_vector_idx
ON messages USING GIN (to_tsvector('english', coalesce(subject, '') || ' ' || coalesce(from_addresses::text, '') || ' ' || coalesce(to_addresses::text, '') || ' ' || coalesce(cc_addresses::text, '') || ' ' || coalesce(text_body_extract, '') || ' ' || coalesce(html_body_extract, '') || ' ' || coalesce(attachments::text, '')))
`

var sqliteMessageSearchSQL = []string{
	`CREATE VIRTUAL TABLE IF NOT EXISTS messages_search USING fts5(
		message_id UNINDEXED,
		subject,
		from_addresses,
		to_addresses,
		cc_addresses,
		text_body_extract,
		html_body_extract,
		attachments
	)`,
	`INSERT INTO messages_search (message_id, subject, from_addresses, to_addresses, cc_addresses, text_body_extract, html_body_extract, attachments)
	 SELECT id, subject, from_addresses, to_addresses, cc_addresses, text_body_extract, html_body_extract, attachments
	 FROM messages`,
	`CREATE TRIGGER IF NOT EXISTS messages_search_insert AFTER INSERT ON messages BEGIN
		INSERT INTO messages_search (message_id, subject, from_addresses, to_addresses, cc_addresses, text_body_extract, html_body_extract, attachments)
		VALUES (new.id, new.subject, new.from_addresses, new.to_addresses, new.cc_addresses, new.text_body_extract, new.html_body_extract, new.attachments);
	END`,
	`CREATE TRIGGER IF NOT EXISTS messages_search_update AFTER UPDATE OF subject, from_addresses, to_addresses, cc_addresses, text_body_extract, html_body_extract, attachments ON messages BEGIN
		DELETE FROM messages_search WHERE message_id = old.id;
		INSERT INTO messages_search (message_id, subject, from_addresses, to_addresses, cc_addresses, text_body_extract, html_body_extract, attachments)
		VALUES (new.id, new.subject, new.from_addresses, new.to_addresses, new.cc_addresses, new.text_body_extract, new.html_body_extract, new.attachments);
	END`,
	`CREATE TRIGGER IF NOT EXISTS messages_search_delete AFTER DELETE ON messages BEGIN
		DELETE FROM messages_search WHERE message_id = old.id;
	END`,
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
		if _, err := tx.ExecContext(ctx, c.query(`INSERT INTO schema_migrations (id, applied_at) VALUES ($1, $2)`, `INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`), migration.ID, time.Now().UTC()); err != nil {
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
	if err := c.db.QueryRowContext(ctx, c.query(`SELECT count(*) FROM schema_migrations WHERE id = $1`, `SELECT count(*) FROM schema_migrations WHERE id = ?`), id).Scan(&count); err != nil {
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

func (c *Client) query(postgres string, sqlite string) string {
	if c.backend.SQLiteFamily() {
		return sqlite
	}
	return postgres
}

func versionedMigrations(backend Backend) []Migration {
	sqliteFamily := backend.SQLiteFamily()
	return []Migration{
		{
			ID: "0001_message_search_index",
			Apply: func(ctx context.Context, tx *sql.Tx) error {
				if sqliteFamily {
					return execAll(ctx, tx, sqliteMessageSearchSQL...)
				}
				return execAll(ctx, tx, messageSearchIndexSQL)
			},
		},
		{
			ID: "0002_backfill_imap_uids",
			Apply: func(ctx context.Context, tx *sql.Tx) error {
				if sqliteFamily {
					return execAll(ctx, tx,
						`UPDATE folders SET uid_validity = CAST(strftime('%s', created_at) AS INTEGER) WHERE uid_validity <= 1`,
						`UPDATE labels SET uid_validity = CAST(strftime('%s', created_at) AS INTEGER) WHERE uid_validity <= 1`,
						`WITH numbered AS (
						   SELECT id, folder_id, row_number() OVER (PARTITION BY folder_id ORDER BY created_at, id) AS seq
						   FROM mailbox_messages WHERE uid = 0
						 )
						 UPDATE mailbox_messages SET uid = (
						   SELECT folders.uid_next + numbered.seq - 1
						   FROM numbered JOIN folders ON folders.id = numbered.folder_id
						   WHERE numbered.id = mailbox_messages.id
						 ) WHERE id IN (SELECT id FROM numbered)`,
						`UPDATE folders SET uid_next = COALESCE((SELECT max(uid) + 1 FROM mailbox_messages WHERE folder_id = folders.id), 1)`,
						`WITH numbered AS (
						   SELECT id, label_id, row_number() OVER (PARTITION BY label_id ORDER BY created_at, id) AS seq
						   FROM mailbox_message_labels WHERE uid = 0
						 )
						 UPDATE mailbox_message_labels SET uid = (
						   SELECT labels.uid_next + numbered.seq - 1
						   FROM numbered JOIN labels ON labels.id = numbered.label_id
						   WHERE numbered.id = mailbox_message_labels.id
						 ) WHERE id IN (SELECT id FROM numbered)`,
						`UPDATE labels SET uid_next = COALESCE((SELECT max(uid) + 1 FROM mailbox_message_labels WHERE label_id = labels.id), 1)`,
					)
				}
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
				if sqliteFamily {
					return execAll(ctx, tx,
						`UPDATE mailboxes SET used_bytes = COALESCE((
						   SELECT sum(messages.size_bytes) FROM mailbox_messages JOIN messages ON messages.id = mailbox_messages.message_id
						   WHERE mailbox_messages.mailbox_id = mailboxes.id AND mailbox_messages.deleted_at IS NULL
						 ), 0)`,
					)
				}
				return execAll(ctx, tx,
					`UPDATE mailboxes mb SET used_bytes = COALESCE((
					   SELECT sum(m.size_bytes) FROM mailbox_messages mm JOIN messages m ON m.id = mm.message_id
					   WHERE mm.mailbox_id = mb.id AND mm.deleted_at IS NULL
					 ), 0)`,
				)
			},
		},
		{
			ID: "0004_sqlite_change_bus",
			Apply: func(ctx context.Context, tx *sql.Tx) error {
				if !sqliteFamily {
					return nil
				}
				return execAll(ctx, tx,
					`CREATE TABLE IF NOT EXISTS haitatsu_mail_changes (
					   id INTEGER PRIMARY KEY AUTOINCREMENT,
					   payload BLOB NOT NULL,
					   created_at INTEGER NOT NULL
					 )`,
					`CREATE INDEX IF NOT EXISTS haitatsu_mail_changes_created_at ON haitatsu_mail_changes (created_at)`,
				)
			},
		},
	}
}
