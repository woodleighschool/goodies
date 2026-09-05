package pglock_test

import (
	"context"
	"errors"
	"hash/fnv"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/woodleighschool/goodies/pglock"
)

func testLocker(t *testing.T) (*pgxpool.Pool, *pglock.Locker) {
	t.Helper()
	databaseURL := os.Getenv("PGLOCK_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("PGLOCK_TEST_DATABASE_URL is required for PostgreSQL tests")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	key := fnv.New64a()
	_, _ = key.Write([]byte(t.Name()))
	return pool, pglock.New(pool, int64(key.Sum64()&0x7fffffffffffffff))
}

func TestLockedWorkLeavesPoolAvailable(t *testing.T) {
	pool, locker := testLocker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	acquired, err := locker.Try(ctx, pool.Ping)
	if err != nil || !acquired {
		t.Fatalf("acquired=%t error=%v", acquired, err)
	}
	if err := locker.With(ctx, pool.Ping); err != nil {
		t.Fatal(err)
	}
}

func TestContentionSkipsWorkAndWaitingAcquiresAfterRelease(t *testing.T) {
	_, locker := testLocker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	holderCtx, release := context.WithCancel(ctx)
	defer release()
	held := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- locker.With(holderCtx, func(ctx context.Context) error { close(held); <-ctx.Done(); return nil })
	}()
	select {
	case <-held:
	case err := <-holderDone:
		t.Fatalf("hold lock: %v", err)
	}
	acquired, err := locker.Try(ctx, func(context.Context) error { t.Error("contended work ran"); return nil })
	if err != nil || acquired {
		t.Errorf("contended acquired=%t error=%v", acquired, err)
	}
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- locker.With(ctx, func(context.Context) error { return nil }) }()
	release()
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	if err := <-waiterDone; err != nil {
		t.Fatal(err)
	}
}

func TestCallbackFailureAndPanicReleaseLock(t *testing.T) {
	_, locker := testLocker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	failed := errors.New("work failed")
	acquired, err := locker.Try(ctx, func(context.Context) error { return failed })
	if !acquired || !errors.Is(err, failed) {
		t.Fatalf("acquired=%t error=%v", acquired, err)
	}
	func() {
		defer func() {
			if got := recover(); got != "synthetic panic" {
				t.Errorf("panic=%v", got)
			}
		}()
		_ = locker.With(ctx, func(context.Context) error { panic("synthetic panic") })
	}()
	if err := locker.With(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
}

func TestWaitingCancellation(t *testing.T) {
	_, locker := testLocker(t)
	err := locker.With(t.Context(), func(context.Context) error {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		err := locker.With(ctx, func(context.Context) error { t.Error("cancelled wait ran work"); return nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("waiting error=%v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := locker.With(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledWorkReleasesLock(t *testing.T) {
	_, locker := testLocker(t)
	ctx, cancel := context.WithCancel(t.Context())
	err := locker.With(ctx, func(ctx context.Context) error { cancel(); return ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("work error=%v", err)
	}
	ctx, timeoutCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer timeoutCancel()
	if err := locker.With(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
