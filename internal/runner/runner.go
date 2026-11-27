// Package runner creates and resumes native sessions while persisting observed outcomes.
package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"workstave/internal/outcome"
	"workstave/internal/store"
)

var outputSecret = regexp.MustCompile(`sk-[A-Za-z0-9*_-]{6,}`)

// Request is one prompt for one native session.
type Request struct {
	Dir      string
	NativeID string
	Prompt   string
	Resume   bool
}

// Execution contains the terminal outcome and final assistant text observed from the Harness.
type Execution struct {
	Outcome outcome.Outcome
	Text    string
}

// Harness is the execution surface needed by create and resume.
type Harness interface {
	Name() string
	NewNativeID() string
	Execute(ctx context.Context, req Request, onAcknowledged func(id string)) (Execution, error)
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

// MainParams associates a queued Conversation input with the Work's logical Main Run.
type MainParams struct {
	WorkID          string
	ConversationID  string
	MailboxSequence int64
	MessageID       string
	ReplyID         string
	RunID           string
	OpID            string
	WorkingDir      string
	InputSnapshot   string
	ContextRevision int64
	Prompt          string
}

// Create binds a session to a Harness, records a Run, executes it and returns its outcome.
func Create(ctx context.Context, db *store.DB, h Harness, p Params) (outcome.Outcome, error) {
	execution, err := CreateWithOutput(ctx, db, h, p)
	return execution.Outcome, err
}

// CreateWithOutput is Create with the final assistant text observed on the Harness stream.
func CreateWithOutput(ctx context.Context, db *store.DB, h Harness, p Params) (Execution, error) {
	nativeID := h.NewNativeID()
	bindingID, run, created, err := db.BeginFirstRun(ctx, p.RunID, p.SessionID, h.Name(), p.Dir, nativeID, p.OpID)
	if err != nil {
		return Execution{}, err
	}
	if !created {
		return Execution{}, fmt.Errorf("runner: op %q already has run %q; not launching again", p.OpID, run.ID)
	}
	if err := db.AuthorizeDispatch(ctx, p.RunID); err != nil {
		return Execution{}, err
	}
	return executeAuthorized(ctx, db, h, bindingID, p.RunID, Request{Dir: p.Dir, NativeID: nativeID, Prompt: p.Prompt})
}

// Resume starts a new Run on the active native binding of the same logical session.
func Resume(ctx context.Context, db *store.DB, h Harness, p ResumeParams) (outcome.Outcome, error) {
	execution, err := ResumeWithOutput(ctx, db, h, p)
	return execution.Outcome, err
}

// ResumeWithOutput is Resume with the final assistant text observed on the Harness stream.
func ResumeWithOutput(ctx context.Context, db *store.DB, h Harness, p ResumeParams) (Execution, error) {
	binding, err := db.ActiveBinding(ctx, p.SessionID)
	if err != nil {
		return Execution{}, err
	}
	if binding.Harness != h.Name() {
		return Execution{}, fmt.Errorf("runner: session %q is bound to %q, not %q", p.SessionID, binding.Harness, h.Name())
	}
	if binding.NativeID == "" {
		return Execution{}, fmt.Errorf("runner: session %q has no recorded native id; cannot resume", p.SessionID)
	}
	run, created, err := db.BeginRun(ctx, p.RunID, p.SessionID, binding.ID, p.OpID)
	if err != nil {
		return Execution{}, err
	}
	if !created {
		return Execution{}, fmt.Errorf("runner: op %q already has run %q; not launching again", p.OpID, run.ID)
	}
	if err := db.AuthorizeDispatch(ctx, run.ID); err != nil {
		return Execution{}, err
	}
	return executeAuthorized(ctx, db, h, binding.ID, run.ID, Request{
		Dir: binding.WorkingDir, NativeID: binding.NativeID, Prompt: p.Prompt, Resume: true,
	})
}

// ProcessMainMessage atomically authorizes a Mailbox input, runs it on the Work's logical
// Main session, and finalizes the Run, reply and Mailbox record in one completion transaction.
func ProcessMainMessage(ctx context.Context, db *store.DB, h Harness, p MainParams) (Execution, error) {
	work, err := db.Work(ctx, p.WorkID)
	if err != nil {
		return Execution{}, err
	}
	binding, err := db.ActiveBinding(ctx, work.MainSessionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Execution{}, err
	}
	start := store.MainRunStart{
		RunID: p.RunID, OpID: p.OpID, WorkID: p.WorkID, MainSessionID: work.MainSessionID,
		ConversationID: p.ConversationID, MailboxSequence: p.MailboxSequence, SourceMessageID: p.MessageID,
		Harness: h.Name(), WorkingDir: p.WorkingDir, NativeID: h.NewNativeID(),
		InputSnapshot: p.InputSnapshot, ContextRevision: p.ContextRevision,
	}
	req := Request{Dir: p.WorkingDir, NativeID: start.NativeID, Prompt: p.Prompt}
	if err == nil {
		if binding.Harness == h.Name() {
			if binding.NativeID == "" {
				return Execution{}, fmt.Errorf("runner: Main session %q has no recorded native id", work.MainSessionID)
			}
			start.BindingID = binding.ID
			start.NativeID = binding.NativeID
			req = Request{Dir: binding.WorkingDir, NativeID: binding.NativeID, Prompt: p.Prompt, Resume: true}
		}
	}
	bindingID, err := db.BeginMainRun(ctx, start)
	if err != nil {
		return Execution{}, err
	}
	execution, runErr := runHarness(ctx, db, h, bindingID, p.RunID, req)
	if runErr != nil || execution.Outcome == "" {
		execution.Outcome = outcome.Unknown
	}
	finishErr := db.CompleteMainRun(context.WithoutCancel(ctx), p.RunID, p.ReplyID, execution.Text, execution.Outcome)
	if finishErr != nil {
		return Execution{Outcome: outcome.Unknown}, errors.Join(runErr, finishErr)
	}
	return execution, runErr
}

func executeAuthorized(ctx context.Context, db *store.DB, h Harness, bindingID int64, runID string, req Request) (Execution, error) {
	execution, runErr := runHarness(ctx, db, h, bindingID, runID, req)
	if runErr != nil || execution.Outcome == "" {
		execution.Outcome = outcome.Unknown
	}
	if err := db.FinishRun(context.WithoutCancel(ctx), runID, execution.Outcome); err != nil {
		return Execution{Outcome: outcome.Unknown}, errors.Join(runErr, err)
	}
	return execution, runErr
}

func runHarness(ctx context.Context, db *store.DB, h Harness, bindingID int64, runID string, req Request) (Execution, error) {
	var idErr error
	recordCtx := context.WithoutCancel(ctx)
	execution, runErr := h.Execute(ctx, req, func(id string) {
		if idErr != nil {
			return
		}
		if id == "" {
			idErr = fmt.Errorf("runner: Harness acknowledged an empty native session id")
			return
		}
		if req.NativeID == "" {
			if err := db.SetNativeID(recordCtx, bindingID, id, ""); err != nil {
				idErr = err
				return
			}
		} else if id != req.NativeID {
			idErr = fmt.Errorf("runner: Harness acknowledged native session %q, want %q", id, req.NativeID)
			return
		}
		if err := db.MarkAcknowledged(recordCtx, runID); err != nil {
			idErr = err
		}
	})
	execution.Text = outputSecret.ReplaceAllString(execution.Text, "sk-<redacted>")
	if runErr != nil || idErr != nil || execution.Outcome == "" {
		execution.Outcome = outcome.Unknown
	}
	return execution, errors.Join(runErr, idErr)
}
