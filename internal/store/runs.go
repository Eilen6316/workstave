package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"workstave/internal/outcome"
)

// Run is one stored execution attempt. Outcome is NULL until a terminal state was recorded.
type Run struct {
	ID        string
	SessionID string
	BindingID sql.NullInt64
	OpID      string
	StartedAt int64
	EndedAt   sql.NullInt64
	Outcome   sql.NullString
}

func nowMS() int64 { return time.Now().UnixMilli() }

// CreateSession records a logical session.
func (d *DB) CreateSession(ctx context.Context, id, workID, role string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO sessions(id, work_id, role, created_at) VALUES (?,?,?,?)`, id, workID, role, nowMS())
	if err != nil {
		return fmt.Errorf("store: create session %q: %w", id, err)
	}
	return nil
}

// BindNative makes a new active binding for the session and deactivates the previous one in
// the same transaction, so a session never has two active bindings. The native id is unknown
// at this point (Codex returns it only after creation); record it with SetNativeID.
func (d *DB) BindNative(ctx context.Context, sessionID, harness, cwd string) (int64, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: bind native: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := bindTx(ctx, tx, sessionID, harness, cwd, "")
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit binding: %w", err)
	}
	return id, nil
}

// SetNativeID records the native id once. It refuses to overwrite an id that is already set.
func (d *DB) SetNativeID(ctx context.Context, bindingID int64, nativeID, nativePath string) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE native_bindings SET native_id = ?, native_path = NULLIF(?, '') WHERE id = ? AND native_id IS NULL`, nativeID, nativePath, bindingID)
	if err != nil {
		return fmt.Errorf("store: set native id on binding %d: %w", bindingID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set native id on binding %d: %w", bindingID, err)
	}
	if n == 0 {
		return fmt.Errorf("store: binding %d not found or its native id is already set", bindingID)
	}
	return nil
}

// BeginRun records a run before any external call is made. The op_id is the stable operation
// identity: repeating a BeginRun with the same op_id returns the existing run (created=false)
// instead of a new one. The same op_id used for a different session or binding is rejected.
// Comparing the business intent behind the op_id is the caller's job.
func (d *DB) BeginRun(ctx context.Context, runID, sessionID string, bindingID int64, opID string) (Run, bool, error) {
	bind := sql.NullInt64{Int64: bindingID, Valid: bindingID != 0}
	res, err := d.sql.ExecContext(ctx, `INSERT INTO runs(id, session_id, binding_id, op_id, started_at) VALUES (?,?,?,?,?) ON CONFLICT(op_id) DO NOTHING`, runID, sessionID, bind, opID, nowMS())
	if err != nil {
		return Run{}, false, fmt.Errorf("store: begin run %q: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Run{}, false, fmt.Errorf("store: begin run %q: %w", runID, err)
	}
	r, err := d.runByOp(ctx, opID)
	if err != nil {
		return Run{}, false, err
	}
	if n == 0 && (r.SessionID != sessionID || r.BindingID != bind) {
		return Run{}, false, fmt.Errorf("store: op %q already belongs to run %q for a different session or binding", opID, r.ID)
	}
	return r, n == 1, nil
}

// FinishRun records the terminal outcome once. A run that already has an outcome is not changed.
func (d *DB) FinishRun(ctx context.Context, runID string, o outcome.Outcome) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE runs SET outcome = ?, ended_at = ? WHERE id = ? AND outcome IS NULL`, string(o), nowMS(), runID)
	if err != nil {
		return fmt.Errorf("store: finish run %q: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: finish run %q: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("store: run %q not found or already finished", runID)
	}
	return nil
}

// UnfinishedRuns lists runs that have no recorded outcome, oldest first.
func (d *DB) UnfinishedRuns(ctx context.Context) ([]Run, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, session_id, binding_id, op_id, started_at, ended_at, outcome FROM runs WHERE outcome IS NULL ORDER BY started_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list unfinished runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.SessionID, &r.BindingID, &r.OpID, &r.StartedAt, &r.EndedAt, &r.Outcome); err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list unfinished runs: %w", err)
	}
	return out, nil
}

// MarkUnfinishedUnknown records outcome unknown for every run without an outcome and returns
// how many were changed. Call it once at daemon start, before any new run begins: a run that
// was in flight when the previous daemon stopped has an unobserved end and must be reconciled,
// never treated as success, failure, or safe to retry.
func (d *DB) MarkUnfinishedUnknown(ctx context.Context) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `UPDATE runs SET outcome = 'unknown', ended_at = ? WHERE outcome IS NULL`, nowMS())
	if err != nil {
		return 0, fmt.Errorf("store: mark unfinished runs unknown: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: mark unfinished runs unknown: %w", err)
	}
	return n, nil
}

func (d *DB) runByOp(ctx context.Context, opID string) (Run, error) {
	var r Run
	err := d.sql.QueryRowContext(ctx, `SELECT id, session_id, binding_id, op_id, started_at, ended_at, outcome FROM runs WHERE op_id = ?`, opID).
		Scan(&r.ID, &r.SessionID, &r.BindingID, &r.OpID, &r.StartedAt, &r.EndedAt, &r.Outcome)
	if err != nil {
		return Run{}, fmt.Errorf("store: run for op %q: %w", opID, err)
	}
	return r, nil
}
