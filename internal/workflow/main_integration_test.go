package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workstave/internal/outcome"
	"workstave/internal/runner"
	"workstave/internal/store"
)

// TestRealSharedWorkAcrossConversations uses each installed Harness with its existing provider
// configuration. It makes one low-budget Claude request and one Codex request.
func TestRealSharedWorkAcrossConversations(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run the billable shared-Work experiment")
	}
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workstave.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	workDir := t.TempDir()
	if err := db.CreateWorkWithHarness(ctx, "real-work", "real-main", "claude-code", workDir); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"conversation-a", "conversation-b"} {
		if err := db.CreateConversation(ctx, id, "real-work"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Accept(ctx, db, "message-a", "conversation-a", "The authentication service must use JWT tokens.\nConversation A note: orange-comet."); err != nil {
		t.Fatal(err)
	}
	harnesses := map[string]runner.Harness{
		"claude-code": runner.Claude{MaxBudget: "0.05", Model: "claude-haiku-4-5-20251001"},
		"codex":       runner.Codex{},
	}
	process := func() error {
		return ProcessPending(ctx, db, "real-work", workDir, func(name string) (runner.Harness, error) {
			h, ok := harnesses[name]
			if !ok {
				t.Fatalf("unsupported harness %q", name)
			}
			return h, nil
		})
	}
	if err := process(); err != nil {
		t.Fatalf("Conversation A processing failed: %v", err)
	}
	if _, err := AcceptCrossHarnessExperiment(ctx, db, "message-b1", "conversation-b", "codex", "What authentication method did we choose? Answer with the method only."); err != nil {
		t.Fatal(err)
	}
	if err := process(); err != nil {
		t.Fatalf("Conversation B recall failed: %v", err)
	}
	mainRunA, found, err := db.MainRunForMessage(ctx, "message-a")
	if err != nil || !found || mainRunA.Outcome.String != string(outcome.Completed) {
		t.Fatalf("Conversation A Main Run found=%v outcome=%q err=%v", found, mainRunA.Outcome.String, err)
	}
	mainRunB, found, err := db.MainRunForMessage(ctx, "message-b1")
	if err != nil || !found || mainRunB.Outcome.String != string(outcome.Completed) {
		t.Fatalf("Conversation B Main Run found=%v outcome=%q err=%v", found, mainRunB.Outcome.String, err)
	}
	var snapshot inputSnapshot
	if err := json.Unmarshal([]byte(mainRunB.InputSnapshot), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Context.Facts) != 1 || snapshot.Context.Facts[0].SourceMessageID != "message-a" || !strings.Contains(snapshot.Context.Facts[0].Value, "JWT") || !strings.Contains(snapshot.CurrentInput, "What authentication method") || len(snapshot.Context.History) != 0 {
		t.Fatalf("Conversation B did not receive an isolated context with shared state: %+v", snapshot)
	}
	if strings.Contains(snapshot.CurrentInput, "orange-comet") {
		t.Fatal("Conversation B repeated a Conversation A-only detail")
	}
	reply, err := db.MessageByID(ctx, mainRunB.OutputMessageID)
	if err != nil || reply.ReplyTo != "message-b1" || !strings.Contains(strings.ToLower(reply.Body), "jwt") {
		t.Fatalf("Conversation B reply=%+v err=%v, want a correlated answer containing JWT", reply, err)
	}
	if _, err := Accept(ctx, db, "message-b2", "conversation-b", "Could Cookie sessions work instead?"); err != nil {
		t.Fatal(err)
	}
	if err := process(); err != nil {
		t.Fatalf("Cookie discussion processing failed: %v", err)
	}
	facts, err := db.WorkFacts(ctx, "real-work")
	if err != nil {
		t.Fatal(err)
	}
	var confirmedJWT store.WorkFact
	for _, fact := range facts {
		if fact.Kind == "requirement" && fact.Key == "auth_method" && fact.Status == "confirmed" {
			confirmedJWT = fact
		}
	}
	if confirmedJWT.Value != "JWT tokens" {
		t.Fatalf("Cookie discussion changed authority: facts=%+v", facts)
	}
	if _, err := Accept(ctx, db, "message-b3", "conversation-b", "Replace the JWT requirement with Cookie sessions."); err != nil {
		t.Fatal(err)
	}
	if err := process(); err != nil {
		t.Fatalf("Cookie requirement update failed: %v", err)
	}
	current, found, err := db.WorkFactByKey(ctx, "real-work", "requirement", "auth_method")
	if err != nil || !found || current.Status != "confirmed" || current.Value != "Cookie sessions" || current.Revision <= confirmedJWT.Revision {
		t.Fatalf("confirmed Cookie version=%+v found=%v err=%v", current, found, err)
	}
}
