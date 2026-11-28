package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"workstave/internal/outcome"
)

// Real-harness test: runs both adapters against the installed CLIs with the user's own
// configuration (provider, endpoint and keys are never overridden). It makes billable
// requests and only runs when WORKSTAVE_REAL_HARNESS=1.
func TestRealCreateThenResume(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run against the real harnesses (billable)")
	}
	cases := []struct {
		name string
		h    Harness
	}{
		{"claude-code", Claude{MaxBudget: "0.05"}},
		{"codex", Codex{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := open(t)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			p := Params{SessionID: "s1", RunID: "r1", OpID: "op1", Dir: t.TempDir(), Prompt: "Reply with exactly the word: pong"}
			got, err := Create(ctx, db, c.h, p)
			if err != nil || got != outcome.Completed {
				t.Fatalf("outcome=%s err=%v, want completed", got, err)
			}
			id, err := db.NativeID(ctx, 1)
			if err != nil || id == "" {
				t.Fatalf("native id not recorded: id=%q err=%v", id, err)
			}
			if s := storedOutcome(t, db); s != string(outcome.Completed) {
				t.Fatalf("stored outcome %q, want completed", s)
			}
			resumed, err := Resume(ctx, db, c.h, ResumeParams{
				SessionID: p.SessionID, RunID: "r2", OpID: "op2", Prompt: "Reply with exactly the word: pong2",
			})
			if err != nil || resumed != outcome.Completed {
				t.Fatalf("resume outcome=%s err=%v, want completed", resumed, err)
			}
			active, err := db.ActiveBinding(ctx, p.SessionID)
			if err != nil || active.NativeID != id || active.WorkingDir != p.Dir {
				t.Fatalf("active binding after resume = %+v err=%v, want same native id and working directory", active, err)
			}
			r, ok, err := db.RunForOp(ctx, "op2")
			if err != nil || !ok || r.Outcome.String != string(outcome.Completed) {
				t.Fatalf("resumed run: ok=%v outcome=%q err=%v, want completed", ok, r.Outcome.String, err)
			}
		})
	}
}

// TestRealResumeAfterNeedsInputCarriesNativeTaskContext checks whether the same native Session
// supplies the prior task details to a new Run after the first Run asks the user for missing data.
// Workstave has no persisted Task entity yet, so this observes native-session continuity only.
func TestRealResumeAfterNeedsInputCarriesNativeTaskContext(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run the billable needs-input resume experiment")
	}
	cases := []struct {
		name    string
		harness Harness
	}{
		{"claude-code", Claude{MaxBudget: "0.05", Model: "claude-haiku-4-5-20251001"}},
		{"codex", Codex{}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			db := open(t)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			dir := t.TempDir()
			params := Params{
				SessionID: "s1", RunID: "r-needs-input", OpID: "op-needs-input", Dir: dir,
				Prompt: "We need the arithmetic mean error rate for services Alpha and Beta. Alpha's error rate is 7.4%. Beta's rate is missing. Do not guess. Reply exactly NEEDS_INPUT: Beta error rate.",
			}
			first, err := CreateWithOutput(ctx, db, test.harness, params)
			if err != nil || first.Outcome != outcome.Completed || !strings.Contains(strings.ToLower(first.Text), "needs_input") {
				t.Fatalf("first Run did not complete with a needs-input request (outcome=%s, error-present=%t, needs-input-present=%t)", first.Outcome, err != nil, strings.Contains(strings.ToLower(first.Text), "needs_input"))
			}

			second, err := ResumeWithOutput(ctx, db, test.harness, ResumeParams{
				SessionID: params.SessionID, RunID: "r-after-needs-input", OpID: "op-after-needs-input",
				Prompt: "Beta's error rate is 3.6%. Give the Alpha rate, Beta rate, and arithmetic mean.",
			})
			if err != nil || second.Outcome != outcome.Completed {
				t.Fatalf("follow-up Run outcome=%s error-present=%t, want completed", second.Outcome, err != nil)
			}
			answer := strings.ToLower(second.Text)
			if !strings.Contains(answer, "7.4") || !strings.Contains(answer, "3.6") || !strings.Contains(answer, "5.5") {
				t.Fatalf("follow-up Run did not use the previous task context: alpha_present=%t beta_present=%t mean_present=%t", strings.Contains(answer, "7.4"), strings.Contains(answer, "3.6"), strings.Contains(answer, "5.5"))
			}
		})
	}
}

func TestRealCodexStructuredOutputSchema(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run the Codex structured-output probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schema := `{"type":"object","additionalProperties":false,"properties":{"reply":{"type":"string"},"changes":{"type":"array","items":{"type":"string"}}},"required":["reply","changes"]}`
	var acknowledgedID string
	execution, err := (Codex{}).Execute(ctx, Request{
		Dir: t.TempDir(), Prompt: "Return reply exactly PONG and changes as an empty array.", OutputSchema: schema,
	}, func(id string) { acknowledgedID = id })
	if err != nil || execution.Outcome != outcome.Completed {
		t.Fatalf("Codex structured-output Run outcome=%s error-present=%t", execution.Outcome, err != nil)
	}
	var response struct {
		Reply   string   `json:"reply"`
		Changes []string `json:"changes"`
	}
	if err := json.Unmarshal([]byte(execution.Text), &response); err != nil {
		t.Fatalf("Codex structured-output response is not JSON: %v", err)
	}
	if response.Reply != "PONG" || response.Changes == nil || len(response.Changes) != 0 || acknowledgedID == "" {
		t.Fatalf("Codex structured-output response=%+v correlated_thread_present=%t", response, acknowledgedID != "")
	}
}

// TestRealSafePointCrossHarnessHandoff is a deliberately minimal real-harness experiment. The
// operator summary is supplied explicitly because Workstave does not yet persist a Work goal,
// constraints, or an event cursor from which to compile one.
func TestRealSafePointCrossHarnessHandoff(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run the billable cross-harness handoff experiment")
	}
	db := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dir := t.TempDir()
	workID, sessionID := "w1", "s1"

	firstOutcome, err := Create(ctx, db, Claude{MaxBudget: "0.05", Model: "claude-haiku-4-5-20251001"}, Params{
		SessionID: sessionID, RunID: "r-handoff-claude", OpID: "op-handoff-claude", Dir: dir,
		Prompt: "Reach this safe point and reply with exactly HANDOFF_READY. Do not use tools or perform external actions.",
	})
	if err != nil || firstOutcome != outcome.Completed {
		t.Fatalf("Claude source run did not complete (outcome=%s, error-present=%t)", firstOutcome, err != nil)
	}
	firstRun, ok, err := db.RunForOp(ctx, "op-handoff-claude")
	if err != nil || !ok || firstRun.Outcome.String != string(outcome.Completed) {
		t.Fatalf("Claude source Run is not persisted as completed (found=%t, error-present=%t)", ok, err != nil)
	}
	firstBinding, err := db.ActiveBinding(ctx, sessionID)
	if err != nil || firstBinding.Harness != "claude-code" || firstBinding.NativeID == "" {
		t.Fatalf("Claude source binding is unavailable (found-harness=%t, native-id-present=%t)", firstBinding.Harness == "claude-code", firstBinding.NativeID != "")
	}
	if firstRun.BindingID.Int64 != firstBinding.ID {
		t.Fatal("Claude source Run does not refer to the active binding")
	}

	// This explicit receipt record uses persisted identities and the observed terminal outcome.
	// The store does not yet contain a Work goal or constraints from which to build a fuller handoff.
	handoff := fmt.Sprintf("Safe-point receipt. Work %s. Logical Session %s. Previous Run %s completed. Reply exactly HANDOFF_ACCEPTED.", workID, sessionID, firstRun.ID)
	// Disable web search and MCP to prevent external tool calls during handoff verification.
	secondOutcome, err := Create(ctx, db, Codex{ConfigOverrides: []string{"features.web_search=false", "features.mcp=false"}}, Params{
		SessionID: sessionID, RunID: "r-handoff-codex", OpID: "op-handoff-codex", Dir: dir,
		Prompt: handoff,
	})
	if err != nil || secondOutcome != outcome.Completed {
		t.Fatalf("Codex candidate run did not complete (outcome=%s, error-present=%t)", secondOutcome, err != nil)
	}
	secondRun, ok, err := db.RunForOp(ctx, "op-handoff-codex")
	if err != nil || !ok || secondRun.Outcome.String != string(outcome.Completed) {
		t.Fatalf("Codex candidate Run is not persisted as completed (found=%t, error-present=%t)", ok, err != nil)
	}
	secondBinding, err := db.ActiveBinding(ctx, sessionID)
	if err != nil || secondBinding.Harness != "codex" || secondBinding.NativeID == "" {
		t.Fatalf("Codex candidate binding is unavailable (found-harness=%t, native-id-present=%t)", secondBinding.Harness == "codex", secondBinding.NativeID != "")
	}
	if secondBinding.ID == firstBinding.ID || secondRun.BindingID.Int64 != secondBinding.ID {
		t.Fatal("Codex candidate did not replace the active Native Binding for the same logical Session")
	}
	if secondBinding.WorkingDir != dir || firstBinding.WorkingDir != dir || secondRun.SessionID != firstRun.SessionID {
		t.Fatal("cross-harness handoff changed the logical Session or working directory")
	}
	if prior, ok, err := db.RunForOp(ctx, "op-handoff-claude"); err != nil || !ok || prior.Outcome.String != string(outcome.Completed) {
		t.Fatalf("Claude source Run history was not preserved (found=%t, error-present=%t)", ok, err != nil)
	}
}
