package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Work is the authority boundary and owns one logical Main session.
type Work struct {
	ID              string
	MainSessionID   string
	Revision        int64
	ControllerEpoch int64
	CreatedAt       int64
}

// Conversation has its own message history and belongs to exactly one Work.
type Conversation struct {
	ID        string
	WorkID    string
	CreatedAt int64
}

// FactProposal is derived from an explicitly marked user statement at the input boundary.
type FactProposal struct {
	Kind   string
	Key    string
	Value  string
	Status string
}

// WorkFact is a versioned, sourced Work state record.
type WorkFact struct {
	Kind            string
	Key             string
	Value           string
	Status          string
	Revision        int64
	SourceMessageID string
}

// ContextMessage is a prior message from the current Conversation only.
type ContextMessage struct {
	ID   string
	Role string
	Body string
}

// MainContext is a consistent authority snapshot used to compile one Main input.
type MainContext struct {
	WorkID          string
	MainSessionID   string
	Revision        int64
	ControllerEpoch int64
	Facts           []WorkFact
	History         []ContextMessage
}

// CreateWork creates the Work and its stable logical Main session atomically.
func (d *DB) CreateWork(ctx context.Context, workID, mainSessionID string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin work %q: %w", workID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_pause(work_id) VALUES (?)`, workID); err != nil {
		return fmt.Errorf("store: initialize pause state for work %q: %w", workID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, work_id, role, created_at) VALUES (?,?,'main',?)`, mainSessionID, workID, nowMS()); err != nil {
		return fmt.Errorf("store: create Main session for work %q: %w", workID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO works(id, main_session_id, created_at) VALUES (?,?,?)`, workID, mainSessionID, nowMS()); err != nil {
		return fmt.Errorf("store: create work %q: %w", workID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit work %q: %w", workID, err)
	}
	return nil
}

// Work returns one persisted Work and its stable Main session identity.
func (d *DB) Work(ctx context.Context, workID string) (Work, error) {
	var w Work
	err := d.sql.QueryRowContext(ctx, `SELECT id, main_session_id, revision, controller_epoch, created_at FROM works WHERE id = ?`, workID).
		Scan(&w.ID, &w.MainSessionID, &w.Revision, &w.ControllerEpoch, &w.CreatedAt)
	if err != nil {
		return Work{}, fmt.Errorf("store: work %q: %w", workID, err)
	}
	return w, nil
}

// CreateConversation creates a new independent message history inside a Work.
func (d *DB) CreateConversation(ctx context.Context, id, workID string) error {
	if _, err := d.sql.ExecContext(ctx, `INSERT INTO conversations(id, work_id, created_at) VALUES (?,?,?)`, id, workID, nowMS()); err != nil {
		return fmt.Errorf("store: create Conversation %q in Work %q: %w", id, workID, err)
	}
	return nil
}

// Conversation returns one Conversation and its Work owner.
func (d *DB) Conversation(ctx context.Context, id string) (Conversation, error) {
	var c Conversation
	err := d.sql.QueryRowContext(ctx, `SELECT id, work_id, created_at FROM conversations WHERE id = ?`, id).
		Scan(&c.ID, &c.WorkID, &c.CreatedAt)
	if err != nil {
		return Conversation{}, fmt.Errorf("store: Conversation %q: %w", id, err)
	}
	return c, nil
}

// MainContext reads confirmed shared facts and a bounded history from this Conversation in
// one SQLite read transaction. Messages from other Conversations are never included.
func (d *DB) MainContext(ctx context.Context, workID, conversationID string, beforeSequence int64) (MainContext, error) {
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MainContext{}, fmt.Errorf("store: begin Main context read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var c MainContext
	err = tx.QueryRowContext(ctx, `SELECT id, main_session_id, revision, controller_epoch FROM works WHERE id = ?`, workID).
		Scan(&c.WorkID, &c.MainSessionID, &c.Revision, &c.ControllerEpoch)
	if err != nil {
		return MainContext{}, fmt.Errorf("store: Main context Work %q: %w", workID, err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT kind, fact_key, value, status, revision, source_message_id
		FROM work_facts WHERE work_id = ? AND status IN ('confirmed','candidate')
		ORDER BY kind, fact_key, revision`, workID)
	if err != nil {
		return MainContext{}, fmt.Errorf("store: read Work facts for %q: %w", workID, err)
	}
	for rows.Next() {
		var f WorkFact
		if err := rows.Scan(&f.Kind, &f.Key, &f.Value, &f.Status, &f.Revision, &f.SourceMessageID); err != nil {
			_ = rows.Close()
			return MainContext{}, fmt.Errorf("store: scan Work fact: %w", err)
		}
		c.Facts = append(c.Facts, f)
	}
	if err := rows.Close(); err != nil {
		return MainContext{}, fmt.Errorf("store: close Work facts: %w", err)
	}
	if err := rows.Err(); err != nil {
		return MainContext{}, fmt.Errorf("store: read Work facts: %w", err)
	}
	if beforeSequence > 0 {
		rows, err = tx.QueryContext(ctx, `SELECT m.id, m.role, m.body
			FROM messages m JOIN mailbox b ON b.message_id = CASE WHEN m.role = 'user' THEN m.id ELSE m.reply_to END
			WHERE m.conversation_id = ? AND b.sequence < ?
			ORDER BY b.sequence DESC, CASE m.role WHEN 'assistant' THEN 1 ELSE 0 END DESC LIMIT 12`, conversationID, beforeSequence)
		if err != nil {
			return MainContext{}, fmt.Errorf("store: read Conversation history %q: %w", conversationID, err)
		}
		var reverse []ContextMessage
		for rows.Next() {
			var m ContextMessage
			if err := rows.Scan(&m.ID, &m.Role, &m.Body); err != nil {
				_ = rows.Close()
				return MainContext{}, fmt.Errorf("store: scan Conversation history: %w", err)
			}
			reverse = append(reverse, m)
		}
		if err := rows.Close(); err != nil {
			return MainContext{}, fmt.Errorf("store: close Conversation history: %w", err)
		}
		if err := rows.Err(); err != nil {
			return MainContext{}, fmt.Errorf("store: read Conversation history: %w", err)
		}
		for i := len(reverse) - 1; i >= 0; i-- {
			c.History = append(c.History, reverse[i])
		}
	}
	if err := tx.Commit(); err != nil {
		return MainContext{}, fmt.Errorf("store: commit Main context read: %w", err)
	}
	return c, nil
}

// WorkFacts lists the current confirmed and candidate records without conversation history.
func (d *DB) WorkFacts(ctx context.Context, workID string) ([]WorkFact, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT kind, fact_key, value, status, revision, source_message_id
		FROM work_facts WHERE work_id = ? AND status IN ('confirmed','candidate')
		ORDER BY kind, fact_key, revision`, workID)
	if err != nil {
		return nil, fmt.Errorf("store: list Work facts %q: %w", workID, err)
	}
	defer rows.Close()
	var facts []WorkFact
	for rows.Next() {
		var f WorkFact
		if err := rows.Scan(&f.Kind, &f.Key, &f.Value, &f.Status, &f.Revision, &f.SourceMessageID); err != nil {
			return nil, fmt.Errorf("store: scan Work fact: %w", err)
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list Work facts: %w", err)
	}
	return facts, nil
}

// ConversationMessages returns one Conversation's messages in submission order, including
// the mailbox state of each user input.
func (d *DB) ConversationMessages(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT m.id, m.work_id, m.conversation_id, m.role, m.body,
		COALESCE(m.reply_to,''), m.created_at, COALESCE(b.state,'completed')
		FROM messages m LEFT JOIN mailbox b ON b.message_id = CASE WHEN m.role = 'user' THEN m.id ELSE m.reply_to END
	WHERE m.conversation_id = ? ORDER BY COALESCE(b.sequence, 0), CASE m.role WHEN 'assistant' THEN 1 ELSE 0 END, m.id`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("store: list messages for Conversation %q: %w", conversationID, err)
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.WorkID, &m.ConversationID, &m.Role, &m.Body, &m.ReplyTo, &m.CreatedAt, &m.MailboxState); err != nil {
			return nil, fmt.Errorf("store: scan Conversation message: %w", err)
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list Conversation messages: %w", err)
	}
	return messages, nil
}

// Message is a persisted Conversation message. MailboxState is empty for assistant messages.
type Message struct {
	ID             string
	WorkID         string
	ConversationID string
	Role           string
	Body           string
	ReplyTo        string
	CreatedAt      int64
	MailboxState   string
}

// MainRun is the causal association between one accepted input and one runner Run.
type MainRun struct {
	RunID           string
	WorkID          string
	ConversationID  string
	MailboxSequence int64
	SourceMessageID string
	InputSnapshot   string
	ContextRevision int64
	ControllerEpoch int64
	OutputMessageID string
	StartedAt       int64
	EndedAt         sql.NullInt64
	Outcome         sql.NullString
}

// MainRunForMessage returns the Main Run associated with a source Message, if one exists.
func (d *DB) MainRunForMessage(ctx context.Context, messageID string) (MainRun, bool, error) {
	var r MainRun
	err := d.sql.QueryRowContext(ctx, `SELECT mr.run_id, mr.work_id, mr.conversation_id, mr.mailbox_sequence,
		mr.source_message_id, mr.input_snapshot, mr.context_revision, mr.controller_epoch,
		COALESCE(mr.output_message_id,''), mr.started_at, r.ended_at, r.outcome
		FROM main_runs mr JOIN runs r ON r.id = mr.run_id WHERE mr.source_message_id = ?`, messageID).
		Scan(&r.RunID, &r.WorkID, &r.ConversationID, &r.MailboxSequence, &r.SourceMessageID,
			&r.InputSnapshot, &r.ContextRevision, &r.ControllerEpoch, &r.OutputMessageID,
			&r.StartedAt, &r.EndedAt, &r.Outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return MainRun{}, false, nil
	}
	if err != nil {
		return MainRun{}, false, fmt.Errorf("store: Main Run for message %q: %w", messageID, err)
	}
	return r, true, nil
}
