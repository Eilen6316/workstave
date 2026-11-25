package claudecode

import "workstave/internal/outcome"

// Outcome classifies the invocation from its terminal result event. A result event with
// subtype success and no error is Completed, any other result event is Failed, and a stream
// without a result event is Unknown.
func (r Result) Outcome() outcome.Outcome {
	for i := len(r.Events) - 1; i >= 0; i-- {
		ev := r.Events[i]
		if ev.Type != "result" {
			continue
		}
		if ev.Subtype == "success" && !ev.IsError {
			return outcome.Completed
		}
		return outcome.Failed
	}
	return outcome.Unknown
}
