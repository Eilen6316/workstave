package runner

import (
	"context"
	"crypto/rand"
	"fmt"

	"workstave/internal/claudecode"
	"workstave/internal/codex"
	"workstave/internal/outcome"
)

// Claude adapts the Claude Code driver. It pre-assigns the session id before launch.
type Claude struct {
	MaxBudget string
	Model     string
}

func (Claude) Name() string { return "claude-code" }

func (Claude) NewNativeID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // documented to never return an error
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (c Claude) Execute(ctx context.Context, req Request, onAcknowledged func(string)) (outcome.Outcome, error) {
	res, err := claudecode.Run(ctx, claudecode.Options{
		Dir: req.Dir, SessionID: req.NativeID, Resume: req.Resume, Prompt: req.Prompt, MaxBudget: c.MaxBudget, Model: c.Model,
		OnEvent: func(ev claudecode.Event) {
			if ev.Type == "system" && ev.Subtype == "init" {
				onAcknowledged(ev.SessionID)
			}
		},
	})
	return res.Outcome(), err
}

// Codex adapts the Codex exec driver. Codex reports its thread id only after creation.
type Codex struct {
	Model string
}

func (Codex) Name() string { return "codex" }

func (Codex) NewNativeID() string { return "" }

func (c Codex) Execute(ctx context.Context, req Request, onAcknowledged func(string)) (outcome.Outcome, error) {
	threadID := ""
	if req.Resume {
		threadID = req.NativeID
	}
	res, err := codex.Run(ctx, codex.Options{
		Dir:      req.Dir,
		ThreadID: threadID,
		Model:    c.Model,
		Prompt:   req.Prompt,
		OnEvent: func(ev codex.Event) {
			if ev.Type == "thread.started" {
				onAcknowledged(ev.ThreadID)
			}
		},
	})
	return res.Outcome(), err
}
