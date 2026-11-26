package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Binding is the active native harness binding for a logical session.
type Binding struct {
	ID         int64
	Harness    string
	NativeID   string
	WorkingDir string
}

// ActiveBinding returns the current native binding for a logical session. NativeID is empty
// when the harness id was never recorded; callers that require native resume must reject it.
func (d *DB) ActiveBinding(ctx context.Context, sessionID string) (Binding, error) {
	var b Binding
	var nativeID sql.NullString
	err := d.sql.QueryRowContext(ctx, `SELECT id, harness, native_id, cwd FROM native_bindings WHERE session_id = ? AND active = 1`, sessionID).
		Scan(&b.ID, &b.Harness, &nativeID, &b.WorkingDir)
	if err != nil {
		return Binding{}, fmt.Errorf("store: active binding of session %q: %w", sessionID, err)
	}
	b.NativeID = nativeID.String
	return b, nil
}

// NativeID returns the native id recorded on a binding, or "" if none has been recorded yet.
func (d *DB) NativeID(ctx context.Context, bindingID int64) (string, error) {
	var id sql.NullString
	if err := d.sql.QueryRowContext(ctx, `SELECT native_id FROM native_bindings WHERE id = ?`, bindingID).Scan(&id); err != nil {
		return "", fmt.Errorf("store: native id of binding %d: %w", bindingID, err)
	}
	return id.String, nil
}

// RunForOp returns the run recorded for an operation id; ok is false when there is none.
func (d *DB) RunForOp(ctx context.Context, opID string) (Run, bool, error) {
	r, err := d.runByOp(ctx, opID)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return r, true, nil
}
