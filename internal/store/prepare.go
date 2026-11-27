package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// bindTx deactivates the session's active binding and inserts a new active one inside tx.
// nativeID may be empty when the harness reports its id only after creation.
func bindTx(ctx context.Context, tx *sql.Tx, sessionID, harness, cwd, nativeID string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE native_bindings SET active = 0 WHERE session_id = ? AND active = 1`, sessionID); err != nil {
		return 0, fmt.Errorf("store: deactivate binding of %q: %w", sessionID, err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO native_bindings(session_id, harness, native_id, cwd, active, created_at) VALUES (?,?,NULLIF(?, ''),?,1,?)`, sessionID, harness, nativeID, cwd, nowMS())
	if err != nil {
		return 0, fmt.Errorf("store: insert binding for %q: %w", sessionID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: binding id: %w", err)
	}
	return id, nil
}

// BeginFirstRun makes the binding (with the pre-assigned native id, if any) and the run in one
// transaction, so a crash can never leave a binding without its run or the reverse. It must be
// committed before any external process is started. A repeated operation identity returns the
// existing Run only when its intent hash matches. Main Sessions use BeginMainRun instead.
func (d *DB) BeginFirstRun(ctx context.Context, runID, sessionID, harness, cwd, nativeID, opID, intentHash string) (int64, Run, bool, error) {
	if runID == "" || sessionID == "" || harness == "" || opID == "" || intentHash == "" {
		return 0, Run{}, false, fmt.Errorf("store: run, session, harness, operation identity and intent are required")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, Run{}, false, fmt.Errorf("store: begin first run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing Run
	err = tx.QueryRowContext(ctx, `SELECT id, session_id, binding_id, op_id, intent_hash, dispatch, started_at, ended_at, outcome
		FROM runs WHERE op_id = ?`, opID).
		Scan(&existing.ID, &existing.SessionID, &existing.BindingID, &existing.OpID, &existing.IntentHash, &existing.Dispatch,
			&existing.StartedAt, &existing.EndedAt, &existing.Outcome)
	switch {
	case err == nil:
		if existing.SessionID != sessionID || existing.IntentHash != intentHash {
			return 0, Run{}, false, fmt.Errorf("store: op %q already belongs to run %q for a different session or intent", opID, existing.ID)
		}
		return existing.BindingID.Int64, existing, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, Run{}, false, fmt.Errorf("store: look up op %q: %w", opID, err)
	}
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM sessions WHERE id = ?`, sessionID).Scan(&role); err != nil {
		return 0, Run{}, false, fmt.Errorf("store: session %q: %w", sessionID, err)
	}
	if role != "worker" {
		return 0, Run{}, false, fmt.Errorf("store: direct Run creation is not allowed for Main session %q", sessionID)
	}
	if err := ensureNoUnresolvedRunTx(ctx, tx, sessionID, ""); err != nil {
		return 0, Run{}, false, err
	}

	bindingID, err := bindTx(ctx, tx, sessionID, harness, cwd, nativeID)
	if err != nil {
		return 0, Run{}, false, err
	}
	started := nowMS()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id, session_id, binding_id, op_id, started_at, intent_hash)
		VALUES (?,?,?,?,?,?)`, runID, sessionID, bindingID, opID, started, intentHash); err != nil {
		return 0, Run{}, false, fmt.Errorf("store: insert run %q: %w", runID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, Run{}, false, fmt.Errorf("store: commit first run: %w", err)
	}
	return bindingID, Run{ID: runID, SessionID: sessionID, BindingID: sql.NullInt64{Int64: bindingID, Valid: true}, OpID: opID, IntentHash: intentHash, Dispatch: DispatchPending, StartedAt: started}, true, nil
}
