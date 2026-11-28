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

type structuredChange struct {
	Kind             string `json:"kind"`
	Key              string `json:"key"`
	Value            string `json:"value"`
	Status           string `json:"status"`
	Basis            string `json:"basis"`
	Evidence         string `json:"evidence"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type structuredReply struct {
	Reply   string             `json:"reply"`
	Changes []structuredChange `json:"changes"`
}

// ConfirmedFacts parses an explicit fixture format for deterministic tests. Accept does not
// call it for ordinary user messages.
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
func Accept(ctx context.Context, db *store.DB, messageID, conversationID, body string) (store.MailboxItem, error) {
	return db.AcceptUserMessage(ctx, messageID, conversationID, body, nil)
}

// AcceptCrossHarnessExperiment explicitly records an input that requests a Main Harness rebuild.
func AcceptCrossHarnessExperiment(ctx context.Context, db *store.DB, messageID, conversationID, harness, body string) (store.MailboxItem, error) {
	return db.AcceptHarnessExperiment(ctx, messageID, conversationID, harness, body)
}

// ProcessPending handles queued inputs in Work sequence order. It stops on an unknown Run so
// the next input cannot be blindly applied to an unresolved Main execution.
func ProcessPending(ctx context.Context, db *store.DB, workID, workingDir string, harness func(name string) (runner.Harness, error)) error {
	work, err := db.Work(ctx, workID)
	if err != nil {
		return err
	}
	if work.WorkingDir != "" {
		workingDir = work.WorkingDir
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
			RunID: runID, OpID: item.OpID, WorkingDir: workingDir, OutputSchema: mainOutputSchema,
			InputSnapshot: string(encoded), ContextRevision: state.Revision, Prompt: prompt,
			DecodeReply: decodeMainReply, CrossHarnessExperiment: item.CrossHarnessExperiment,
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
		if mailbox.Result == "failed" {
			return fmt.Errorf("workflow: message %q was consumed with failure %q; later messages remain queued", item.MessageID, mailbox.ResultDetail)
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
	fmt.Fprintf(&b, "You are the Main for Work %s. Return one JSON object with keys reply and changes. Each change must have kind, key, value, status, basis, evidence, and expected_revision. Use status confirmed only for a clear user requirement or decision. Use basis explicit_user_statement for that case and set evidence to an exact quote from the current user message. Questions and discussions may create candidate changes with basis discussion and an exact quote in evidence, but never replace a confirmed fact. Use basis model_inference for unverified model conclusions and keep them candidate. expected_revision is the latest revision shown for that same fact key, or 0 if none is shown. Use an empty changes array when no state change is justified. The source message is supplied by the Kernel and must not be invented. Keep the reply in this Conversation.\n", input.Context.WorkID)
	fmt.Fprintf(&b, "Work revision %d. Confirmed facts:\n", input.Context.Revision)
	hasConfirmed := false
	for _, fact := range input.Context.Facts {
		if fact.Status != "confirmed" {
			continue
		}
		hasConfirmed = true
		fmt.Fprintf(&b, "- %s %s, revision %d, source message %s, basis %s: %s\n", fact.Kind, fact.Key, fact.Revision, fact.SourceMessageID, fact.ConfirmationBasis, fact.Value)
	}
	if !hasConfirmed {
		b.WriteString("- none\n")
	}
	for _, fact := range input.Context.Facts {
		if fact.Status == "candidate" {
			fmt.Fprintf(&b, "Candidate %s %s, revision %d, source message %s, basis %s: %s\n", fact.Kind, fact.Key, fact.Revision, fact.SourceMessageID, fact.ConfirmationBasis, fact.Value)
		}
	}
	if len(input.Context.History) != 0 {
		b.WriteString("Recent messages from this Conversation only:\n")
		for _, message := range input.Context.History {
			fmt.Fprintf(&b, "%s: %s\n", message.Role, message.Body)
		}
	}
	fmt.Fprintf(&b, "Current user message from Conversation %s:\n%s\n", input.ConversationID, input.CurrentInput)
	b.WriteString("The reply field must directly answer the current user message using the Work facts above. Do not acknowledge these instructions or describe the JSON format. Return JSON only. Do not use tools for this request.")
	return b.String()
}

// mainOutputSchema describes the structured Main response. A Harness that supports schema-
// validated output returns the answer in this shape instead of free text; decodeMainReply still
// validates it, because the Kernel does not trust the Harness to have enforced the schema.
const mainOutputSchema = `{"type":"object","additionalProperties":false,"properties":{"reply":{"type":"string"},"changes":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"kind":{"type":"string"},"key":{"type":"string"},"value":{"type":"string"},"status":{"type":"string"},"basis":{"type":"string"},"evidence":{"type":"string"},"expected_revision":{"type":"integer"}},"required":["kind","key","value","status","basis","evidence","expected_revision"]}}},"required":["reply","changes"]}`

func decodeMainReply(text string) (store.MainCompletion, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```json") && strings.HasSuffix(text, "```") {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "```json"), "```"))
	}
	var response structuredReply
	if err := json.Unmarshal([]byte(text), &response); err != nil {
		return store.MainCompletion{}, fmt.Errorf("workflow: decode Main response: %w", err)
	}
	if strings.TrimSpace(response.Reply) == "" {
		return store.MainCompletion{}, fmt.Errorf("workflow: Main response has no reply")
	}
	if response.Changes == nil {
		return store.MainCompletion{}, fmt.Errorf("workflow: Main response has no changes array")
	}
	changes := make([]store.WorkChange, 0, len(response.Changes))
	for _, change := range response.Changes {
		changes = append(changes, store.WorkChange{
			Kind: change.Kind, Key: change.Key, Value: change.Value, Status: change.Status,
			Basis: change.Basis, Evidence: change.Evidence, ExpectedRevision: change.ExpectedRevision,
		})
	}
	return store.MainCompletion{Reply: response.Reply, Changes: changes, Result: "completed"}, nil
}

func newID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("workflow: generate identity: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}
