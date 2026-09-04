package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

const changeChannel = "haitatsu_mail_changes"

type ChangeBus interface {
	Publish(ctx context.Context, payload []byte) error
	Subscribe(ctx context.Context, handler func(payload []byte)) error
}

type postgresChangeBus struct {
	db *sql.DB
}

func NewChangeBus(db *sql.DB, backends ...Backend) ChangeBus {
	backend := BackendPostgres
	if len(backends) > 0 {
		backend = backends[0]
	}
	switch backend {
	case BackendLibSQL:
		return &pollingChangeBus{db: db, interval: 250 * time.Millisecond}
	case BackendSQLite:
		return nil
	default:
		return &postgresChangeBus{db: db}
	}
}

func (b *postgresChangeBus) Publish(ctx context.Context, payload []byte) error {
	if len(payload) > 7900 {
		return errors.New("change payload too large for NOTIFY")
	}
	_, err := b.db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, changeChannel, string(payload))
	return err
}

func (b *postgresChangeBus) Subscribe(ctx context.Context, handler func(payload []byte)) error {
	for {
		err := b.listen(ctx, handler)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (b *postgresChangeBus) listen(ctx context.Context, handler func(payload []byte)) error {
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		pgxConn, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return errors.New("unexpected driver connection type")
		}
		raw := pgxConn.Conn()
		if _, err := raw.Exec(ctx, "LISTEN "+changeChannel); err != nil {
			return err
		}
		for {
			notification, err := raw.WaitForNotification(ctx)
			if err != nil {
				return err
			}
			handler([]byte(notification.Payload))
		}
	})
}

type pollingChangeBus struct {
	db       *sql.DB
	interval time.Duration
}

func (b *pollingChangeBus) Publish(ctx context.Context, payload []byte) error {
	_, err := b.db.ExecContext(ctx, `INSERT INTO haitatsu_mail_changes (payload, created_at) VALUES (?, ?)`, payload, time.Now().UnixMilli())
	return err
}

func (b *pollingChangeBus) Subscribe(ctx context.Context, handler func(payload []byte)) error {
	var lastID int64
	for {
		id, err := b.latestID(ctx)
		if err == nil {
			lastID = id
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	cleanup := time.NewTicker(10 * time.Minute)
	defer cleanup.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			nextID, err := b.poll(ctx, lastID, handler)
			if err == nil {
				lastID = nextID
			}
		case <-cleanup.C:
			_, _ = b.db.ExecContext(ctx, `DELETE FROM haitatsu_mail_changes WHERE created_at < ?`, time.Now().Add(-time.Hour).UnixMilli())
		}
	}
}

func (b *pollingChangeBus) latestID(ctx context.Context) (int64, error) {
	var id int64
	err := b.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM haitatsu_mail_changes`).Scan(&id)
	return id, err
}

func (b *pollingChangeBus) poll(ctx context.Context, afterID int64, handler func(payload []byte)) (int64, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, payload FROM haitatsu_mail_changes WHERE id > ? ORDER BY id LIMIT 256`, afterID)
	if err != nil {
		return afterID, err
	}
	defer rows.Close()

	type change struct {
		id      int64
		payload []byte
	}
	var changes []change
	for rows.Next() {
		var item change
		if err := rows.Scan(&item.id, &item.payload); err != nil {
			return afterID, err
		}
		changes = append(changes, item)
	}
	if err := rows.Err(); err != nil {
		return afterID, err
	}
	for _, item := range changes {
		handler(item.payload)
		afterID = item.id
	}
	return afterID, nil
}
