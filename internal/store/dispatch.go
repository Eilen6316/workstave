package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Dispatch states of a run's external submission (Engineering Contracts C3.4). They are kept
// apart from the observed run lifecycle in runs.outcome: a run can be acknowledged and still
// have no outcome. Only the states P0-A needs exist.
const (
	DispatchPending      = "pending"      // intent persisted, no external call attempted
	DispatchDispatching  = "dispatching"  // external call attempted or about to be; no return value does not mean not executed
	DispatchAcknowledged = "acknowledged" // a correlatable acknowledgement was observed
)

// migrateDispatch adds the dispatch column to a database created before it existed. Rows of
// such a database that have no outcome were launched by an older binary that recorded no
// dispatch state, so they are conservatively marked dispatching, never pending.
func migrateDispatch(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'dispatch'`).Scan(&n); err != nil {
		return fmt.Errorf("store: inspect runs table: %w", err)
	}
	if n == 1 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin dispatch migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN dispatch TEXT NOT NULL DEFAULT 'pending' CHECK (dispatch IN ('pending','dispatching','acknowledged'))`); err != nil {
		return fmt.Errorf("store: add dispatch column: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET dispatch = 'dispatching' WHERE outcome IS NULL`); err != nil {
		return fmt.Errorf("store: backfill dispatch state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit dispatch migration: %w", err)
	}
	return nil
}

func (d *DB) setDispatch(ctx context.Context, runID, from, to string) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE runs SET dispatch = ? WHERE id = ? AND dispatch = ? AND outcome IS NULL`, to, runID, from)
	if err != nil {
		return fmt.Errorf("store: run %q %s -> %s: %w", runID, from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: run %q %s -> %s: %w", runID, from, to, err)
	}
	if n == 0 {
		return fmt.Errorf("store: run %q is not an unfinished %s run", runID, from)
	}
	return nil
}

// MarkDispatching records that the external call is about to be attempted. Commit it before
// the call: if the process dies afterwards, the missing return value must not be read as
// "not executed".
func (d *DB) MarkDispatching(ctx context.Context, runID string) error {
	return d.setDispatch(ctx, runID, DispatchPending, DispatchDispatching)
}

// MarkAcknowledged records that a correlatable acknowledgement was observed. It does not say
// the run is running, finished or cancelled.
func (d *DB) MarkAcknowledged(ctx context.Context, runID string) error {
	return d.setDispatch(ctx, runID, DispatchDispatching, DispatchAcknowledged)
}

// DispatchState returns the dispatch state of a run.
func (d *DB) DispatchState(ctx context.Context, runID string) (string, error) {
	var s string
	if err := d.sql.QueryRowContext(ctx, `SELECT dispatch FROM runs WHERE id = ?`, runID).Scan(&s); err != nil {
		return "", fmt.Errorf("store: dispatch state of run %q: %w", runID, err)
	}
	return s, nil
}

// Recovery reports what RecoverOnStart did.
type Recovery struct {
	Pending int64 // runs left pending: never dispatched, still eligible for a fresh dispatch authorization
	Unknown int64 // runs whose external submission may have happened and whose end was not observed
}

// RecoverOnStart applies Engineering Contracts C6.2 to runs without an outcome. Pending runs
// are left untouched. Dispatching and acknowledged runs become unknown, because this build has
// no way to find out whether their process is still alive (process discovery is Not Tested), so
// there is no evidence to keep them running or to prove they never executed. An unknown run
// restricts only operations that conflict with it. Call it once at start, after the state
// directory lock is taken and before any new dispatch.
func (d *DB) RecoverOnStart(ctx context.Context) (Recovery, error) {
	var r Recovery
	if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE outcome IS NULL AND dispatch = 'pending'`).Scan(&r.Pending); err != nil {
		return Recovery{}, fmt.Errorf("store: count pending runs: %w", err)
	}
	res, err := d.sql.ExecContext(ctx, `UPDATE runs SET outcome = 'unknown', ended_at = ? WHERE outcome IS NULL AND dispatch IN ('dispatching','acknowledged')`, nowMS())
	if err != nil {
		return Recovery{}, fmt.Errorf("store: mark undispatched-or-unobserved runs: %w", err)
	}
	if r.Unknown, err = res.RowsAffected(); err != nil {
		return Recovery{}, fmt.Errorf("store: count unknown runs: %w", err)
	}
	return r, nil
}
