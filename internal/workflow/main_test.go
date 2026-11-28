package workflow

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
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
	responses []runner.Execution
	requests  []runner.Request
	callCount int
	during    func()
}

type blockingHarness struct {
	mu       sync.Mutex
	started  chan struct{}
	release  chan struct{}
	calls    int
	requests []runner.Request
}

func (h *blockingHarness) Name() string        { return "codex" }
func (h *blockingHarness) NewNativeID() string { return "" }
func (h *blockingHarness) Execute(_ context.Context, request runner.Request, acknowledged func(string)) (runner.Execution, error) {
	h.mu.Lock()
	h.calls++
	call := h.calls
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	nativeID := request.NativeID
	if nativeID == "" {
		nativeID = "thread-1"
	}
	acknowledged(nativeID)
	if call == 1 {
		close(h.started)
		<-h.release
	}
	return runner.Execution{Outcome: outcome.Completed, Text: `{"reply":"processed","changes":[]}`}, nil
}

func (h *fakeHarness) Name() string        { return h.name }
func (h *fakeHarness) NewNativeID() string { return h.nativeID }
func (h *fakeHarness) Execute(_ context.Context, request runner.Request, acknowledged func(string)) (runner.Execution, error) {
	h.callCount++
	h.requests = append(h.requests, request)
	if h.during != nil {
		h.during()
	}
	if h.ackID != "" {
		acknowledged(h.ackID)
	}
	if h.callCount <= len(h.responses) {
		return h.responses[h.callCount-1], nil
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
	if err := db.CreateWorkWithHarness(ctx, "work-1", "main-1", "claude-code", "/work"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"conversation-a", "conversation-b"} {
		if err := db.CreateConversation(ctx, id, "work-1"); err != nil {
			t.Fatal(err)
		}
	}
	a, err := db.AcceptUserMessage(ctx, "message-a", "conversation-a", "Confirmed requirement inventory_format: weekly report is TSV.\nConversation A only: planning note delta-zebra.", ConfirmedFacts("Confirmed requirement inventory_format: weekly report is TSV."))
	if err != nil {
		t.Fatal(err)
	}
	b, err := AcceptCrossHarnessExperiment(ctx, db, "message-b", "conversation-b", "codex", "What report format was agreed? I do not have the earlier details.")
	if err != nil {
		t.Fatal(err)
	}

	claude := &fakeHarness{name: "claude-code", nativeID: "native-a", ackID: "native-a", result: runner.Execution{Outcome: outcome.Completed, Text: `{"reply":"Recorded the weekly TSV requirement.","changes":[]}`}}
	codex := &fakeHarness{name: "codex", ackID: "thread-b", result: runner.Execution{Outcome: outcome.Completed, Text: `{"reply":"The weekly report should be TSV.","changes":[]}`}}
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
	if claude.requests[0].OutputSchema != "" || codex.requests[0].OutputSchema != mainOutputSchema {
		t.Fatalf("Harness output schemas Claude=%q Codex_matches=%t", claude.requests[0].OutputSchema, codex.requests[0].OutputSchema == mainOutputSchema)
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
	for _, accepted := range []store.MailboxItem{a, b} {
		run, found, err := db.RunForOp(ctx, accepted.OpID)
		if err != nil || !found {
			t.Fatalf("stable Mailbox operation %q did not produce its Run: found=%v err=%v", accepted.OpID, found, err)
		}
		mainRun, found, err := db.MainRunForMessage(ctx, accepted.MessageID)
		if err != nil || !found || mainRun.RunID != run.ID {
			t.Fatalf("operation %q Main Run=%+v Run=%+v found=%v err=%v", accepted.OpID, mainRun, run, found, err)
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
	if _, err := Accept(ctx, db, "message-a", "conversation-a", "Please check status."); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWorkPaused(ctx, "work-1", true); err != nil {
		t.Fatal(err)
	}
	harness := &fakeHarness{name: "codex", result: runner.Execution{Outcome: outcome.Completed, Text: `{"reply":"done","changes":[]}`}}
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

func TestPauseAfterMainAuthorizationFinishesInFlightAndBlocksNextInput(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWork(ctx, "work-1", "main-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conversation-a", "work-1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"message-a", "message-b"} {
		if _, err := Accept(ctx, db, id, "conversation-a", "Continue the work."); err != nil {
			t.Fatal(err)
		}
	}
	harness := &blockingHarness{started: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- ProcessPending(ctx, db, "work-1", "/work", func(string) (runner.Harness, error) { return harness, nil })
	}()
	<-harness.started
	if err := db.SetWorkPaused(ctx, "work-1", true); err != nil {
		t.Fatal(err)
	}
	close(harness.release)
	if err := <-result; err == nil {
		t.Fatal("second Main input unexpectedly dispatched after Pause")
	}
	first, err := db.MailboxByMessage(ctx, "message-a")
	if err != nil || first.Result != "completed" {
		t.Fatalf("authorized in-flight input=%+v err=%v", first, err)
	}
	second, err := db.MailboxByMessage(ctx, "message-b")
	if err != nil || second.State != "pending" || second.Result != "" {
		t.Fatalf("paused next input=%+v err=%v", second, err)
	}
	if _, found, err := db.MainRunForMessage(ctx, "message-b"); err != nil || found {
		t.Fatalf("paused input got a Main Run: found=%v err=%v", found, err)
	}
	workIDs, err := db.PendingWorkIDs(ctx)
	if err != nil || len(workIDs) != 0 {
		t.Fatalf("paused pending Works=%v err=%v", workIDs, err)
	}
	if err := db.SetWorkPaused(ctx, "work-1", false); err != nil {
		t.Fatal(err)
	}
	workIDs, err = db.PendingWorkIDs(ctx)
	if err != nil || len(workIDs) != 1 || workIDs[0] != "work-1" {
		t.Fatalf("unpaused pending Works=%v err=%v", workIDs, err)
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
	if _, err := Accept(ctx, db, "message-a", "conversation-a", "Check status."); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptUserMessage(ctx, "message-b", "conversation-a", "Confirmed requirement auth_method: use JWT.", ConfirmedFacts("Confirmed requirement auth_method: use JWT.")); err != nil {
		t.Fatal(err)
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
	if _, found, err := db.ClaimNextMailbox(ctx, "work-1"); err != nil || found {
		t.Fatalf("unknown Main Run allowed a later claim: found=%v err=%v", found, err)
	}
	if _, found, err := db.WorkFactByKey(ctx, "work-1", "requirement", "auth_method"); err != nil || found {
		t.Fatalf("later fact was applied behind an unresolved Main Run: found=%v err=%v", found, err)
	}
	if err := db.CreateWork(ctx, "work-2", "main-2"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conversation-c", "work-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(ctx, db, "message-c", "conversation-c", "Independent Work remains available."); err != nil {
		t.Fatal(err)
	}
	workIDs, err := db.PendingWorkIDs(ctx)
	if err != nil || len(workIDs) != 1 || workIDs[0] != "work-2" {
		t.Fatalf("unknown Main Run blocked unrelated Work: pending=%v err=%v", workIDs, err)
	}
}

func TestConcurrentProcessorsDoNotRecoverOrApplyNextMessage(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWork(ctx, "work-1", "main-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conversation-a", "work-1"); err != nil {
		t.Fatal(err)
	}
	first, err := Accept(ctx, db, "message-a", "conversation-a", "Check the status.")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.AcceptUserMessage(ctx, "message-b", "conversation-a", "Confirmed requirement auth_method: use JWT.", ConfirmedFacts("Confirmed requirement auth_method: use JWT."))
	if err != nil {
		t.Fatal(err)
	}
	harness := &blockingHarness{started: make(chan struct{}), release: make(chan struct{})}
	process := func() error {
		return ProcessPending(ctx, db, "work-1", "/work", func(string) (runner.Harness, error) { return harness, nil })
	}
	firstResult := make(chan error, 1)
	go func() { firstResult <- process() }()
	<-harness.started

	if err := process(); err != nil {
		t.Errorf("second processor returned while the first owned the Work: %v", err)
	}
	firstState, err := db.MailboxByMessage(ctx, "message-a")
	if err != nil || firstState.State != "processing" {
		t.Errorf("active Mailbox input = %+v err=%v, want processing", firstState, err)
	}
	secondState, err := db.MailboxByMessage(ctx, "message-b")
	if err != nil || secondState.State != "pending" {
		t.Errorf("next Mailbox input = %+v err=%v, want pending", secondState, err)
	}
	if _, found, err := db.WorkFactByKey(ctx, "work-1", "requirement", "auth_method"); err != nil || found {
		t.Errorf("next input's fact was applied before Main reached it: found=%v err=%v", found, err)
	}

	close(harness.release)
	if err := <-firstResult; err != nil {
		t.Fatalf("first processor: %v", err)
	}
	for _, input := range []struct {
		message string
		seq     int64
	}{{"message-a", first.Sequence}, {"message-b", second.Sequence}} {
		item, err := db.MailboxByMessage(ctx, input.message)
		if err != nil || item.State != "completed" {
			t.Fatalf("Mailbox %s = %+v err=%v, want completed", input.message, item, err)
		}
		mainRun, found, err := db.MainRunForMessage(ctx, input.message)
		if err != nil || !found || mainRun.MailboxSequence != input.seq {
			t.Fatalf("Main Run for %s = %+v found=%v err=%v", input.message, mainRun, found, err)
		}
	}
	harness.mu.Lock()
	calls := harness.calls
	harness.mu.Unlock()
	if calls != 2 {
		t.Fatalf("Harness calls=%d, want one ordered call per message", calls)
	}
	if fact, found, err := db.WorkFactByKey(ctx, "work-1", "requirement", "auth_method"); err != nil || !found || fact.Value != "use JWT." {
		t.Fatalf("second input fact = %+v found=%v err=%v", fact, found, err)
	}
}

func TestClaudeMainReceivesAppendSystemPromptCodexDoesNot(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWorkWithHarness(ctx, "work-1", "main-1", "claude-code", "/work"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"conv-claude", "conv-codex"} {
		if err := db.CreateConversation(ctx, id, "work-1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Accept(ctx, db, "msg-claude", "conv-claude", "Note something."); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptCrossHarnessExperiment(ctx, db, "msg-codex", "conv-codex", "codex", "Note something."); err != nil {
		t.Fatal(err)
	}
	claude := &fakeHarness{name: "claude-code", nativeID: "nat-c", ackID: "nat-c", result: runner.Execution{Outcome: outcome.Completed, Text: `{"reply":"ok","changes":[]}`}}
	codex := &fakeHarness{name: "codex", ackID: "thread-x", result: runner.Execution{Outcome: outcome.Completed, Text: `{"reply":"ok","changes":[]}`}}
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
	if claude.requests[0].AppendSystemPrompt == "" {
		t.Error("claude-code Main did not receive AppendSystemPrompt")
	}
	if !strings.Contains(claude.requests[0].AppendSystemPrompt, "JSON") {
		t.Errorf("claude-code AppendSystemPrompt does not mention JSON: %q", claude.requests[0].AppendSystemPrompt)
	}
	if codex.requests[0].AppendSystemPrompt != "" {
		t.Errorf("codex Main received unexpected AppendSystemPrompt: %q", codex.requests[0].AppendSystemPrompt)
	}
}

func TestNoChangeExtractedDetailWhenChangesEmpty(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWork(ctx, "work-1", "main-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conv-a", "work-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(ctx, db, "msg-a", "conv-a", "Just a question, no fact change."); err != nil {
		t.Fatal(err)
	}
	// Harness returns valid JSON with an empty changes array — decode succeeds but nothing extracted.
	harness := &fakeHarness{name: "codex", ackID: "thread-1", result: runner.Execution{Outcome: outcome.Completed, Text: `{"reply":"No changes needed.","changes":[]}`}}
	if err := ProcessPending(ctx, db, "work-1", "/work", func(string) (runner.Harness, error) { return harness, nil }); err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.MailboxByMessage(ctx, "msg-a")
	if err != nil {
		t.Fatal(err)
	}
	if mailbox.Result != "completed" {
		t.Fatalf("Mailbox.Result = %q, want completed", mailbox.Result)
	}
	if mailbox.ResultDetail != "no_change_extracted" {
		t.Fatalf("Mailbox.ResultDetail = %q, want no_change_extracted", mailbox.ResultDetail)
	}
}

func TestBusyWorkDoesNotBlockAnotherWork(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	for _, id := range []string{"work-a", "work-b"} {
		if err := db.CreateWork(ctx, id, "main-"+id); err != nil {
			t.Fatal(err)
		}
		if err := db.CreateConversation(ctx, "conv-"+id, id); err != nil {
			t.Fatal(err)
		}
		if _, err := Accept(ctx, db, "message-"+id, "conv-"+id, "Process this independent Work."); err != nil {
			t.Fatal(err)
		}
	}
	blocked := &blockingHarness{started: make(chan struct{}), release: make(chan struct{})}
	blockedResult := make(chan error, 1)
	go func() {
		blockedResult <- ProcessPending(ctx, db, "work-a", "/work-a", func(string) (runner.Harness, error) { return blocked, nil })
	}()
	<-blocked.started
	other := &fakeHarness{name: "codex", ackID: "thread-b", result: runner.Execution{Outcome: outcome.Completed, Text: `{"reply":"done","changes":[]}`}}
	if err := ProcessPending(ctx, db, "work-b", "/work-b", func(string) (runner.Harness, error) { return other, nil }); err != nil {
		t.Fatalf("independent Work was blocked: %v", err)
	}
	if other.callCount != 1 {
		t.Fatalf("independent Work Harness calls=%d, want 1", other.callCount)
	}
	close(blocked.release)
	if err := <-blockedResult; err != nil {
		t.Fatal(err)
	}
}
