package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// MailboxItem is one accepted user input in the Work-wide processing order.
type MailboxItem struct {
	Sequence               int64
	WorkID                 string
	ConversationID         string
	MessageID              string
	OpID                   string
	Harness                string
	CrossHarnessExperiment bool
	State                  string
	Result                 string
	ResultDetail           string
	Body                   string
}

// AcceptUserMessage records a normal message on the Work's current Main Harness.
func (d *DB) AcceptUserMessage(ctx context.Context, messageID, conversationID, body string, proposals []FactProposal) (MailboxItem, error) {
	return d.acceptUserMessage(ctx, messageID, conversationID, "", body, proposals)
}

// AcceptHarnessExperiment records a message that explicitly requests a different Main Harness.
func (d *DB) AcceptHarnessExperiment(ctx context.Context, messageID, conversationID, harness, body string) (MailboxItem, error) {
	return d.acceptUserMessage(ctx, messageID, conversationID, harness, body, nil)
}

func (d *DB) acceptUserMessage(ctx context.Context, messageID, conversationID, requestedHarness, body string, proposals []FactProposal) (MailboxItem, error) {
	body = strings.TrimSpace(body)
	if messageID == "" || conversationID == "" || body == "" {
		return MailboxItem{}, fmt.Errorf("store: message id, Conversation id and body are required")
	}
	if requestedHarness != "" && requestedHarness != "codex" && requestedHarness != "claude-code" {
		return MailboxItem{}, fmt.Errorf("store: unsupported Main harness %q", requestedHarness)
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return MailboxItem{}, fmt.Errorf("store: begin message acceptance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var workID, currentHarness string
	if err := tx.QueryRowContext(ctx, `SELECT c.work_id,
		COALESCE((SELECT b.harness FROM native_bindings b WHERE b.session_id = w.main_session_id AND b.active = 1), w.main_harness)
		FROM conversations c JOIN works w ON w.id = c.work_id WHERE c.id = ?`, conversationID).
		Scan(&workID, &currentHarness); err != nil {
		return MailboxItem{}, fmt.Errorf("store: resolve Conversation %q: %w", conversationID, err)
	}
	harness := currentHarness
	experiment := false
	if requestedHarness != "" {
		harness = requestedHarness
		experiment = true
	}
	var existing MailboxItem
	err = tx.QueryRowContext(ctx, `SELECT m.id, m.work_id, m.conversation_id, m.body,
		b.sequence, b.op_id, b.harness, b.harness_experiment, b.state, COALESCE(b.result,''), COALESCE(b.result_detail,'')
		FROM messages m JOIN mailbox b ON b.message_id = m.id WHERE m.id = ?`, messageID).
		Scan(&existing.MessageID, &existing.WorkID, &existing.ConversationID, &existing.Body,
			&existing.Sequence, &existing.OpID, &existing.Harness, &existing.CrossHarnessExperiment, &existing.State, &existing.Result, &existing.ResultDetail)
	if err == nil {
		intentChanged := existing.WorkID != workID || existing.ConversationID != conversationID || existing.Body != body
		if requestedHarness != "" && (existing.Harness != requestedHarness || !existing.CrossHarnessExperiment) {
			intentChanged = true
		}
		if requestedHarness == "" && existing.CrossHarnessExperiment {
			intentChanged = true
		}
		var storedProposals []FactProposal
		rows, err := tx.QueryContext(ctx, `SELECT kind, fact_key, value, status FROM message_fact_proposals WHERE message_id = ?`, messageID)
		if err != nil {
			return MailboxItem{}, fmt.Errorf("store: read accepted proposals for message %q: %w", messageID, err)
		}
		for rows.Next() {
			var proposal FactProposal
			if err := rows.Scan(&proposal.Kind, &proposal.Key, &proposal.Value, &proposal.Status); err != nil {
				_ = rows.Close()
				return MailboxItem{}, fmt.Errorf("store: scan accepted proposal for message %q: %w", messageID, err)
			}
			storedProposals = append(storedProposals, proposal)
		}
		if err := rows.Close(); err != nil {
			return MailboxItem{}, fmt.Errorf("store: close accepted proposals for message %q: %w", messageID, err)
		}
		if err := rows.Err(); err != nil {
			return MailboxItem{}, fmt.Errorf("store: read accepted proposals for message %q: %w", messageID, err)
		}
		if len(storedProposals) != len(proposals) {
			intentChanged = true
		} else {
			proposalSet := make(map[FactProposal]struct{}, len(proposals))
			for _, proposal := range proposals {
				proposalSet[proposal] = struct{}{}
			}
			for _, existingProposal := range storedProposals {
				if _, matched := proposalSet[existingProposal]; !matched {
					intentChanged = true
					break
				}
			}
		}
		if intentChanged {
			return MailboxItem{}, fmt.Errorf("store: message id %q was already accepted with different input", messageID)
		}
		if err := tx.Commit(); err != nil {
			return MailboxItem{}, fmt.Errorf("store: commit repeated message lookup: %w", err)
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MailboxItem{}, fmt.Errorf("store: look up message %q: %w", messageID, err)
	}
	var operationBytes [16]byte
	if _, err := rand.Read(operationBytes[:]); err != nil {
		return MailboxItem{}, fmt.Errorf("store: generate operation identity for message %q: %w", messageID, err)
	}
	opID := "op-" + hex.EncodeToString(operationBytes[:])
	accepted := nowMS()
	experimentFlag := 0
	if experiment {
		experimentFlag = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id, work_id, conversation_id, role, body, created_at) VALUES (?,?,?,'user',?,?)`, messageID, workID, conversationID, body, accepted); err != nil {
		return MailboxItem{}, fmt.Errorf("store: persist user message %q: %w", messageID, err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO mailbox(work_id, conversation_id, message_id, op_id, harness, harness_experiment, accepted_at)
		VALUES (?,?,?,?,?,?,?)`, workID, conversationID, messageID, opID, harness, experimentFlag, accepted)
	if err != nil {
		return MailboxItem{}, fmt.Errorf("store: accept user message %q into Mailbox: %w", messageID, err)
	}
	sequence, err := res.LastInsertId()
	if err != nil {
		return MailboxItem{}, fmt.Errorf("store: read Mailbox sequence for message %q: %w", messageID, err)
	}
	for _, proposal := range proposals {
		if proposal.Kind != "requirement" && proposal.Kind != "decision" {
			return MailboxItem{}, fmt.Errorf("store: unsupported fact proposal kind %q", proposal.Kind)
		}
		if proposal.Status != "confirmed" && proposal.Status != "candidate" {
			return MailboxItem{}, fmt.Errorf("store: unsupported fact proposal status %q", proposal.Status)
		}
		if proposal.Key == "" || proposal.Value == "" {
			return MailboxItem{}, fmt.Errorf("store: fact proposal key and value are required")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_fact_proposals(message_id, kind, fact_key, value, status) VALUES (?,?,?,?,?)`, messageID, proposal.Kind, proposal.Key, proposal.Value, proposal.Status); err != nil {
			return MailboxItem{}, fmt.Errorf("store: persist fact proposal for message %q: %w", messageID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return MailboxItem{}, fmt.Errorf("store: commit user message acceptance %q: %w", messageID, err)
	}
	return MailboxItem{Sequence: sequence, WorkID: workID, ConversationID: conversationID, MessageID: messageID, OpID: opID, Harness: harness,
		CrossHarnessExperiment: experiment, State: "pending", Body: body}, nil
}

// ClaimNextMailbox moves the oldest pending input to processing and applies its explicitly
// authorized fact proposals in the same transaction.
func (d *DB) ClaimNextMailbox(ctx context.Context, workID string) (MailboxItem, bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return MailboxItem{}, false, fmt.Errorf("store: begin Mailbox claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var item MailboxItem
	err = tx.QueryRowContext(ctx, `UPDATE mailbox SET state = 'processing', started_at = ?,
		harness = CASE WHEN harness_experiment = 1 THEN harness ELSE COALESCE(
			(SELECT b.harness FROM native_bindings b JOIN works w ON w.main_session_id = b.session_id
				WHERE w.id = mailbox.work_id AND b.active = 1),
			(SELECT main_harness FROM works WHERE id = mailbox.work_id)) END
		WHERE sequence = (
			SELECT next.sequence FROM mailbox next
			WHERE next.work_id = ? AND next.state = 'pending'
			AND NOT EXISTS (SELECT 1 FROM mailbox active WHERE active.work_id = next.work_id AND active.state = 'processing')
			AND NOT EXISTS (SELECT 1 FROM main_runs mr JOIN runs r ON r.id = mr.run_id
				WHERE mr.work_id = next.work_id AND (r.outcome IS NULL OR r.outcome = 'unknown'))
			ORDER BY next.sequence LIMIT 1
		) AND state = 'pending'
		RETURNING sequence, work_id, conversation_id, message_id, op_id, harness, harness_experiment, state`, nowMS(), workID).
		Scan(&item.Sequence, &item.WorkID, &item.ConversationID, &item.MessageID, &item.OpID, &item.Harness, &item.CrossHarnessExperiment, &item.State)
	if errors.Is(err, sql.ErrNoRows) {
		return MailboxItem{}, false, nil
	}
	if err != nil {
		return MailboxItem{}, false, fmt.Errorf("store: claim next Mailbox input for Work %q: %w", workID, err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT body FROM messages WHERE id = ?`, item.MessageID).Scan(&item.Body); err != nil {
		return MailboxItem{}, false, fmt.Errorf("store: read claimed Mailbox message %q: %w", item.MessageID, err)
	}
	if err := applyMessageFacts(ctx, tx, item.WorkID, item.MessageID); err != nil {
		return MailboxItem{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return MailboxItem{}, false, fmt.Errorf("store: commit Mailbox claim %d: %w", item.Sequence, err)
	}
	item.State = "processing"
	return item, true, nil
}

// PendingWorkIDs lists unpaused Works with a safe next input. Unresolved Main Runs keep the
// remaining queue untouched until evidence-based reconciliation.
func (d *DB) PendingWorkIDs(ctx context.Context) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT b.work_id FROM mailbox b
		JOIN work_pause p ON p.work_id = b.work_id
		WHERE b.state = 'pending' AND p.paused = 0
		AND NOT EXISTS (SELECT 1 FROM mailbox active WHERE active.work_id = b.work_id AND active.state = 'processing')
		AND NOT EXISTS (SELECT 1 FROM main_runs mr JOIN runs r ON r.id = mr.run_id
			WHERE mr.work_id = b.work_id AND (r.outcome IS NULL OR r.outcome = 'unknown'))
		ORDER BY b.work_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list Works with pending Mailbox inputs: %w", err)
	}
	defer rows.Close()
	var workIDs []string
	for rows.Next() {
		var workID string
		if err := rows.Scan(&workID); err != nil {
			return nil, fmt.Errorf("store: scan pending Mailbox Work: %w", err)
		}
		workIDs = append(workIDs, workID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read Works with pending Mailbox inputs: %w", err)
	}
	return workIDs, nil
}

func applyMessageFacts(ctx context.Context, tx *sql.Tx, workID, messageID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT kind, fact_key, value, status FROM message_fact_proposals WHERE message_id = ? ORDER BY kind, fact_key`, messageID)
	if err != nil {
		return fmt.Errorf("store: read fact proposals for message %q: %w", messageID, err)
	}
	var proposals []FactProposal
	for rows.Next() {
		var p FactProposal
		if err := rows.Scan(&p.Kind, &p.Key, &p.Value, &p.Status); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: scan fact proposal: %w", err)
		}
		proposals = append(proposals, p)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: close fact proposals: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read fact proposals: %w", err)
	}
	for _, p := range proposals {
		var applied int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_facts
			WHERE work_id = ? AND kind = ? AND fact_key = ? AND source_message_id = ?`, workID, p.Kind, p.Key, messageID).Scan(&applied); err != nil {
			return fmt.Errorf("store: check applied Work fact %s/%s: %w", p.Kind, p.Key, err)
		}
		if applied != 0 {
			continue
		}
		if p.Status == "confirmed" {
			if _, err := tx.ExecContext(ctx, `UPDATE work_facts SET status = 'superseded'
				WHERE work_id = ? AND kind = ? AND fact_key = ? AND status = 'confirmed'`, workID, p.Kind, p.Key); err != nil {
				return fmt.Errorf("store: supersede Work fact %s/%s: %w", p.Kind, p.Key, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE works SET revision = revision + 1 WHERE id = ?`, workID); err != nil {
			return fmt.Errorf("store: advance Work revision %q: %w", workID, err)
		}
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM works WHERE id = ?`, workID).Scan(&revision); err != nil {
			return fmt.Errorf("store: read Work revision %q: %w", workID, err)
		}
		basis := "fixture_candidate"
		if p.Status == "confirmed" {
			basis = "fixture_confirmed"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO work_facts(work_id, kind, fact_key, revision, value, status, source_message_id, confirmation_basis, created_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, workID, p.Kind, p.Key, revision, p.Value, p.Status, messageID, basis, nowMS()); err != nil {
			return fmt.Errorf("store: record Work fact %s/%s: %w", p.Kind, p.Key, err)
		}
	}
	return nil
}

// RecoverMailboxOnStart reconciles Mailbox inputs left processing by a dead owner. Inputs with
// no Main Run are safe to return to pending. Inputs with a persisted Run and no terminal result
// become unknown and are never replayed automatically.
func (d *DB) RecoverMailboxOnStart(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `UPDATE mailbox SET state = 'pending', result = NULL, result_detail = NULL, started_at = NULL
		WHERE state = 'processing' AND NOT EXISTS (SELECT 1 FROM main_runs WHERE main_runs.mailbox_sequence = mailbox.sequence)`); err != nil {
		return fmt.Errorf("store: return unstarted Mailbox inputs to pending: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx, `UPDATE mailbox SET state = 'unknown', result = 'unknown', result_detail = 'interrupted_main_run', finished_at = ?
		WHERE state = 'processing' AND EXISTS (
			SELECT 1 FROM main_runs mr JOIN runs r ON r.id = mr.run_id
			WHERE mr.mailbox_sequence = mailbox.sequence AND (r.outcome IS NULL OR r.outcome = 'unknown')
		)`, nowMS()); err != nil {
		return fmt.Errorf("store: mark interrupted Mailbox inputs unknown: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx, `UPDATE mailbox SET state = 'completed',
		result = (SELECT r.outcome FROM main_runs mr JOIN runs r ON r.id = mr.run_id WHERE mr.mailbox_sequence = mailbox.sequence),
		result_detail = CASE WHEN (SELECT r.outcome FROM main_runs mr JOIN runs r ON r.id = mr.run_id WHERE mr.mailbox_sequence = mailbox.sequence) = 'failed' THEN 'harness_run_failed' ELSE NULL END,
		finished_at = ?
		WHERE state = 'processing' AND EXISTS (
			SELECT 1 FROM main_runs mr JOIN runs r ON r.id = mr.run_id
			WHERE mr.mailbox_sequence = mailbox.sequence AND r.outcome IN ('completed','failed')
		)`, nowMS()); err != nil {
		return fmt.Errorf("store: complete terminal Mailbox inputs: %w", err)
	}
	return nil
}

// MailboxByMessage returns the accepted state for one message.
func (d *DB) MailboxByMessage(ctx context.Context, messageID string) (MailboxItem, error) {
	var item MailboxItem
	err := d.sql.QueryRowContext(ctx, `SELECT b.sequence, b.work_id, b.conversation_id, b.message_id, b.op_id, b.harness, b.harness_experiment, b.state,
		COALESCE(b.result,''), COALESCE(b.result_detail,''), m.body
		FROM mailbox b JOIN messages m ON m.id = b.message_id WHERE b.message_id = ?`, messageID).
		Scan(&item.Sequence, &item.WorkID, &item.ConversationID, &item.MessageID, &item.OpID, &item.Harness, &item.CrossHarnessExperiment, &item.State,
			&item.Result, &item.ResultDetail, &item.Body)
	if err != nil {
		return MailboxItem{}, fmt.Errorf("store: Mailbox message %q: %w", messageID, err)
	}
	return item, nil
}

// ReturnMailboxToPending releases a claimed input only when no Main Run was committed.
func (d *DB) ReturnMailboxToPending(ctx context.Context, sequence int64) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE mailbox SET state = 'pending', started_at = NULL
		WHERE sequence = ? AND state = 'processing' AND NOT EXISTS (SELECT 1 FROM main_runs WHERE mailbox_sequence = ?)`, sequence, sequence)
	if err != nil {
		return fmt.Errorf("store: release Mailbox input %d: %w", sequence, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: release Mailbox input %d: %w", sequence, err)
	}
	if n == 0 {
		return fmt.Errorf("store: Mailbox input %d is no longer releasable", sequence)
	}
	return nil
}

// MessageByID returns a persisted message for one Conversation response.
func (d *DB) MessageByID(ctx context.Context, messageID string) (Message, error) {
	var m Message
	err := d.sql.QueryRowContext(ctx, `SELECT id, work_id, conversation_id, role, body, COALESCE(reply_to,''), created_at
		FROM messages WHERE id = ?`, messageID).
		Scan(&m.ID, &m.WorkID, &m.ConversationID, &m.Role, &m.Body, &m.ReplyTo, &m.CreatedAt)
	if err != nil {
		return Message{}, fmt.Errorf("store: message %q: %w", messageID, err)
	}
	return m, nil
}

// WorkFactByKey returns the latest confirmed or candidate version of one fact.
func (d *DB) WorkFactByKey(ctx context.Context, workID, kind, key string) (WorkFact, bool, error) {
	var f WorkFact
	err := d.sql.QueryRowContext(ctx, `SELECT kind, fact_key, value, status, revision, source_message_id, confirmation_basis
		FROM work_facts WHERE work_id = ? AND kind = ? AND fact_key = ? AND status != 'superseded'
		ORDER BY revision DESC LIMIT 1`, workID, kind, key).
		Scan(&f.Kind, &f.Key, &f.Value, &f.Status, &f.Revision, &f.SourceMessageID, &f.ConfirmationBasis)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkFact{}, false, nil
	}
	if err != nil {
		return WorkFact{}, false, fmt.Errorf("store: Work fact %q/%q: %w", kind, key, err)
	}
	return f, true, nil
}
