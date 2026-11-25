package codex

import "workstave/internal/outcome"

// Outcome classifies the invocation from terminal events only: turn.failed is Failed,
// turn.completed is Completed, and a stream with neither is Unknown. Item-level error events
// and the exit code are ignored; a killed run exits 1 without any terminal event.
func (r Result) Outcome() outcome.Outcome {
	switch {
	case r.Failed:
		return outcome.Failed
	case r.Completed:
		return outcome.Completed
	default:
		return outcome.Unknown
	}
}
