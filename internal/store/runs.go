package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"workstave/internal/outcome"
)

// Run is one stored execution attempt. Outcome is NULL until a terminal state was recorded.
type Run struct {
	ID         string
	SessionID  string
	BindingID  sql.NullInt64
	OpID       string
	IntentHash string
	Dispatch   string
	StartedAt  int64
	EndedAt    sql.NullInt64
	Outcome    sql.NullString
}

func nowMS() int64 { return time.Now().UnixMilli() }

// CreateSession records a logical session.
func (d *DB) CreateSession(ctx context.Context, id, workID, role string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin session %q: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_pause(work_id) VALUES (?)`, workID); err != nil {
		return fmt.Errorf("store: initialize pause state for work %q: %w", workID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, work_id, role, created_at) VALUES (?,?,?,?)`, id, workID, role, nowMS()); err != nil {
		return fmt.Errorf("store: create session %q: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit session %q: %w", id, err)
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

func ensureNoUnresolvedRunTx(ctx context.Context, tx *sql.Tx, sessionID, excludeRunID string) error {
	var runID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE session_id = ? AND id != ?
		AND (outcome IS NULL OR outcome = 'unknown') ORDER BY started_at, id LIMIT 1`, sessionID, excludeRunID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: inspect unresolved Runs for Session %q: %w", sessionID, err)
	}
	return fmt.Errorf("store: Session %q has unresolved Run %q", sessionID, runID)
}

// BeginRun persists a new attempt for an active Worker binding. The operation identity may be
// reused only for the same intent, and an unresolved run blocks only this logical Session.
func (d *DB) BeginRun(ctx context.Context, runID, sessionID string, bindingID int64, opID, intentHash string) (Run, bool, error) {
	if runID == "" || sessionID == "" || opID == "" || intentHash == "" || bindingID == 0 {
		return Run{}, false, fmt.Errorf("store: run, session, binding, operation identity and intent are required")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, false, fmt.Errorf("store: begin run %q: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()
	bind := sql.NullInt64{Int64: bindingID, Valid: bindingID != 0}
	var existing Run
	err = tx.QueryRowContext(ctx, `SELECT id, session_id, binding_id, op_id, intent_hash, dispatch, started_at, ended_at, outcome
		FROM runs WHERE op_id = ?`, opID).
		Scan(&existing.ID, &existing.SessionID, &existing.BindingID, &existing.OpID, &existing.IntentHash, &existing.Dispatch,
			&existing.StartedAt, &existing.EndedAt, &existing.Outcome)
	if err == nil {
		if existing.SessionID != sessionID || !existing.BindingID.Valid || existing.BindingID.Int64 != bindingID || existing.IntentHash != intentHash {
			return Run{}, false, fmt.Errorf("store: op %q already belongs to a different session, binding or intent", opID)
		}
		if err := tx.Commit(); err != nil {
			return Run{}, false, fmt.Errorf("store: commit repeated run lookup %q: %w", opID, err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, fmt.Errorf("store: look up op %q: %w", opID, err)
	}
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM sessions WHERE id = ?`, sessionID).Scan(&role); err != nil {
		return Run{}, false, fmt.Errorf("store: session %q: %w", sessionID, err)
	}
	if role != "worker" {
		return Run{}, false, fmt.Errorf("store: direct Run creation is not allowed for Main session %q", sessionID)
	}
	if err := ensureNoUnresolvedRunTx(ctx, tx, sessionID, ""); err != nil {
		return Run{}, false, err
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_bindings WHERE id = ? AND session_id = ? AND active = 1)`, bindingID, sessionID).Scan(&active); err != nil {
		return Run{}, false, fmt.Errorf("store: inspect active binding %d: %w", bindingID, err)
	}
	if active == 0 {
		return Run{}, false, fmt.Errorf("store: binding %d is not active for session %q", bindingID, sessionID)
	}
	started := nowMS()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id, session_id, binding_id, op_id, started_at, intent_hash)
		VALUES (?,?,?,?,?,?)`, runID, sessionID, bind, opID, started, intentHash); err != nil {
		return Run{}, false, fmt.Errorf("store: insert run %q: %w", runID, err)
	}
	if err := tx.Commit(); err != nil {
		return Run{}, false, fmt.Errorf("store: commit run %q: %w", runID, err)
	}
	return Run{ID: runID, SessionID: sessionID, BindingID: bind, OpID: opID, IntentHash: intentHash, Dispatch: DispatchPending, StartedAt: started}, true, nil
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
	rows, err := d.sql.QueryContext(ctx, `SELECT id, session_id, binding_id, op_id, intent_hash, dispatch, started_at, ended_at, outcome
		FROM runs WHERE outcome IS NULL ORDER BY started_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list unfinished runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.SessionID, &r.BindingID, &r.OpID, &r.IntentHash, &r.Dispatch, &r.StartedAt, &r.EndedAt, &r.Outcome); err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list unfinished runs: %w", err)
	}
	return out, nil
}

func (d *DB) runByOp(ctx context.Context, opID string) (Run, error) {
	var r Run
	err := d.sql.QueryRowContext(ctx, `SELECT id, session_id, binding_id, op_id, intent_hash, dispatch, started_at, ended_at, outcome FROM runs WHERE op_id = ?`, opID).
		Scan(&r.ID, &r.SessionID, &r.BindingID, &r.OpID, &r.IntentHash, &r.Dispatch, &r.StartedAt, &r.EndedAt, &r.Outcome)
	if err != nil {
		return Run{}, fmt.Errorf("store: run for op %q: %w", opID, err)
	}
	return r, nil
}
