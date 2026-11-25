// Package runner starts the first run of a logical session on a harness and keeps the store
// consistent with what was observed. The order is fixed: the binding, the operation id and the
// run are persisted before any external process is started; the outcome is recorded from
// terminal events only; a run that could not be observed to its end is recorded as unknown.
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
}

// Harness is what Create needs from a harness. Both existing drivers differ on exactly this
// point: Claude Code accepts an id chosen before launch, Codex reports its id after creation.
type Harness interface {
	Name() string
	// NewNativeID returns an id to pre-assign, or "" if the harness assigns its own.
	NewNativeID() string
	// Execute runs the prompt and reports the observed outcome. onNativeID must be called as
	// soon as a harness-assigned id is seen.
	Execute(ctx context.Context, req Request, onNativeID func(id string)) (outcome.Outcome, error)
}

// Params identifies one first run. OpID is the stable operation identity of this request.
type Params struct {
	SessionID string
	RunID     string
	OpID      string
	Dir       string
	Prompt    string
}

// Create binds the session to the harness, records the run, executes it and records the
// outcome. If OpID already has a run, nothing is touched or launched and an error is returned.
// The outcome is recorded even when ctx was cancelled during execution. Resuming an existing
// native session is not handled here yet.
func Create(ctx context.Context, db *store.DB, h Harness, p Params) (outcome.Outcome, error) {
	if existing, ok, err := db.RunForOp(ctx, p.OpID); err != nil {
		return "", err
	} else if ok {
		return "", fmt.Errorf("runner: op %q already has run %q; not launching again", p.OpID, existing.ID)
	}
	bindingID, err := db.BindNative(ctx, p.SessionID, h.Name(), p.Dir)
	if err != nil {
		return "", err
	}
	nativeID := h.NewNativeID()
	if nativeID != "" {
		if err := db.SetNativeID(ctx, bindingID, nativeID, ""); err != nil {
			return "", err
		}
	}
	if _, _, err := db.BeginRun(ctx, p.RunID, p.SessionID, bindingID, p.OpID); err != nil {
		return "", err
	}

	var idErr error
	out, runErr := h.Execute(ctx, Request{Dir: p.Dir, NativeID: nativeID, Prompt: p.Prompt}, func(id string) {
		if err := db.SetNativeID(ctx, bindingID, id, ""); err != nil && idErr == nil {
			idErr = err
		}
	})
	if runErr != nil || out == "" {
		out = outcome.Unknown
	}
	if err := db.FinishRun(context.WithoutCancel(ctx), p.RunID, out); err != nil {
		return out, errors.Join(runErr, idErr, err)
	}
	return out, errors.Join(runErr, idErr)
}
