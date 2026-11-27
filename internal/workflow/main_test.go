package workflow

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"workstave/internal/outcome"
	"workstave/internal/runner"
	"workstave/internal/store"
)

type fakeHarness struct {
	name      string
	nativeID  string
	ackID     string
	result    runner.Execution
	requests  []runner.Request
	callCount int
}

func (h *fakeHarness) Name() string        { return h.name }
func (h *fakeHarness) NewNativeID() string { return h.nativeID }
func (h *fakeHarness) Execute(_ context.Context, request runner.Request, acknowledged func(string)) (runner.Execution, error) {
	h.callCount++
	h.requests = append(h.requests, request)
	if h.ackID != "" {
		acknowledged(h.ackID)
	}
	return h.result, nil
}

func openWorkflowStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workstave.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestTwoConversationsShareWorkFactsAndKeepHistorySeparate(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWork(ctx, "work-1", "main-1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"conversation-a", "conversation-b"} {
		if err := db.CreateConversation(ctx, id, "work-1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Accept(ctx, db, "message-a", "conversation-a", "claude-code", "Confirmed requirement inventory_format: weekly report is TSV.\nConversation A only: planning note delta-zebra."); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(ctx, db, "message-b", "conversation-b", "codex", "What report format was agreed? I do not have the earlier details."); err != nil {
		t.Fatal(err)
	}

	claude := &fakeHarness{name: "claude-code", nativeID: "native-a", ackID: "native-a", result: runner.Execution{Outcome: outcome.Completed, Text: "Recorded the weekly TSV requirement."}}
	codex := &fakeHarness{name: "codex", ackID: "thread-b", result: runner.Execution{Outcome: outcome.Completed, Text: "The weekly report should be TSV."}}
	if err := ProcessPending(ctx, db, "work-1", "/work", func(name string) (runner.Harness, error) {
		switch name {
		case "claude-code":
			return claude, nil
		case "codex":
			return codex, nil
		default:
			t.Fatalf("unexpected harness %q", name)
			return nil, nil
		}
	}); err != nil {
		t.Fatal(err)
	}
	if claude.callCount != 1 || codex.callCount != 1 || codex.requests[0].Resume {
		t.Fatalf("calls claude=%d codex=%d request=%+v", claude.callCount, codex.callCount, codex.requests)
	}
	if strings.Contains(codex.requests[0].Prompt, "delta-zebra") || !strings.Contains(codex.requests[0].Prompt, "weekly report is TSV") || !strings.Contains(codex.requests[0].Prompt, "What report format was agreed?") {
		t.Fatalf("Conversation B prompt did not isolate history and include shared Work state:\n%s", codex.requests[0].Prompt)
	}

	for _, inputID := range []string{"message-a", "message-b"} {
		mainRun, found, err := db.MainRunForMessage(ctx, inputID)
		if err != nil || !found || mainRun.Outcome.String != string(outcome.Completed) || mainRun.OutputMessageID == "" {
			t.Fatalf("Main Run for %q = %+v found=%v err=%v", inputID, mainRun, found, err)
		}
		reply, err := db.MessageByID(ctx, mainRun.OutputMessageID)
		if err != nil || reply.ReplyTo != inputID || reply.ConversationID != mainRun.ConversationID {
			t.Fatalf("reply for %q = %+v err=%v", inputID, reply, err)
		}
		mailbox, err := db.MailboxByMessage(ctx, inputID)
		if err != nil || mailbox.State != "completed" {
			t.Fatalf("Mailbox for %q = %+v err=%v", inputID, mailbox, err)
		}
	}
	mainRunB, _, err := db.MainRunForMessage(ctx, "message-b")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot inputSnapshot
	if err := json.Unmarshal([]byte(mainRunB.InputSnapshot), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Context.Facts) != 1 || snapshot.Context.Facts[0].SourceMessageID != "message-a" || !strings.Contains(snapshot.Context.Facts[0].Value, "TSV") || !strings.Contains(snapshot.CurrentInput, "What report format") || len(snapshot.Context.History) != 0 {
		t.Fatalf("Conversation B input snapshot = %+v", snapshot)
	}
}

func TestPausedWorkReturnsUnstartedMailboxInputToPending(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWork(ctx, "work-1", "main-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conversation-a", "work-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(ctx, db, "message-a", "conversation-a", "codex", "Please check status."); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWorkPaused(ctx, "work-1", true); err != nil {
		t.Fatal(err)
	}
	harness := &fakeHarness{name: "codex", result: runner.Execution{Outcome: outcome.Completed, Text: "done"}}
	err := ProcessPending(ctx, db, "work-1", "/work", func(string) (runner.Harness, error) { return harness, nil })
	if err == nil || harness.callCount != 0 {
		t.Fatalf("ProcessPending err=%v harness calls=%d, want pause error and no launch", err, harness.callCount)
	}
	mailbox, err := db.MailboxByMessage(ctx, "message-a")
	if err != nil || mailbox.State != "pending" {
		t.Fatalf("paused Mailbox = %+v err=%v, want pending", mailbox, err)
	}
	if _, found, err := db.MainRunForMessage(ctx, "message-a"); err != nil || found {
		t.Fatalf("paused input has Main Run found=%v err=%v", found, err)
	}
}

func TestUnknownMainRunStopsLaterMailboxInput(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWork(ctx, "work-1", "main-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conversation-a", "work-1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"message-a", "message-b"} {
		if _, err := Accept(ctx, db, id, "conversation-a", "codex", "Check status."); err != nil {
			t.Fatal(err)
		}
	}
	harness := &fakeHarness{name: "codex", ackID: "thread-a", result: runner.Execution{Outcome: outcome.Unknown}}
	if err := ProcessPending(ctx, db, "work-1", "/work", func(string) (runner.Harness, error) { return harness, nil }); err == nil {
		t.Fatal("unknown Main Run should stop queue processing")
	}
	if harness.callCount != 1 {
		t.Fatalf("harness calls=%d, want queue stopped after first unknown Run", harness.callCount)
	}
	first, err := db.MailboxByMessage(ctx, "message-a")
	if err != nil || first.State != "unknown" {
		t.Fatalf("first Mailbox=%+v err=%v", first, err)
	}
	second, err := db.MailboxByMessage(ctx, "message-b")
	if err != nil || second.State != "pending" {
		t.Fatalf("second Mailbox=%+v err=%v, want pending", second, err)
	}
}
