//go:build postgres || e2e

package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const databaseOperationTimeout = 10 * time.Second

// Create creates an isolated empty PostgreSQL database and registers its cleanup.
func Create(t testing.TB, baseURL string) string {
	t.Helper()

	databaseName := randomDatabaseName(t)
	adminURL, databaseURL, err := databaseURLs(baseURL, databaseName)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), databaseOperationTimeout)
	defer cancel()
	admin, err := connectAdmin(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to test database server: %v", err)
	}
	defer func() {
		if err := admin.Close(ctx); err != nil {
			t.Errorf("close test database admin connection: %v", err)
		}
	}()

	identifier := pgx.Identifier{databaseName}.Sanitize()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), databaseOperationTimeout)
		defer cleanupCancel()
		cleanupAdmin, err := connectAdmin(cleanupCtx, adminURL)
		if err != nil {
			t.Errorf("connect to drop test database %s: %v", databaseName, err)
			return
		}
		defer func() {
			if err := cleanupAdmin.Close(cleanupCtx); err != nil {
				t.Errorf("close cleanup connection for test database %s: %v", databaseName, err)
			}
		}()
		if _, err := cleanupAdmin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", databaseName, err)
		}
	})
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatalf("create test database %s: %v", databaseName, err)
	}
	return databaseURL
}

func connectAdmin(ctx context.Context, databaseURL string) (*pgx.Conn, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, config.ConnConfig)
}

func databaseURLs(baseURL string, databaseName string) (string, string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", "", err
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", "", fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	admin := *parsed
	admin.Path = "/postgres"
	admin.RawPath = ""
	target := *parsed
	target.Path = "/" + databaseName
	target.RawPath = ""
	return admin.String(), target.String(), nil
}

func randomDatabaseName(t testing.TB) string {
	t.Helper()

	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		t.Fatalf("create random test database name: %v", err)
	}
	return "bloby_test_" + hex.EncodeToString(entropy[:])
}
