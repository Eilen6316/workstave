package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrateIdentityAndResults adds operation identity, Run intent and Mailbox conclusion data.
// Existing Mailbox IDs become stable legacy operation IDs so pending inputs remain addressable.
func migrateIdentityAndResults(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin identity migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	columns := []struct {
		table string
		name  string
		ddl   string
	}{
		{"mailbox", "op_id", `ALTER TABLE mailbox ADD COLUMN op_id TEXT NOT NULL DEFAULT ''`},
		{"mailbox", "result", `ALTER TABLE mailbox ADD COLUMN result TEXT CHECK (result IN ('completed','failed','unknown'))`},
		{"mailbox", "result_detail", `ALTER TABLE mailbox ADD COLUMN result_detail TEXT`},
		{"runs", "intent_hash", `ALTER TABLE runs ADD COLUMN intent_hash TEXT NOT NULL DEFAULT ''`},
		{"work_facts", "confirmation_basis", `ALTER TABLE work_facts ADD COLUMN confirmation_basis TEXT NOT NULL DEFAULT 'legacy'`},
		{"works", "main_harness", `ALTER TABLE works ADD COLUMN main_harness TEXT NOT NULL DEFAULT 'codex' CHECK (main_harness IN ('codex','claude-code'))`},
		{"works", "working_dir", `ALTER TABLE works ADD COLUMN working_dir TEXT NOT NULL DEFAULT ''`},
		{"mailbox", "harness_experiment", `ALTER TABLE mailbox ADD COLUMN harness_experiment INTEGER NOT NULL DEFAULT 0 CHECK (harness_experiment IN (0,1))`},
	}
	for _, column := range columns {
		var count int
		query := `SELECT COUNT(*) FROM pragma_table_info('` + column.table + `') WHERE name = ?`
		if err := tx.QueryRowContext(ctx, query, column.name).Scan(&count); err != nil {
			return fmt.Errorf("store: inspect %s.%s: %w", column.table, column.name, err)
		}
		if count == 0 {
			if _, err := tx.ExecContext(ctx, column.ddl); err != nil {
				return fmt.Errorf("store: add %s.%s: %w", column.table, column.name, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox SET op_id = 'main-' || message_id WHERE op_id = ''`); err != nil {
		return fmt.Errorf("store: assign legacy Mailbox operation identities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE works SET main_harness = COALESCE(
		(SELECT b.harness FROM native_bindings b WHERE b.session_id = works.main_session_id AND b.active = 1), main_harness)`); err != nil {
		return fmt.Errorf("store: migrate current Main Harness: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox SET result = CASE
		WHEN state = 'unknown' THEN 'unknown'
		WHEN EXISTS (SELECT 1 FROM main_runs mr JOIN runs r ON r.id = mr.run_id
			WHERE mr.mailbox_sequence = mailbox.sequence AND r.outcome = 'failed') THEN 'failed'
		WHEN state = 'completed' THEN 'completed'
		ELSE NULL END WHERE result IS NULL`); err != nil {
		return fmt.Errorf("store: backfill Mailbox conclusions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS mailbox_by_operation ON mailbox(op_id)`); err != nil {
		return fmt.Errorf("store: index Mailbox operation identities: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit identity migration: %w", err)
	}
	return nil
}
