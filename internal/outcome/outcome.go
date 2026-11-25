// Package outcome classifies a finished harness invocation by what was observed on the wire.
// Exit codes are not used: Claude Code exits 1 on budget exhaustion with a result event, and a
// killed process of either harness also exits 1 but produces no terminal event at all.
// Unknown means the run produced no terminal event; it must never be recorded as success
// or failure, and external side effects of an Unknown run must be reconciled, not retried.
package outcome

// Outcome is the observed end state of one invocation.
type Outcome string

const (
	Completed Outcome = "completed"
	Failed    Outcome = "failed"
	Unknown   Outcome = "unknown"
)
