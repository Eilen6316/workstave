package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests exercise the open-time pragmas and the schema version on a real SQLite file.

func TestOpenSetsDurabilityAndVersion(t *testing.T) {
	db := openTemp(t)
	var sync int
	if err := db.sql.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", sync)
	}
	var v int
	if err := db.sql.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ws.db")
	db, err := Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec(t, db, `PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(context.Background(), p); err == nil {
		t.Fatal("a database with a newer schema version was opened")
	}
}

func TestOpenControlRequiresExistingCurrentSchema(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := OpenControl(ctx, missing); err == nil {
		t.Fatal("control connection opened a missing database")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control connection changed missing database state: stat err=%v", err)
	}

	path := filepath.Join(t.TempDir(), "current.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	control, err := OpenControl(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := Open(ctx, legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec(t, legacy, `PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenControl(ctx, legacyPath); err == nil {
		t.Fatal("control connection accepted a database requiring migration")
	}
}
