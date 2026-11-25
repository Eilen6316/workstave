package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

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
