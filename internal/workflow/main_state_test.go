package workflow

import (
	"context"
	"testing"

	"workstave/internal/outcome"
	"workstave/internal/runner"
)

func TestInvalidStructuredMainResponseIsConsumedAsVisibleFailure(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWork(ctx, "work-1", "main-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conversation-a", "work-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(ctx, db, "message-a", "conversation-a", "Please record the authentication requirement."); err != nil {
		t.Fatal(err)
	}
	harness := &fakeHarness{name: "codex", ackID: "thread-a", result: runner.Execution{Outcome: outcome.Completed, Text: "unstructured response"}}
	if err := ProcessPending(ctx, db, "work-1", "/work", func(string) (runner.Harness, error) { return harness, nil }); err == nil {
		t.Fatal("invalid Main response was not reported")
	}
	mailbox, err := db.MailboxByMessage(ctx, "message-a")
	if err != nil || mailbox.State != "completed" || mailbox.Result != "failed" || mailbox.ResultDetail != "invalid_main_response" {
		t.Fatalf("invalid response conclusion=%+v err=%v", mailbox, err)
	}
	mainRun, found, err := db.MainRunForMessage(ctx, "message-a")
	if err != nil || !found || mainRun.Outcome.String != string(outcome.Completed) || mainRun.OutputMessageID == "" {
		t.Fatalf("Main Run for invalid response=%+v found=%v err=%v", mainRun, found, err)
	}
	if facts, err := db.WorkFacts(ctx, "work-1"); err != nil || len(facts) != 0 {
		t.Fatalf("invalid response changed Work State: facts=%+v err=%v", facts, err)
	}
}

func TestConfirmedWorkChangeMustQuoteItsSourceMessage(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWork(ctx, "work-1", "main-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conversation-a", "work-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(ctx, db, "message-a", "conversation-a", "Could JWT work for authentication?"); err != nil {
		t.Fatal(err)
	}
	harness := &fakeHarness{name: "codex", ackID: "thread-a", result: runner.Execution{
		Outcome: outcome.Completed,
		Text:    `{"reply":"I recorded JWT as confirmed.","changes":[{"kind":"requirement","key":"auth_method","value":"JWT","status":"confirmed","basis":"explicit_user_statement","evidence":"We will use JWT.","expected_revision":0}]}`,
	}}
	if err := ProcessPending(ctx, db, "work-1", "/work", func(string) (runner.Harness, error) { return harness, nil }); err == nil {
		t.Fatal("Main confirmed a Work change without source-message evidence")
	}
	mailbox, err := db.MailboxByMessage(ctx, "message-a")
	if err != nil || mailbox.Result != "failed" || mailbox.ResultDetail != "invalid_work_change_evidence" {
		t.Fatalf("unsupported confirmation conclusion=%+v err=%v", mailbox, err)
	}
	if facts, err := db.WorkFacts(ctx, "work-1"); err != nil || len(facts) != 0 {
		t.Fatalf("unsupported confirmation changed Work State: facts=%+v err=%v", facts, err)
	}
}

func TestNaturalLanguageProposalsUseControlledFactVersions(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWork(ctx, "work-1", "main-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conversation-a", "work-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(ctx, db, "message-jwt", "conversation-a", "The authentication service must use JWT tokens."); err != nil {
		t.Fatal(err)
	}
	if facts, err := db.WorkFacts(ctx, "work-1"); err != nil || len(facts) != 0 {
		t.Fatalf("natural input changed authority before Main proposed it: facts=%+v err=%v", facts, err)
	}
	harness := &fakeHarness{name: "codex", ackID: "thread-shared", responses: []runner.Execution{
		{Outcome: outcome.Completed, Text: `{"reply":"I recorded JWT as the authentication requirement.","changes":[{"kind":"requirement","key":"auth_method","value":"JWT tokens","status":"confirmed","basis":"explicit_user_statement","evidence":"The authentication service must use JWT tokens.","expected_revision":0}]}`},
		{Outcome: outcome.Completed, Text: `{"reply":"Cookie sessions are a discussion candidate. JWT remains the confirmed requirement.","changes":[{"kind":"requirement","key":"auth_method","value":"Cookie sessions","status":"candidate","basis":"discussion","evidence":"Could Cookie sessions work here?","expected_revision":1}]}`},
		{Outcome: outcome.Completed, Text: `{"reply":"The authentication requirement is now Cookie sessions.","changes":[{"kind":"requirement","key":"auth_method","value":"Cookie sessions","status":"confirmed","basis":"explicit_user_statement","evidence":"Replace the JWT requirement with Cookie sessions.","expected_revision":2}]}`},
	}}
	process := func() error {
		return ProcessPending(ctx, db, "work-1", "/work", func(string) (runner.Harness, error) { return harness, nil })
	}
	if err := process(); err != nil {
		t.Fatal(err)
	}
	confirmed, found, err := db.WorkFactByKey(ctx, "work-1", "requirement", "auth_method")
	if err != nil || !found || confirmed.Status != "confirmed" || confirmed.Value != "JWT tokens" || confirmed.Revision != 1 {
		t.Fatalf("confirmed JWT fact=%+v found=%v err=%v", confirmed, found, err)
	}
	if _, err := Accept(ctx, db, "message-cookie-discussion", "conversation-a", "Could Cookie sessions work here?"); err != nil {
		t.Fatal(err)
	}
	if err := process(); err != nil {
		t.Fatal(err)
	}
	facts, err := db.WorkFacts(ctx, "work-1")
	if err != nil {
		t.Fatal(err)
	}
	confirmedValue := ""
	for _, fact := range facts {
		if fact.Kind == "requirement" && fact.Key == "auth_method" && fact.Status == "confirmed" {
			confirmedValue = fact.Value
		}
	}
	if confirmedValue != "JWT tokens" {
		t.Fatalf("discussion changed confirmed authority: facts=%+v", facts)
	}
	if candidate, found, err := db.WorkFactByKey(ctx, "work-1", "requirement", "auth_method"); err != nil || !found || candidate.Status != "candidate" || candidate.Value != "Cookie sessions" {
		t.Fatalf("discussion candidate=%+v found=%v err=%v", candidate, found, err)
	}
	if _, err := Accept(ctx, db, "message-cookie-change", "conversation-a", "Replace the JWT requirement with Cookie sessions."); err != nil {
		t.Fatal(err)
	}
	if err := process(); err != nil {
		t.Fatal(err)
	}
	current, found, err := db.WorkFactByKey(ctx, "work-1", "requirement", "auth_method")
	if err != nil || !found || current.Status != "confirmed" || current.Value != "Cookie sessions" || current.Revision != 3 || current.SourceMessageID != "message-cookie-change" {
		t.Fatalf("updated authority=%+v found=%v err=%v", current, found, err)
	}
	history, err := db.WorkFactHistory(ctx, "work-1", "requirement", "auth_method")
	if err != nil || len(history) != 3 || history[0].Status != "superseded" || history[0].Value != "JWT tokens" ||
		history[1].Status != "superseded" || history[1].Value != "Cookie sessions" || history[2].Value != "Cookie sessions" {
		t.Fatalf("fact history=%+v err=%v", history, err)
	}
}

func TestNormalMessagesFollowCurrentMainHarnessAfterExplicitExperiment(t *testing.T) {
	ctx := context.Background()
	db := openWorkflowStore(t)
	if err := db.CreateWorkWithHarness(ctx, "work-1", "main-1", "claude-code", "/work"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateConversation(ctx, "conversation-a", "work-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(ctx, db, "message-a", "conversation-a", "Start on Claude."); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptCrossHarnessExperiment(ctx, db, "message-b", "conversation-a", "codex", "Explicitly rebuild on Codex for this experiment."); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(ctx, db, "message-c", "conversation-a", "Continue with the current Main Harness."); err != nil {
		t.Fatal(err)
	}
	claude := &fakeHarness{name: "claude-code", nativeID: "native-claude", ackID: "native-claude", result: runner.Execution{Outcome: outcome.Completed, Text: `{"reply":"done","changes":[]}`}}
	codex := &fakeHarness{name: "codex", ackID: "thread-codex", result: runner.Execution{Outcome: outcome.Completed, Text: `{"reply":"done","changes":[]}`}}
	if err := ProcessPending(ctx, db, "work-1", "/work", func(name string) (runner.Harness, error) {
		if name == "claude-code" {
			return claude, nil
		}
		if name == "codex" {
			return codex, nil
		}
		t.Fatalf("unexpected Main Harness %q", name)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if claude.callCount != 1 || codex.callCount != 2 {
		t.Fatalf("Harness call counts Claude=%d Codex=%d, want 1 and 2", claude.callCount, codex.callCount)
	}
	message, err := db.MailboxByMessage(ctx, "message-c")
	if err != nil || message.Harness != "codex" || message.CrossHarnessExperiment {
		t.Fatalf("ordinary message after experiment=%+v err=%v", message, err)
	}
	retry, err := Accept(ctx, db, "message-c", "conversation-a", "Continue with the current Main Harness.")
	if err != nil || retry.OpID != message.OpID || retry.Sequence != message.Sequence {
		t.Fatalf("ordinary acceptance retry=%+v err=%v", retry, err)
	}
	work, err := db.Work(ctx, "work-1")
	if err != nil || work.MainHarness != "codex" {
		t.Fatalf("current Work Main Harness=%q err=%v", work.MainHarness, err)
	}
}
