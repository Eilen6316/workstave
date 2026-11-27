// Package workflow connects persisted Conversation input to the Work's logical Main session.
package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"workstave/internal/runner"
	"workstave/internal/store"
)

var factLine = regexp.MustCompile(`(?im)^\s*(Confirmed|Candidate)\s+(requirement|decision)\s+([a-zA-Z][a-zA-Z0-9_.-]{0,63})\s*:\s*(\S.*)$`)

type inputSnapshot struct {
	Context        store.MainContext `json:"context"`
	SourceMessage  string            `json:"source_message_id"`
	ConversationID string            `json:"conversation_id"`
	CurrentInput   string            `json:"current_input"`
}

// ConfirmedFacts extracts only explicitly marked requirement and decision lines. Questions,
// discussion and hypotheses stay in the message history and cannot replace confirmed facts.
func ConfirmedFacts(body string) []store.FactProposal {
	matches := factLine.FindAllStringSubmatch(body, -1)
	proposals := make([]store.FactProposal, 0, len(matches))
	seen := make(map[string]bool)
	for _, match := range matches {
		kind := strings.ToLower(match[2])
		status := "candidate"
		if strings.EqualFold(match[1], "confirmed") {
			status = "confirmed"
		}
		key := kind + ":" + match[3]
		if seen[key] {
			continue
		}
		seen[key] = true
		proposals = append(proposals, store.FactProposal{Kind: kind, Key: match[3], Value: strings.TrimSpace(match[4]), Status: status})
	}
	return proposals
}

// Accept records one independently identified Conversation message and its Mailbox entry.
func Accept(ctx context.Context, db *store.DB, messageID, conversationID, harness, body string) (store.MailboxItem, error) {
	return db.AcceptUserMessage(ctx, messageID, conversationID, harness, body, ConfirmedFacts(body))
}

// ProcessPending handles queued inputs in Work sequence order. It stops on an unknown Run so
// the next input cannot be blindly applied to an unresolved Main execution.
func ProcessPending(ctx context.Context, db *store.DB, workID, workingDir string, harness func(name string) (runner.Harness, error)) error {
	if err := db.RecoverMailboxOnStart(ctx); err != nil {
		return err
	}
	for {
		item, found, err := db.ClaimNextMailbox(ctx, workID)
		if err != nil || !found {
			return err
		}
		state, err := db.MainContext(ctx, workID, item.ConversationID, item.Sequence)
		if err != nil {
			return errorsReturnPending(ctx, db, item.Sequence, err)
		}
		snapshot := inputSnapshot{
			Context: state, SourceMessage: item.MessageID, ConversationID: item.ConversationID, CurrentInput: item.Body,
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			return errorsReturnPending(ctx, db, item.Sequence, fmt.Errorf("workflow: encode Main input: %w", err))
		}
		h, err := harness(item.Harness)
		if err != nil {
			return errorsReturnPending(ctx, db, item.Sequence, err)
		}
		runID, err := newID("r-")
		if err != nil {
			return errorsReturnPending(ctx, db, item.Sequence, err)
		}
		prompt := compilePrompt(snapshot)
		_, err = runner.ProcessMainMessage(ctx, db, h, runner.MainParams{
			WorkID: workID, ConversationID: item.ConversationID, MailboxSequence: item.Sequence,
			MessageID: item.MessageID, ReplyID: "reply-" + item.MessageID,
			RunID: runID, OpID: "main-" + item.MessageID, WorkingDir: workingDir,
			InputSnapshot: string(encoded), ContextRevision: state.Revision, Prompt: prompt,
		})
		if err != nil {
			mainRun, found, lookupErr := db.MainRunForMessage(ctx, item.MessageID)
			if lookupErr != nil {
				return fmt.Errorf("workflow: process message %q: %v; inspect persisted Main Run: %w", item.MessageID, err, lookupErr)
			}
			if found {
				return fmt.Errorf("workflow: Main Run %q for message %q is persisted with outcome %q; input remains unreplayed: %w", mainRun.RunID, item.MessageID, mainRun.Outcome.String, err)
			}
			return errorsReturnPending(ctx, db, item.Sequence, err)
		}
		mailbox, err := db.MailboxByMessage(ctx, item.MessageID)
		if err != nil {
			return err
		}
		if mailbox.State == "unknown" {
			return fmt.Errorf("workflow: Main Run for message %q is unknown; later messages remain queued", item.MessageID)
		}
	}
}

func errorsReturnPending(ctx context.Context, db *store.DB, sequence int64, cause error) error {
	if err := db.ReturnMailboxToPending(context.WithoutCancel(ctx), sequence); err != nil {
		return fmt.Errorf("workflow: %v; release unstarted Mailbox input: %w", cause, err)
	}
	return cause
}

func compilePrompt(input inputSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the Main for Work %s. Answer the current user using the authoritative Work State below. Do not claim a candidate as confirmed. Keep this user's reply in their Conversation.\n", input.Context.WorkID)
	fmt.Fprintf(&b, "Work revision %d. Confirmed facts:\n", input.Context.Revision)
	hasConfirmed := false
	for _, fact := range input.Context.Facts {
		if fact.Status != "confirmed" {
			continue
		}
		hasConfirmed = true
		fmt.Fprintf(&b, "- %s %s, revision %d, source message %s: %s\n", fact.Kind, fact.Key, fact.Revision, fact.SourceMessageID, fact.Value)
	}
	if !hasConfirmed {
		b.WriteString("- none\n")
	}
	for _, fact := range input.Context.Facts {
		if fact.Status == "candidate" {
			fmt.Fprintf(&b, "Candidate %s %s, source message %s: %s\n", fact.Kind, fact.Key, fact.SourceMessageID, fact.Value)
		}
	}
	if len(input.Context.History) != 0 {
		b.WriteString("Recent messages from this Conversation only:\n")
		for _, message := range input.Context.History {
			fmt.Fprintf(&b, "%s: %s\n", message.Role, message.Body)
		}
	}
	fmt.Fprintf(&b, "Current user message from Conversation %s:\n%s\n", input.ConversationID, input.CurrentInput)
	b.WriteString("Answer directly. Do not use tools for this request.")
	return b.String()
}

func newID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("workflow: generate identity: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}
