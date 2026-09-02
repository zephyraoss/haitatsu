package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

const changeChannel = "haitatsu_mail_changes"

type ChangeBus struct {
	db *sql.DB
}

func NewChangeBus(db *sql.DB) *ChangeBus {
	return &ChangeBus{db: db}
}

func (b *ChangeBus) Publish(ctx context.Context, payload []byte) error {
	if len(payload) > 7900 {
		return errors.New("change payload too large for NOTIFY")
	}
	_, err := b.db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, changeChannel, string(payload))
	return err
}

func (b *ChangeBus) Subscribe(ctx context.Context, handler func(payload []byte)) error {
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

func (b *ChangeBus) listen(ctx context.Context, handler func(payload []byte)) error {
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
