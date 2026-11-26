// Package runner creates or resumes native sessions and keeps the store consistent with what
// was observed. The binding, operation id and run are persisted before an external process is
// started; outcomes come from terminal events only, and an unobserved end is recorded unknown.
package runner

import (
	"context"
	"errors"
	"fmt"

	"workstave/internal/outcome"
	"workstave/internal/store"
)

// Request is one prompt for one native session.
type Request struct {
	Dir      string
	NativeID string // set when the caller pre-assigned it; empty when the harness creates it
	Prompt   string
	Resume   bool
}

// Harness is the execution surface needed by create and resume. Claude Code accepts an id
// before launch; Codex reports its id after creation.
type Harness interface {
	Name() string
	// NewNativeID returns an id to pre-assign, or "" if the harness assigns its own.
	NewNativeID() string
	// Execute runs the prompt and reports the observed outcome. onAcknowledged must be called
	// when the harness reports a correlatable session acknowledgement and its native id.
	Execute(ctx context.Context, req Request, onAcknowledged func(id string)) (outcome.Outcome, error)
}

// Params identifies one first run. OpID is the stable operation identity of this request.
type Params struct {
	SessionID string
	RunID     string
	OpID      string
	Dir       string
	Prompt    string
}

// ResumeParams identifies a new run on an existing native session.
type ResumeParams struct {
	SessionID string
	RunID     string
	OpID      string
	Prompt    string
}

// Create binds the session to the harness, records the run, executes it and records the
// outcome. If OpID already has a run, nothing is touched or launched and an error is returned.
// The outcome is recorded even when ctx was cancelled during execution.
func Create(ctx context.Context, db *store.DB, h Harness, p Params) (outcome.Outcome, error) {
	// One transaction records the binding, the pre-assigned native id and the run. Nothing is
	// launched until it has committed; a repeated op id writes nothing and launches nothing.
	nativeID := h.NewNativeID()
	bindingID, run, created, err := db.BeginFirstRun(ctx, p.RunID, p.SessionID, h.Name(), p.Dir, nativeID, p.OpID)
	if err != nil {
		return "", err
	}
	if !created {
		return "", fmt.Errorf("runner: op %q already has run %q; not launching again", p.OpID, run.ID)
	}
	// Pause and dispatch authorization share a SQLite commit order. No external call is allowed
	// unless this transaction commits with the work unpaused.
	if err := db.AuthorizeDispatch(ctx, p.RunID); err != nil {
		return "", err
	}
	return executeAuthorized(ctx, db, h, bindingID, p.RunID, Request{Dir: p.Dir, NativeID: nativeID, Prompt: p.Prompt})
}

// Resume starts a new run on the active native binding of the same logical session. It uses
// the persisted harness, native id and working directory and never creates a new binding.
func Resume(ctx context.Context, db *store.DB, h Harness, p ResumeParams) (outcome.Outcome, error) {
	binding, err := db.ActiveBinding(ctx, p.SessionID)
	if err != nil {
		return "", err
	}
	if binding.Harness != h.Name() {
		return "", fmt.Errorf("runner: session %q is bound to %q, not %q", p.SessionID, binding.Harness, h.Name())
	}
	if binding.NativeID == "" {
		return "", fmt.Errorf("runner: session %q has no recorded native id; cannot resume", p.SessionID)
	}

	run, created, err := db.BeginRun(ctx, p.RunID, p.SessionID, binding.ID, p.OpID)
	if err != nil {
		return "", err
	}
	if !created {
		return "", fmt.Errorf("runner: op %q already has run %q; not launching again", p.OpID, run.ID)
	}
	if err := db.AuthorizeDispatch(ctx, run.ID); err != nil {
		return "", err
	}
	return executeAuthorized(ctx, db, h, binding.ID, run.ID, Request{
		Dir: binding.WorkingDir, NativeID: binding.NativeID, Prompt: p.Prompt, Resume: true,
	})
}

func executeAuthorized(ctx context.Context, db *store.DB, h Harness, bindingID int64, runID string, req Request) (outcome.Outcome, error) {
	var idErr error
	recordCtx := context.WithoutCancel(ctx)
	out, runErr := h.Execute(ctx, req, func(id string) {
		if idErr != nil {
			return
		}
		if id == "" {
			idErr = fmt.Errorf("runner: harness acknowledged an empty native session id")
			return
		}
		if req.NativeID == "" {
			if err := db.SetNativeID(recordCtx, bindingID, id, ""); err != nil {
				idErr = err
				return
			}
		} else if id != req.NativeID {
			idErr = fmt.Errorf("runner: harness acknowledged native session %q, want %q", id, req.NativeID)
			return
		}
		if err := db.MarkAcknowledged(recordCtx, runID); err != nil {
			idErr = err
		}
	})
	if runErr != nil || out == "" {
		out = outcome.Unknown
	}
	if err := db.FinishRun(recordCtx, runID, out); err != nil {
		return out, errors.Join(runErr, idErr, err)
	}
	return out, errors.Join(runErr, idErr)
}
