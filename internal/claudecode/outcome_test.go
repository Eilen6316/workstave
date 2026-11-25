package claudecode

import (
	"testing"

	"workstave/internal/outcome"
)

// These tests classify hand-built event lists. They check the classification rules only;
// they are not evidence about the real harness.
func TestOutcome(t *testing.T) {
	init := Event{Type: "system", Subtype: "init"}
	assistant := Event{Type: "assistant"}
	cases := []struct {
		name   string
		events []Event
		exit   int
		want   outcome.Outcome
	}{
		{"success", []Event{init, assistant, {Type: "result", Subtype: "success"}}, 0, outcome.Completed},
		{"budget exhausted", []Event{init, assistant, {Type: "result", Subtype: "error_max_budget_usd"}}, 1, outcome.Failed},
		{"success flagged as error", []Event{init, {Type: "result", Subtype: "success", IsError: true}}, 0, outcome.Failed},
		{"killed, no result", []Event{init, assistant, assistant}, 1, outcome.Unknown},
		{"no events", nil, 1, outcome.Unknown},
	}
	for _, c := range cases {
		if got := (Result{Events: c.events, ExitCode: c.exit}).Outcome(); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
