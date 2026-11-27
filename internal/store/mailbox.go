package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// MailboxItem is one accepted user input in the Work-wide processing order.
type MailboxItem struct {
	Sequence       int64
	WorkID         string
	ConversationID string
	MessageID      string
	Harness        string
	State          string
	Body           string
}

// AcceptUserMessage atomically persists the independent Conversation message, its Mailbox
// position, and any explicitly confirmed fact proposals derived at the input boundary.
func (d *DB) AcceptUserMessage(ctx context.Context, messageID, conversationID, harness, body string, proposals []FactProposal) (MailboxItem, error) {
	body = strings.TrimSpace(body)
	if messageID == "" || conversationID == "" || body == "" {
		return MailboxItem{}, fmt.Errorf("store: message id, Conversation id and body are required")
	}
	if harness != "codex" && harness != "claude-code" {
		return MailboxItem{}, fmt.Errorf("store: unsupported Main harness %q", harness)
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return MailboxItem{}, fmt.Errorf("store: begin message acceptance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var workID string
	if err := tx.QueryRowContext(ctx, `SELECT work_id FROM conversations WHERE id = ?`, conversationID).Scan(&workID); err != nil {
		return MailboxItem{}, fmt.Errorf("store: resolve Conversation %q: %w", conversationID, err)
	}
	var existing MailboxItem
	err = tx.QueryRowContext(ctx, `SELECT m.id, m.work_id, m.conversation_id, m.body,
		b.sequence, b.harness, b.state FROM messages m JOIN mailbox b ON b.message_id = m.id WHERE m.id = ?`, messageID).
		Scan(&existing.MessageID, &existing.WorkID, &existing.ConversationID, &existing.Body,
			&existing.Sequence, &existing.Harness, &existing.State)
	if err == nil {
		if existing.ConversationID != conversationID || existing.Body != body || existing.Harness != harness {
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
	accepted := nowMS()
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id, work_id, conversation_id, role, body, created_at) VALUES (?,?,?,'user',?,?)`, messageID, workID, conversationID, body, accepted); err != nil {
		return MailboxItem{}, fmt.Errorf("store: persist user message %q: %w", messageID, err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO mailbox(work_id, conversation_id, message_id, harness, accepted_at) VALUES (?,?,?,?,?)`, workID, conversationID, messageID, harness, accepted)
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
	return MailboxItem{Sequence: sequence, WorkID: workID, ConversationID: conversationID, MessageID: messageID, Harness: harness, State: "pending", Body: body}, nil
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
	err = tx.QueryRowContext(ctx, `SELECT b.sequence, b.work_id, b.conversation_id, b.message_id, b.harness, b.state, m.body
		FROM mailbox b JOIN messages m ON m.id = b.message_id
		WHERE b.work_id = ? AND b.state = 'pending' ORDER BY b.sequence LIMIT 1`, workID).
		Scan(&item.Sequence, &item.WorkID, &item.ConversationID, &item.MessageID, &item.Harness, &item.State, &item.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return MailboxItem{}, false, nil
	}
	if err != nil {
		return MailboxItem{}, false, fmt.Errorf("store: find pending Mailbox input for Work %q: %w", workID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox SET state = 'processing', started_at = ? WHERE sequence = ? AND state = 'pending'`, nowMS(), item.Sequence); err != nil {
		return MailboxItem{}, false, fmt.Errorf("store: claim Mailbox input %d: %w", item.Sequence, err)
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO work_facts(work_id, kind, fact_key, revision, value, status, source_message_id, created_at)
			VALUES (?,?,?,?,?,?,?,?)`, workID, p.Kind, p.Key, revision, p.Value, p.Status, messageID, nowMS()); err != nil {
			return fmt.Errorf("store: record Work fact %s/%s: %w", p.Kind, p.Key, err)
		}
	}
	return nil
}

// RecoverMailboxOnStart reconciles Mailbox inputs left processing by a dead owner. Inputs with
// no Main Run are safe to return to pending. Inputs with a persisted Run and no terminal result
// become unknown and are never replayed automatically.
func (d *DB) RecoverMailboxOnStart(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `UPDATE mailbox SET state = 'pending', started_at = NULL
		WHERE state = 'processing' AND NOT EXISTS (SELECT 1 FROM main_runs WHERE main_runs.mailbox_sequence = mailbox.sequence)`); err != nil {
		return fmt.Errorf("store: return unstarted Mailbox inputs to pending: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx, `UPDATE mailbox SET state = 'unknown', finished_at = ?
		WHERE state = 'processing' AND EXISTS (
			SELECT 1 FROM main_runs mr JOIN runs r ON r.id = mr.run_id
			WHERE mr.mailbox_sequence = mailbox.sequence AND (r.outcome IS NULL OR r.outcome = 'unknown')
		)`, nowMS()); err != nil {
		return fmt.Errorf("store: mark interrupted Mailbox inputs unknown: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx, `UPDATE mailbox SET state = 'completed', finished_at = ?
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
	err := d.sql.QueryRowContext(ctx, `SELECT b.sequence, b.work_id, b.conversation_id, b.message_id, b.harness, b.state, m.body
		FROM mailbox b JOIN messages m ON m.id = b.message_id WHERE b.message_id = ?`, messageID).
		Scan(&item.Sequence, &item.WorkID, &item.ConversationID, &item.MessageID, &item.Harness, &item.State, &item.Body)
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
	err := d.sql.QueryRowContext(ctx, `SELECT kind, fact_key, value, status, revision, source_message_id
		FROM work_facts WHERE work_id = ? AND kind = ? AND fact_key = ? AND status != 'superseded'
		ORDER BY revision DESC LIMIT 1`, workID, kind, key).
		Scan(&f.Kind, &f.Key, &f.Value, &f.Status, &f.Revision, &f.SourceMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkFact{}, false, nil
	}
	if err != nil {
		return WorkFact{}, false, fmt.Errorf("store: Work fact %q/%q: %w", kind, key, err)
	}
	return f, true, nil
}
