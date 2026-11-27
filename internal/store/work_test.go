package store

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"workstave/internal/outcome"
)

func TestWorkConversationsMailboxAndMainRunAssociations(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	if err := db.CreateWork(ctx, "w1", "main-w1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"conv-a", "conv-b"} {
		if err := db.CreateConversation(ctx, id, "w1"); err != nil {
			t.Fatal(err)
		}
	}

	a, err := db.AcceptUserMessage(ctx, "msg-a1", "conv-a", "Confirmed requirement inventory_format: weekly report is TSV.", []FactProposal{{Kind: "requirement", Key: "inventory_format", Value: "weekly report is TSV", Status: "confirmed"}})
	if err != nil || a.Sequence != 1 || a.State != "pending" {
		t.Fatalf("Conversation A acceptance = %+v err=%v", a, err)
	}
	if repeated, err := db.AcceptUserMessage(ctx, "msg-a1", "conv-a", "Confirmed requirement inventory_format: weekly report is TSV.", []FactProposal{{Kind: "requirement", Key: "inventory_format", Value: "weekly report is TSV", Status: "confirmed"}}); err != nil || repeated.Sequence != a.Sequence {
		t.Fatalf("repeat acceptance = %+v err=%v, want the original Mailbox identity", repeated, err)
	}
	if _, err := db.AcceptUserMessage(ctx, "msg-a1", "conv-a", "different input", nil); err == nil {
		t.Fatal("same message id with different input was accepted")
	}
	if _, err := db.AcceptUserMessage(ctx, "msg-a1", "conv-a", "Confirmed requirement inventory_format: weekly report is TSV.", []FactProposal{{Kind: "requirement", Key: "inventory_format", Value: "PDF", Status: "confirmed"}}); err == nil {
		t.Fatal("same message identity with a different state proposal was accepted")
	}
	claimedA, found, err := db.ClaimNextMailbox(ctx, "w1")
	if err != nil || !found || claimedA.MessageID != "msg-a1" || claimedA.State != "processing" {
		t.Fatalf("claim A = %+v found=%v err=%v", claimedA, found, err)
	}
	contextA, err := db.MainContext(ctx, "w1", "conv-a", claimedA.Sequence)
	if err != nil || contextA.Revision != 1 || len(contextA.Facts) != 1 || contextA.Facts[0].Status != "confirmed" {
		t.Fatalf("Main context A = %+v err=%v", contextA, err)
	}

	bindingID, err := db.BeginMainRun(ctx, MainRunStart{
		RunID: "run-a1", OpID: "main-msg-a1", WorkID: "w1", MainSessionID: "main-w1",
		ConversationID: "conv-a", MailboxSequence: claimedA.Sequence, SourceMessageID: "msg-a1",
		Harness: "codex", WorkingDir: "/work", NativeID: "thread-a", IntentHash: "intent-a1", InputSnapshot: `{"revision":1}`, ContextRevision: contextA.Revision,
	})
	if err != nil || bindingID == 0 {
		t.Fatalf("begin Main Run A binding=%d err=%v", bindingID, err)
	}
	if state, err := db.DispatchState(ctx, "run-a1"); err != nil || state != DispatchDispatching {
		t.Fatalf("Main Run dispatch=%q err=%v, want dispatching authorization", state, err)
	}
	if err := db.CompleteMainRun(ctx, "run-a1", MainCompletion{ReplyID: "reply-a1", Reply: "Requirement recorded.", Result: "completed"}, outcome.Completed); err != nil {
		t.Fatal(err)
	}
	mainRun, ok, err := db.MainRunForMessage(ctx, "msg-a1")
	if err != nil || !ok || mainRun.RunID != "run-a1" || mainRun.OutputMessageID != "reply-a1" || mainRun.ContextRevision != 1 {
		t.Fatalf("Main Run association=%+v found=%v err=%v", mainRun, ok, err)
	}
	message, err := db.MessageByID(ctx, "reply-a1")
	if err != nil || message.ConversationID != "conv-a" || message.ReplyTo != "msg-a1" || message.Role != "assistant" {
		t.Fatalf("reply association=%+v err=%v", message, err)
	}
	if item, err := db.MailboxByMessage(ctx, "msg-a1"); err != nil || item.State != "completed" {
		t.Fatalf("Mailbox A=%+v err=%v", item, err)
	}

	b, err := db.AcceptUserMessage(ctx, "msg-b1", "conv-b", "What format was agreed?", nil)
	if err != nil || b.Sequence <= a.Sequence {
		t.Fatalf("Conversation B acceptance=%+v err=%v", b, err)
	}
	claimedB, found, err := db.ClaimNextMailbox(ctx, "w1")
	if err != nil || !found || claimedB.MessageID != "msg-b1" {
		t.Fatalf("claim B=%+v found=%v err=%v", claimedB, found, err)
	}
	contextB, err := db.MainContext(ctx, "w1", "conv-b", claimedB.Sequence)
	if err != nil || contextB.Revision != 1 || len(contextB.Facts) != 1 || !strings.Contains(contextB.Facts[0].Value, "TSV") {
		t.Fatalf("compiled shared Work context B=%+v err=%v", contextB, err)
	}
	if len(contextB.History) != 0 {
		t.Fatalf("Conversation B received another Conversation's history: %+v", contextB.History)
	}
}

func TestAcceptanceAssignsStableKernelOperationIdentity(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	if err := db.CreateWork(ctx, "w1", "main-w1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conv-a", "w1"); err != nil {
		t.Fatal(err)
	}
	first, err := db.AcceptUserMessage(ctx, "request-1", "conv-a", "Summarize the current state.", nil)
	if err != nil {
		t.Fatal(err)
	}
	field, ok := reflect.TypeOf(first).FieldByName("OpID")
	if !ok || field.Type.Kind() != reflect.String {
		t.Fatal("accepted Mailbox item does not expose the Kernel-assigned operation identity")
	}
	opID := reflect.ValueOf(first).FieldByIndex(field.Index).String()
	if opID == "" || opID == "main-"+first.MessageID {
		t.Fatalf("operation identity %q was absent or derived from the Message ID", opID)
	}
	retry, err := db.AcceptUserMessage(ctx, "request-1", "conv-a", "Summarize the current state.", nil)
	retryOpID := reflect.ValueOf(retry).FieldByName("OpID")
	if err != nil || retry.MessageID != first.MessageID || !retryOpID.IsValid() || retryOpID.String() != opID {
		t.Fatalf("retry acceptance = %+v err=%v, want stable Message and operation identities", retry, err)
	}
	if _, err := db.AcceptUserMessage(ctx, "request-1", "conv-a", "Use a different intent.", nil); err == nil {
		t.Fatal("same request identity with a different intent was accepted")
	}
}

func TestCandidateDoesNotSupersedeConfirmedWorkFact(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	if err := db.CreateWork(ctx, "w1", "main-w1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conv-a", "w1"); err != nil {
		t.Fatal(err)
	}
	confirmedMessage, err := db.AcceptUserMessage(ctx, "msg-confirmed", "conv-a", "Confirmed decision format: use TSV.", []FactProposal{{Kind: "decision", Key: "format", Value: "use TSV", Status: "confirmed"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimNextMailbox(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if err := exec(t, db, `UPDATE mailbox SET state='completed' WHERE sequence=?`, confirmedMessage.Sequence); err != nil {
		t.Fatal(err)
	}
	candidate, err := db.AcceptUserMessage(ctx, "msg-candidate", "conv-a", "Candidate decision format: PDF might be easier.", []FactProposal{{Kind: "decision", Key: "format", Value: "PDF might be easier", Status: "candidate"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimNextMailbox(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if err := exec(t, db, `UPDATE mailbox SET state='completed' WHERE sequence=?`, candidate.Sequence); err != nil {
		t.Fatal(err)
	}
	confirmed, err := db.sql.QueryContext(ctx, `SELECT value FROM work_facts WHERE work_id='w1' AND kind='decision' AND fact_key='format' AND status='confirmed'`)
	if err != nil {
		t.Fatal(err)
	}
	defer confirmed.Close()
	if !confirmed.Next() {
		t.Fatal("candidate replaced the confirmed decision")
	}
	var value string
	if err := confirmed.Scan(&value); err != nil || value != "use TSV" {
		t.Fatalf("confirmed decision=%q err=%v", value, err)
	}
	var candidates int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_facts WHERE work_id='w1' AND status='candidate'`).Scan(&candidates); err != nil || candidates != 1 {
		t.Fatalf("candidate count=%d err=%v, want 1", candidates, err)
	}
}

func TestRecoverMailboxDoesNotReplayCommittedMainRun(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	if err := db.CreateWork(ctx, "w1", "main-w1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conv-a", "w1"); err != nil {
		t.Fatal(err)
	}
	item, err := db.AcceptUserMessage(ctx, "msg-a1", "conv-a", "Check status.", nil)
	if err != nil {
		t.Fatal(err)
	}
	item, found, err := db.ClaimNextMailbox(ctx, "w1")
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if _, err := db.BeginMainRun(ctx, MainRunStart{RunID: "run-a1", OpID: "main-msg-a1", WorkID: "w1", MainSessionID: "main-w1", ConversationID: "conv-a", MailboxSequence: item.Sequence, SourceMessageID: "msg-a1", Harness: "codex", WorkingDir: "/work", IntentHash: "intent-main", InputSnapshot: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverMailboxOnStart(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := db.MailboxByMessage(ctx, "msg-a1")
	if err != nil || got.State != "unknown" {
		t.Fatalf("recovered Mailbox=%q err=%v, want unknown", got.State, err)
	}
	if _, found, err := db.ClaimNextMailbox(ctx, "w1"); err != nil || found {
		t.Fatalf("unknown input was replayed: found=%v err=%v", found, err)
	}
}

func TestRetryingClaimDoesNotApplyFactProposalTwice(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	if err := db.CreateWork(ctx, "w1", "main-w1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conv-a", "w1"); err != nil {
		t.Fatal(err)
	}
	item, err := db.AcceptUserMessage(ctx, "msg-a1", "conv-a", "Confirmed decision format: use TSV.", []FactProposal{{Kind: "decision", Key: "format", Value: "use TSV", Status: "confirmed"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.ClaimNextMailbox(ctx, "w1"); err != nil || !found {
		t.Fatalf("first claim found=%v err=%v", found, err)
	}
	if err := db.ReturnMailboxToPending(ctx, item.Sequence); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.ClaimNextMailbox(ctx, "w1"); err != nil || !found {
		t.Fatalf("retry claim found=%v err=%v", found, err)
	}
	facts, err := db.WorkFacts(ctx, "w1")
	if err != nil || len(facts) != 1 || facts[0].Revision != 1 {
		t.Fatalf("Work facts after retried claim=%+v err=%v, want one revision", facts, err)
	}
	w, err := db.Work(ctx, "w1")
	if err != nil || w.Revision != 1 {
		t.Fatalf("Work revision=%d err=%v, want 1", w.Revision, err)
	}
}

func TestOwnerStartupRestoresUnstartedInputToPendingWithoutReapplyingFact(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	if err := db.CreateWork(ctx, "w1", "main-w1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conv-a", "w1"); err != nil {
		t.Fatal(err)
	}
	item, err := db.AcceptUserMessage(ctx, "msg-a1", "conv-a", "Confirmed requirement auth_method: use JWT.", []FactProposal{{Kind: "requirement", Key: "auth_method", Value: "use JWT", Status: "confirmed"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.ClaimNextMailbox(ctx, "w1"); err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if err := db.RecoverMailboxOnStart(ctx); err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.MailboxByMessage(ctx, "msg-a1")
	if err != nil || mailbox.State != "pending" {
		t.Fatalf("unstarted Mailbox recovery=%+v err=%v", mailbox, err)
	}
	if _, found, err := db.ClaimNextMailbox(ctx, "w1"); err != nil || !found {
		t.Fatalf("reclaimed message found=%v err=%v", found, err)
	}
	history, err := db.WorkFactHistory(ctx, "w1", "requirement", "auth_method")
	if err != nil || len(history) != 1 || history[0].Revision != 1 || item.Sequence != mailbox.Sequence {
		t.Fatalf("fact history after recovery=%+v err=%v", history, err)
	}
}

func TestConcurrentMailboxClaimsHaveOneOrderedOwner(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	if err := db.CreateWork(ctx, "w1", "main-w1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conv-a", "w1"); err != nil {
		t.Fatal(err)
	}
	first, err := db.AcceptUserMessage(ctx, "msg-1", "conv-a", "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptUserMessage(ctx, "msg-2", "conv-a", "second", nil); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan struct {
		item  MailboxItem
		found bool
		err   error
	}, 2)
	var ready sync.WaitGroup
	for range 2 {
		ready.Add(1)
		go func() {
			ready.Done()
			<-start
			item, found, err := db.ClaimNextMailbox(ctx, "w1")
			results <- struct {
				item  MailboxItem
				found bool
				err   error
			}{item, found, err}
		}()
	}
	ready.Wait()
	close(start)
	claimed := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent claim returned an error: %v", result.err)
		}
		if result.found {
			claimed++
			if result.item.Sequence != first.Sequence || result.item.MessageID != "msg-1" {
				t.Fatalf("claimed out of order: %+v, want first Mailbox item", result.item)
			}
		}
	}
	if claimed != 1 {
		t.Fatalf("simultaneous consumers claimed %d items, want one owner", claimed)
	}
	firstState, err := db.MailboxByMessage(ctx, "msg-1")
	if err != nil || firstState.State != "processing" {
		t.Fatalf("first Mailbox state=%+v err=%v", firstState, err)
	}
	secondState, err := db.MailboxByMessage(ctx, "msg-2")
	if err != nil || secondState.State != "pending" {
		t.Fatalf("second Mailbox state=%+v err=%v", secondState, err)
	}
}
