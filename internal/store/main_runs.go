package store

import (
	"context"
	"fmt"

	"workstave/internal/outcome"
)

// MainRunStart contains the persisted causal input for one Main decision Run.
type MainRunStart struct {
	RunID           string
	OpID            string
	WorkID          string
	MainSessionID   string
	ConversationID  string
	MailboxSequence int64
	SourceMessageID string
	Harness         string
	WorkingDir      string
	NativeID        string
	BindingID       int64 // zero creates a new active Native Binding
	InputSnapshot   string
	ContextRevision int64
}

// BeginMainRun atomically records the binding if needed, the Run, its decision input, and
// dispatch authorization. The Work pause check and dispatching update share this transaction.
func (d *DB) BeginMainRun(ctx context.Context, p MainRunStart) (int64, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin Main Run %q: %w", p.RunID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var mainSessionID string
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT main_session_id, controller_epoch FROM works WHERE id = ?`, p.WorkID).
		Scan(&mainSessionID, &epoch); err != nil {
		return 0, fmt.Errorf("store: Main session for Work %q: %w", p.WorkID, err)
	}
	if mainSessionID != p.MainSessionID {
		return 0, fmt.Errorf("store: session %q is not the Main session for Work %q", p.MainSessionID, p.WorkID)
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main_runs mr JOIN runs r ON r.id = mr.run_id
		WHERE mr.work_id = ? AND (r.outcome IS NULL OR r.outcome = 'unknown')`, p.WorkID).Scan(&pending); err != nil {
		return 0, fmt.Errorf("store: inspect unresolved Main Runs for Work %q: %w", p.WorkID, err)
	}
	if pending != 0 {
		return 0, fmt.Errorf("store: Work %q has an unresolved Main Run", p.WorkID)
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM mailbox WHERE sequence = ? AND work_id = ? AND conversation_id = ? AND message_id = ?`,
		p.MailboxSequence, p.WorkID, p.ConversationID, p.SourceMessageID).Scan(&state); err != nil {
		return 0, fmt.Errorf("store: Mailbox source for Main Run %q: %w", p.RunID, err)
	}
	if state != "processing" {
		return 0, fmt.Errorf("store: Mailbox source for Main Run %q is %s, not processing", p.RunID, state)
	}
	// This write takes SQLite's writer lock before the pause check so Pause and dispatch
	// authorization have one commit order even when another process uses a control connection.
	res, err := tx.ExecContext(ctx, `UPDATE work_pause SET paused = 0 WHERE work_id = ? AND paused = 0`, p.WorkID)
	if err != nil {
		return 0, fmt.Errorf("store: authorize Main dispatch for Work %q: %w", p.WorkID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: authorize Main dispatch for Work %q: %w", p.WorkID, err)
	}
	if n != 1 {
		return 0, fmt.Errorf("store: Work %q is paused or has no pause state", p.WorkID)
	}
	var bindingID int64
	if p.BindingID != 0 {
		bindingID = p.BindingID
		var harness, nativeID string
		if err := tx.QueryRowContext(ctx, `SELECT harness, COALESCE(native_id,''), cwd FROM native_bindings
			WHERE id = ? AND session_id = ? AND active = 1`, bindingID, p.MainSessionID).
			Scan(&harness, &nativeID, new(string)); err != nil {
			return 0, fmt.Errorf("store: active Main binding %d: %w", bindingID, err)
		}
		if harness != p.Harness || nativeID == "" || nativeID != p.NativeID {
			return 0, fmt.Errorf("store: active Main binding %d does not match the requested harness identity", bindingID)
		}
	} else {
		bindingID, err = bindTx(ctx, tx, p.MainSessionID, p.Harness, p.WorkingDir, p.NativeID)
		if err != nil {
			return 0, err
		}
	}
	started := nowMS()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id, session_id, binding_id, op_id, started_at, dispatch)
		VALUES (?,?,?,?,?,?)`, p.RunID, p.MainSessionID, bindingID, p.OpID, started, DispatchDispatching); err != nil {
		return 0, fmt.Errorf("store: insert Main Run %q: %w", p.RunID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO main_runs(run_id, work_id, conversation_id, mailbox_sequence,
		source_message_id, input_snapshot, context_revision, controller_epoch, started_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, p.RunID, p.WorkID, p.ConversationID, p.MailboxSequence,
		p.SourceMessageID, p.InputSnapshot, p.ContextRevision, epoch, started); err != nil {
		return 0, fmt.Errorf("store: record Main Run input %q: %w", p.RunID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit Main Run %q: %w", p.RunID, err)
	}
	return bindingID, nil
}

// CompleteMainRun atomically records the terminal Run outcome, correlated reply, Main Run
// completion, and Mailbox processing state.
func (d *DB) CompleteMainRun(ctx context.Context, runID, replyID, reply string, result outcome.Outcome) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin Main Run completion %q: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var workID, conversationID, sourceMessageID string
	var sequence int64
	err = tx.QueryRowContext(ctx, `SELECT work_id, conversation_id, mailbox_sequence, source_message_id
		FROM main_runs WHERE run_id = ?`, runID).Scan(&workID, &conversationID, &sequence, &sourceMessageID)
	if err != nil {
		return fmt.Errorf("store: Main Run record %q: %w", runID, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE runs SET outcome = ?, ended_at = ? WHERE id = ? AND outcome IS NULL`, string(result), nowMS(), runID)
	if err != nil {
		return fmt.Errorf("store: finish Main Run %q: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: finish Main Run %q: %w", runID, err)
	}
	if n != 1 {
		return fmt.Errorf("store: Main Run %q is already finished or missing", runID)
	}
	outputID := ""
	if result != outcome.Unknown && reply != "" {
		if replyID == "" {
			return fmt.Errorf("store: reply identity is required for Main Run %q", runID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id, work_id, conversation_id, role, body, reply_to, created_at)
			VALUES (?,?,?,'assistant',?,?,?)`, replyID, workID, conversationID, reply, sourceMessageID, nowMS()); err != nil {
			return fmt.Errorf("store: persist Main reply for Run %q: %w", runID, err)
		}
		outputID = replyID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE main_runs SET output_message_id = ?, ended_at = ? WHERE run_id = ?`, nullableID(outputID), nowMS(), runID); err != nil {
		return fmt.Errorf("store: complete Main Run record %q: %w", runID, err)
	}
	state := "completed"
	if result == outcome.Unknown {
		state = "unknown"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox SET state = ?, finished_at = ? WHERE sequence = ? AND state = 'processing'`, state, nowMS(), sequence); err != nil {
		return fmt.Errorf("store: complete Mailbox sequence %d: %w", sequence, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit Main Run completion %q: %w", runID, err)
	}
	return nil
}

func nullableID(value string) any {
	if value == "" {
		return nil
	}
	return value
}
