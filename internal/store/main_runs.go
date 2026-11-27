package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"workstave/internal/outcome"
)

// WorkChange is a Main proposal tied to the source Message and fact version in its input snapshot.
type WorkChange struct {
	Kind             string
	Key              string
	Value            string
	Status           string
	Basis            string
	Evidence         string
	ExpectedRevision int64
}

// MainCompletion records a parsed user reply and the result of processing the Mailbox input.
type MainCompletion struct {
	ReplyID      string
	Reply        string
	Changes      []WorkChange
	Result       string
	ResultDetail string
}

// MainRunStart contains the persisted causal input for one Main decision Run.
type MainRunStart struct {
	RunID                   string
	OpID                    string
	WorkID                  string
	MainSessionID           string
	ConversationID          string
	MailboxSequence         int64
	SourceMessageID         string
	Harness                 string
	WorkingDir              string
	NativeID                string
	BindingID               int64 // zero creates a new active Native Binding
	AllowBindingReplacement bool
	IntentHash              string
	InputSnapshot           string
	ContextRevision         int64
}

// BeginMainRun atomically records the binding if needed, the Run, its decision input, and
// dispatch authorization. The Work pause check and dispatching update share this transaction.
func (d *DB) BeginMainRun(ctx context.Context, p MainRunStart) (int64, error) {
	if p.RunID == "" || p.OpID == "" || p.WorkID == "" || p.MainSessionID == "" || p.SourceMessageID == "" || p.IntentHash == "" {
		return 0, fmt.Errorf("store: Main Run identity, source and intent are required")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin Main Run %q: %w", p.RunID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var mainSessionID, mainHarness string
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT main_session_id, main_harness, controller_epoch FROM works WHERE id = ?`, p.WorkID).
		Scan(&mainSessionID, &mainHarness, &epoch); err != nil {
		return 0, fmt.Errorf("store: Main session for Work %q: %w", p.WorkID, err)
	}
	if mainSessionID != p.MainSessionID {
		return 0, fmt.Errorf("store: session %q is not the Main session for Work %q", p.MainSessionID, p.WorkID)
	}
	if !p.AllowBindingReplacement && p.Harness != mainHarness {
		return 0, fmt.Errorf("store: Main Work %q uses Harness %q, not %q", p.WorkID, mainHarness, p.Harness)
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
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_bindings WHERE session_id = ? AND active = 1)`, p.MainSessionID).Scan(&active); err != nil {
			return 0, fmt.Errorf("store: inspect active Main binding for session %q: %w", p.MainSessionID, err)
		}
		if active != 0 && !p.AllowBindingReplacement {
			return 0, fmt.Errorf("store: replacing the active Main binding requires an explicit experiment")
		}
		bindingID, err = bindTx(ctx, tx, p.MainSessionID, p.Harness, p.WorkingDir, p.NativeID)
		if err != nil {
			return 0, err
		}
		if p.AllowBindingReplacement {
			if _, err := tx.ExecContext(ctx, `UPDATE works SET main_harness = ? WHERE id = ?`, p.Harness, p.WorkID); err != nil {
				return 0, fmt.Errorf("store: update Main Harness for Work %q: %w", p.WorkID, err)
			}
		}
	}
	started := nowMS()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id, session_id, binding_id, op_id, started_at, dispatch, intent_hash)
		VALUES (?,?,?,?,?,?,?)`, p.RunID, p.MainSessionID, bindingID, p.OpID, started, DispatchPending, p.IntentHash); err != nil {
		return 0, fmt.Errorf("store: insert Main Run %q: %w", p.RunID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO main_runs(run_id, work_id, conversation_id, mailbox_sequence,
		source_message_id, input_snapshot, context_revision, controller_epoch, started_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, p.RunID, p.WorkID, p.ConversationID, p.MailboxSequence,
		p.SourceMessageID, p.InputSnapshot, p.ContextRevision, epoch, started); err != nil {
		return 0, fmt.Errorf("store: record Main Run input %q: %w", p.RunID, err)
	}
	if err := authorizeDispatchTx(ctx, tx, p.RunID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit Main Run %q: %w", p.RunID, err)
	}
	return bindingID, nil
}

// CompleteMainRun atomically records the terminal Run outcome, validated Work changes, reply,
// Main Run completion and Mailbox processing conclusion.
func (d *DB) CompleteMainRun(ctx context.Context, runID string, completion MainCompletion, runOutcome outcome.Outcome) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin Main Run completion %q: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var workID, conversationID, sourceMessageID string
	var sequence, controllerEpoch int64
	err = tx.QueryRowContext(ctx, `SELECT work_id, conversation_id, mailbox_sequence, source_message_id, controller_epoch
		FROM main_runs WHERE run_id = ?`, runID).Scan(&workID, &conversationID, &sequence, &sourceMessageID, &controllerEpoch)
	if err != nil {
		return fmt.Errorf("store: Main Run record %q: %w", runID, err)
	}
	processingResult := completion.Result
	resultDetail := completion.ResultDetail
	if processingResult == "" {
		processingResult = "completed"
	}
	if runOutcome == outcome.Unknown {
		processingResult = "unknown"
		resultDetail = "terminal_outcome_unobserved"
	} else if runOutcome == outcome.Failed {
		processingResult = "failed"
		if resultDetail == "" {
			resultDetail = "harness_run_failed"
		}
	} else if runOutcome != outcome.Completed {
		return fmt.Errorf("store: invalid Main Run outcome %q", runOutcome)
	}
	if processingResult != "completed" && processingResult != "failed" && processingResult != "unknown" {
		processingResult = "failed"
		resultDetail = "invalid_processing_result"
	}
	if processingResult == "failed" && resultDetail == "" {
		resultDetail = "main_response_failed"
	}
	if runOutcome == outcome.Completed && processingResult == "completed" {
		var currentEpoch int64
		if err := tx.QueryRowContext(ctx, `SELECT controller_epoch FROM works WHERE id = ?`, workID).Scan(&currentEpoch); err != nil {
			return fmt.Errorf("store: read Work controller epoch %q: %w", workID, err)
		}
		if currentEpoch != controllerEpoch {
			processingResult = "failed"
			resultDetail = "stale_controller_epoch"
		} else if detail, err := applyMainWorkChangesTx(ctx, tx, workID, sourceMessageID, completion.Changes); err != nil {
			return err
		} else if detail != "" {
			processingResult = "failed"
			resultDetail = detail
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE runs SET outcome = ?, ended_at = ? WHERE id = ? AND outcome IS NULL`, string(runOutcome), nowMS(), runID)
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
	if runOutcome != outcome.Unknown && completion.Reply != "" {
		if completion.ReplyID == "" {
			return fmt.Errorf("store: reply identity is required for Main Run %q", runID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id, work_id, conversation_id, role, body, reply_to, created_at)
			VALUES (?,?,?,'assistant',?,?,?)`, completion.ReplyID, workID, conversationID, completion.Reply, sourceMessageID, nowMS()); err != nil {
			return fmt.Errorf("store: persist Main reply for Run %q: %w", runID, err)
		}
		outputID = completion.ReplyID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE main_runs SET output_message_id = ?, ended_at = ? WHERE run_id = ?`, nullableID(outputID), nowMS(), runID); err != nil {
		return fmt.Errorf("store: complete Main Run record %q: %w", runID, err)
	}
	state := "completed"
	if runOutcome == outcome.Unknown {
		state = "unknown"
	}
	mailboxUpdate, err := tx.ExecContext(ctx, `UPDATE mailbox SET state = ?, result = ?, result_detail = NULLIF(?, ''), finished_at = ?
		WHERE sequence = ? AND state = 'processing'`, state, processingResult, resultDetail, nowMS(), sequence)
	if err != nil {
		return fmt.Errorf("store: complete Mailbox sequence %d: %w", sequence, err)
	}
	if affected, err := mailboxUpdate.RowsAffected(); err != nil {
		return fmt.Errorf("store: inspect completed Mailbox sequence %d: %w", sequence, err)
	} else if affected != 1 {
		return fmt.Errorf("store: Mailbox sequence %d is no longer owned by this Main Run", sequence)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit Main Run completion %q: %w", runID, err)
	}
	return nil
}

func applyMainWorkChangesTx(ctx context.Context, tx *sql.Tx, workID, sourceMessageID string, changes []WorkChange) (string, error) {
	if len(changes) == 0 {
		return "", nil
	}
	var sourceWorkID, sourceRole, sourceBody string
	if err := tx.QueryRowContext(ctx, `SELECT work_id, role, body FROM messages WHERE id = ?`, sourceMessageID).Scan(&sourceWorkID, &sourceRole, &sourceBody); err != nil {
		return "", fmt.Errorf("store: validate Work change source %q: %w", sourceMessageID, err)
	}
	if sourceWorkID != workID || sourceRole != "user" {
		return "invalid_change_source", nil
	}
	applied := 0
	seen := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		if (change.Kind != "requirement" && change.Kind != "decision") || change.Key == "" || strings.TrimSpace(change.Value) == "" || change.ExpectedRevision < 0 {
			return "invalid_work_change", nil
		}
		if change.Status != "confirmed" && change.Status != "candidate" {
			return "invalid_work_change", nil
		}
		if change.Basis != "explicit_user_statement" && change.Basis != "discussion" && change.Basis != "model_inference" {
			return "invalid_work_change_authority", nil
		}
		if change.Status == "confirmed" && change.Basis != "explicit_user_statement" {
			return "invalid_work_change_authority", nil
		}
		if change.Basis != "model_inference" {
			evidence := strings.TrimSpace(change.Evidence)
			if evidence == "" || !strings.Contains(sourceBody, evidence) {
				return "invalid_work_change_evidence", nil
			}
		}
		key := change.Kind + "\x00" + change.Key
		if _, exists := seen[key]; exists {
			return "duplicate_work_change", nil
		}
		seen[key] = struct{}{}
		var existingValue, existingStatus, existingBasis string
		err := tx.QueryRowContext(ctx, `SELECT value, status, confirmation_basis
			FROM work_facts WHERE work_id = ? AND kind = ? AND fact_key = ? AND source_message_id = ?`,
			workID, change.Kind, change.Key, sourceMessageID).Scan(&existingValue, &existingStatus, &existingBasis)
		if err == nil {
			if existingValue != strings.TrimSpace(change.Value) || existingStatus != change.Status || existingBasis != change.Basis {
				return "operation_intent_conflict", nil
			}
			applied++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("store: inspect prior Work change %s/%s: %w", change.Kind, change.Key, err)
		}
		var current sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(revision) FROM work_facts WHERE work_id = ? AND kind = ? AND fact_key = ?`, workID, change.Kind, change.Key).Scan(&current); err != nil {
			return "", fmt.Errorf("store: read current Work fact version %s/%s: %w", change.Kind, change.Key, err)
		}
		currentRevision := int64(0)
		if current.Valid {
			currentRevision = current.Int64
		}
		if currentRevision != change.ExpectedRevision {
			return "stale_work_state", nil
		}
	}
	if applied != 0 {
		if applied == len(changes) {
			return "", nil
		}
		return "partial_operation_replay", nil
	}
	for _, change := range changes {
		if change.Status == "confirmed" {
			if _, err := tx.ExecContext(ctx, `UPDATE work_facts SET status = 'superseded'
				WHERE work_id = ? AND kind = ? AND fact_key = ? AND status IN ('confirmed','candidate')`, workID, change.Kind, change.Key); err != nil {
				return "", fmt.Errorf("store: supersede Work fact %s/%s: %w", change.Kind, change.Key, err)
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE work_facts SET status = 'superseded'
			WHERE work_id = ? AND kind = ? AND fact_key = ? AND status = 'candidate'`, workID, change.Kind, change.Key); err != nil {
			return "", fmt.Errorf("store: supersede Work candidate %s/%s: %w", change.Kind, change.Key, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE works SET revision = revision + 1 WHERE id = ?`, workID); err != nil {
			return "", fmt.Errorf("store: advance Work revision %q: %w", workID, err)
		}
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM works WHERE id = ?`, workID).Scan(&revision); err != nil {
			return "", fmt.Errorf("store: read Work revision %q: %w", workID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO work_facts(work_id, kind, fact_key, revision, value, status,
			source_message_id, confirmation_basis, created_at) VALUES (?,?,?,?,?,?,?,?,?)`, workID, change.Kind,
			change.Key, revision, strings.TrimSpace(change.Value), change.Status, sourceMessageID, change.Basis, nowMS()); err != nil {
			return "", fmt.Errorf("store: persist Main Work change %s/%s: %w", change.Kind, change.Key, err)
		}
	}
	return "", nil
}

// ApplyUserWorkChanges applies a direct user-authorized Work correction using the same source
// and per-fact version checks as a Main proposal.
func (d *DB) ApplyUserWorkChanges(ctx context.Context, workID, sourceMessageID string, changes []WorkChange) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin user Work correction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	detail, err := applyMainWorkChangesTx(ctx, tx, workID, sourceMessageID, changes)
	if err != nil {
		return err
	}
	if detail != "" {
		return fmt.Errorf("store: reject user Work correction: %s", detail)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit user Work correction: %w", err)
	}
	return nil
}

func nullableID(value string) any {
	if value == "" {
		return nil
	}
	return value
}
