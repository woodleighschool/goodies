// Package pglock serializes callbacks with PostgreSQL session advisory locks.
package pglock

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Locker holds one advisory lock on a dedicated connection, leaving the pool
// available to work. Closing that connection releases the lock on every exit.
type Locker struct {
	pool *pgxpool.Pool
	key  int64
}

// New uses pool's connection configuration and the application-owned lock key.
func New(pool *pgxpool.Pool, key int64) *Locker {
	return &Locker{pool: pool, key: key}
}

// Try runs work if the lock is immediately available. It returns false without
// calling work when another session holds the lock.
func (l *Locker) Try(ctx context.Context, work func(context.Context) error) (bool, error) {
	return l.run(ctx, false, work)
}

// With waits for the lock before running work. Cancellation stops the wait.
func (l *Locker) With(ctx context.Context, work func(context.Context) error) error {
	_, err := l.run(ctx, true, work)
	return err
}

func (l *Locker) run(ctx context.Context, wait bool, work func(context.Context) error) (bool, error) {
	conn, err := pgx.ConnectConfig(ctx, l.pool.Config().ConnConfig)
	if err != nil {
		return false, fmt.Errorf("connect for advisory lock: %w", err)
	}
	defer func() {
		// pgx always closes the socket, even when a clean protocol close fails.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if wait {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", l.key); err != nil {
			return false, fmt.Errorf("acquire advisory lock: %w", err)
		}
	} else {
		var acquired bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", l.key).Scan(&acquired); err != nil {
			return false, fmt.Errorf("acquire advisory lock: %w", err)
		}
		if !acquired {
			return false, nil
		}
	}
	return true, work(ctx)
}
