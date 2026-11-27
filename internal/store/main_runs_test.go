package store

import (
	"context"
	"testing"

	"workstave/internal/outcome"
)

func prepareVersionedMainRun(t *testing.T) (*DB, MailboxItem, MainContext) {
	t.Helper()
	ctx := context.Background()
	db := openTemp(t)
	if err := db.CreateWork(ctx, "w1", "main-w1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conv-a", "w1"); err != nil {
		t.Fatal(err)
	}
	first, err := db.AcceptUserMessage(ctx, "msg-jwt", "conv-a", "Use JWT for authentication.", []FactProposal{{Kind: "requirement", Key: "auth_method", Value: "JWT", Status: "confirmed"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.ClaimNextMailbox(ctx, "w1"); err != nil || !found {
		t.Fatalf("first claim found=%v err=%v", found, err)
	}
	if err := exec(t, db, `UPDATE mailbox SET state='completed' WHERE sequence=?`, first.Sequence); err != nil {
		t.Fatal(err)
	}
	second, err := db.AcceptUserMessage(ctx, "msg-second", "conv-a", "Should authentication switch to Cookie sessions?", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, found, err := db.ClaimNextMailbox(ctx, "w1")
	if err != nil || !found {
		t.Fatalf("second claim found=%v err=%v", found, err)
	}
	state, err := db.MainContext(ctx, "w1", "conv-a", second.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginMainRun(ctx, MainRunStart{
		RunID: "run-second", OpID: second.OpID, WorkID: "w1", MainSessionID: "main-w1",
		ConversationID: "conv-a", MailboxSequence: second.Sequence, SourceMessageID: second.MessageID,
		Harness: "codex", WorkingDir: "/work", NativeID: "thread-main", IntentHash: "intent-second",
		InputSnapshot: `{"revision":1}`, ContextRevision: state.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	if err := exec(t, db, `INSERT INTO messages(id,work_id,conversation_id,role,body,created_at) VALUES ('msg-control','w1','conv-a','user','Use Passkeys and choose TSV.',?)`, nowMS()); err != nil {
		t.Fatal(err)
	}
	return db, second, state
}

func TestRelatedVersionChangeRejectsStaleMainProposal(t *testing.T) {
	ctx := context.Background()
	db, input, _ := prepareVersionedMainRun(t)
	if err := db.ApplyUserWorkChanges(ctx, "w1", "msg-control", []WorkChange{{
		Kind: "requirement", Key: "auth_method", Value: "Passkeys", Status: "confirmed",
		Basis: "explicit_user_statement", Evidence: "Use Passkeys", ExpectedRevision: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	completion := MainCompletion{
		ReplyID: "reply-second", Reply: "The authentication method is Cookie sessions.", Result: "completed",
		Changes: []WorkChange{{Kind: "requirement", Key: "auth_method", Value: "Cookie sessions", Status: "confirmed",
			Basis: "explicit_user_statement", Evidence: "Should authentication switch to Cookie sessions?", ExpectedRevision: 1}},
	}
	if err := db.CompleteMainRun(ctx, "run-second", completion, outcome.Completed); err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.MailboxByMessage(ctx, input.MessageID)
	if err != nil || mailbox.Result != "failed" || mailbox.ResultDetail != "stale_work_state" {
		t.Fatalf("stale Mailbox conclusion=%+v err=%v", mailbox, err)
	}
	current, found, err := db.WorkFactByKey(ctx, "w1", "requirement", "auth_method")
	if err != nil || !found || current.Value != "Passkeys" || current.Status != "confirmed" {
		t.Fatalf("stale proposal changed authority: %+v found=%v err=%v", current, found, err)
	}
}

func TestUnrelatedVersionChangeDoesNotRejectMainProposal(t *testing.T) {
	ctx := context.Background()
	db, input, _ := prepareVersionedMainRun(t)
	if err := db.ApplyUserWorkChanges(ctx, "w1", "msg-control", []WorkChange{{
		Kind: "decision", Key: "report_format", Value: "TSV", Status: "confirmed",
		Basis: "explicit_user_statement", Evidence: "choose TSV", ExpectedRevision: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	completion := MainCompletion{
		ReplyID: "reply-second", Reply: "Cookie sessions are a candidate.", Result: "completed",
		Changes: []WorkChange{{Kind: "requirement", Key: "auth_method", Value: "Cookie sessions", Status: "candidate",
			Basis: "discussion", Evidence: "Should authentication switch to Cookie sessions?", ExpectedRevision: 1}},
	}
	if err := db.CompleteMainRun(ctx, "run-second", completion, outcome.Completed); err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.MailboxByMessage(ctx, input.MessageID)
	if err != nil || mailbox.Result != "completed" {
		t.Fatalf("unrelated change rejected Main input: Mailbox=%+v err=%v", mailbox, err)
	}
	current, found, err := db.WorkFactByKey(ctx, "w1", "requirement", "auth_method")
	if err != nil || !found || current.Value != "Cookie sessions" || current.Status != "candidate" || current.Revision != 3 {
		t.Fatalf("versioned candidate=%+v found=%v err=%v", current, found, err)
	}
}

func TestStructuredWorkChangeRetryIsIdempotentAndIntentBound(t *testing.T) {
	ctx := context.Background()
	db, _, _ := prepareVersionedMainRun(t)
	change := WorkChange{Kind: "decision", Key: "report_format", Value: "TSV", Status: "confirmed", Basis: "explicit_user_statement", Evidence: "choose TSV", ExpectedRevision: 0}
	if err := db.ApplyUserWorkChanges(ctx, "w1", "msg-control", []WorkChange{change}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyUserWorkChanges(ctx, "w1", "msg-control", []WorkChange{change}); err != nil {
		t.Fatalf("same structured operation retry: %v", err)
	}
	conflict := change
	conflict.Value = "PDF"
	if err := db.ApplyUserWorkChanges(ctx, "w1", "msg-control", []WorkChange{conflict}); err == nil {
		t.Fatal("same source identity accepted a different Work change intent")
	}
	history, err := db.WorkFactHistory(ctx, "w1", "decision", "report_format")
	if err != nil || len(history) != 1 || history[0].Value != "TSV" {
		t.Fatalf("structured change history=%+v err=%v", history, err)
	}
}

func TestMainRunAndMailboxRollBackTogetherWhenReplyPersistenceFails(t *testing.T) {
	ctx := context.Background()
	db, input, _ := prepareVersionedMainRun(t)
	if err := db.CompleteMainRun(ctx, "run-second", MainCompletion{
		ReplyID: input.MessageID,
		Reply:   "This reply id collides with the source user message.",
		Result:  "completed",
	}, outcome.Completed); err == nil {
		t.Fatal("completion succeeded despite the reply identity collision")
	}

	mainRun, found, err := db.MainRunForMessage(ctx, input.MessageID)
	if err != nil || !found || mainRun.Outcome.Valid || mainRun.OutputMessageID != "" {
		t.Fatalf("Main Run after failed completion=%+v found=%v err=%v, want an uncompleted Run", mainRun, found, err)
	}
	mailbox, err := db.MailboxByMessage(ctx, input.MessageID)
	if err != nil || mailbox.State != "processing" || mailbox.Result != "" {
		t.Fatalf("Mailbox after failed completion=%+v err=%v, want the same in-progress conclusion", mailbox, err)
	}

	if _, err := db.RecoverOnStart(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverMailboxOnStart(ctx); err != nil {
		t.Fatal(err)
	}
	mainRun, found, err = db.MainRunForMessage(ctx, input.MessageID)
	if err != nil || !found || !mainRun.Outcome.Valid || mainRun.Outcome.String != string(outcome.Unknown) {
		t.Fatalf("recovered Main Run=%+v found=%v err=%v, want unknown", mainRun, found, err)
	}
	mailbox, err = db.MailboxByMessage(ctx, input.MessageID)
	if err != nil || mailbox.State != "unknown" || mailbox.Result != "unknown" {
		t.Fatalf("recovered Mailbox=%+v err=%v, want matching unknown conclusion", mailbox, err)
	}
}
