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

// migrateWorkPause creates the default, unpaused state for work records that predate
// persisted pause state. The INSERT is idempotent and preserves any existing pause.
func migrateWorkPause(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO work_pause(work_id, paused) SELECT DISTINCT work_id, 0 FROM sessions`); err != nil {
		return fmt.Errorf("store: initialize work pause state: %w", err)
	}
	return nil
}

// SetWorkPaused changes the dispatch authorization state for a work. Pause and dispatch
// authorization both write SQLite state, so their commits have a single, observable order.
func (d *DB) SetWorkPaused(ctx context.Context, workID string, paused bool) error {
	value := 0
	if paused {
		value = 1
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin pause update for work %q: %w", workID, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE work_pause SET paused = ? WHERE work_id = ?`, value, workID)
	if err != nil {
		return fmt.Errorf("store: set paused state for work %q: %w", workID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set paused state for work %q: %w", workID, err)
	}
	if n == 0 {
		return fmt.Errorf("store: work %q has no persisted pause state", workID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit paused state for work %q: %w", workID, err)
	}
	return nil
}

// AuthorizeDispatch atomically checks the work's pause state and moves a pending Run to
// dispatching. Its committed update is the authorization point; callers may invoke a harness
// only after this method returns successfully.
func (d *DB) AuthorizeDispatch(ctx context.Context, runID string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin dispatch authorization for run %q: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE runs
		SET dispatch = ?
		WHERE id = ? AND dispatch = ? AND outcome IS NULL
		AND EXISTS (
			SELECT 1 FROM sessions s
			JOIN work_pause p ON p.work_id = s.work_id
			WHERE s.id = runs.session_id AND p.paused = 0
		)`, DispatchDispatching, runID, DispatchPending)
	if err != nil {
		return fmt.Errorf("store: authorize dispatch for run %q: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: authorize dispatch for run %q: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("store: run %q is not pending, is finished, or its work is paused", runID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit dispatch authorization for run %q: %w", runID, err)
	}
	return nil
}

// MarkAcknowledged records that a correlatable acknowledgement was observed. It does not say
// the run is running, finished or cancelled.
func (d *DB) MarkAcknowledged(ctx context.Context, runID string) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE runs SET dispatch = ?
		WHERE id = ? AND dispatch = ? AND outcome IS NULL
		AND EXISTS (
			SELECT 1 FROM native_bindings b
			WHERE b.id = runs.binding_id AND b.native_id IS NOT NULL
		)`, DispatchAcknowledged, runID, DispatchDispatching)
	if err != nil {
		return fmt.Errorf("store: acknowledge dispatch for run %q: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: acknowledge dispatch for run %q: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("store: run %q is not dispatching with a correlated native id", runID)
	}
	return nil
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
