package codex

import (
	"testing"

	"workstave/internal/outcome"
)

// These tests check the classification rules on hand-built results; they are not evidence
// about the real harness.
func TestOutcome(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		want outcome.Outcome
	}{
		{"completed", Result{Completed: true}, outcome.Completed},
		{"failed", Result{Failed: true, ExitCode: 1}, outcome.Failed},
		{"config warning items only, killed", Result{Events: []Event{{Type: "item.completed", ItemType: "error"}}, ExitCode: 1}, outcome.Unknown},
		{"nothing observed", Result{ExitCode: 1}, outcome.Unknown},
	}
	for _, c := range cases {
		if got := c.res.Outcome(); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
