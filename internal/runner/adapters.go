package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"

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

func (c Claude) Execute(ctx context.Context, req Request, onAcknowledged func(string)) (Execution, error) {
	res, err := claudecode.Run(ctx, claudecode.Options{
		Dir: req.Dir, SessionID: req.NativeID, Resume: req.Resume, Prompt: req.Prompt, MaxBudget: c.MaxBudget, Model: c.Model, JSONSchema: req.OutputSchema,
		OnEvent: func(ev claudecode.Event) {
			if ev.Type == "system" && ev.Subtype == "init" {
				onAcknowledged(ev.SessionID)
			}
		},
	})
	var text string
	for _, ev := range res.Events {
		if ev.Type == "result" {
			text = ev.Result
		}
	}
	return Execution{Outcome: res.Outcome(), Text: text}, err
}

// Codex adapts the Codex exec driver. Codex reports its thread id only after creation.
type Codex struct {
	Model           string
	ConfigOverrides []string // passed via -c to override config.toml settings
}

func (Codex) Name() string { return "codex" }

func (Codex) NewNativeID() string { return "" }

func (c Codex) Execute(ctx context.Context, req Request, onAcknowledged func(string)) (Execution, error) {
	threadID := ""
	if req.Resume {
		threadID = req.NativeID
	}
	schemaPath := ""
	if req.OutputSchema != "" {
		schemaFile, err := os.CreateTemp("", "workstave-codex-schema-*.json")
		if err != nil {
			return Execution{}, fmt.Errorf("runner: create Codex output schema file: %w", err)
		}
		schemaPath = schemaFile.Name()
		if _, err := schemaFile.WriteString(req.OutputSchema); err != nil {
			closeErr := schemaFile.Close()
			removeErr := os.Remove(schemaPath)
			return Execution{}, errors.Join(fmt.Errorf("runner: write Codex output schema: %w", err), closeErr, removeErr)
		}
		if err := schemaFile.Close(); err != nil {
			removeErr := os.Remove(schemaPath)
			return Execution{}, errors.Join(fmt.Errorf("runner: close Codex output schema: %w", err), removeErr)
		}
	}
	res, err := codex.Run(ctx, codex.Options{
		Dir:              req.Dir,
		ThreadID:         threadID,
		Model:            c.Model,
		ConfigOverrides:  c.ConfigOverrides,
		Prompt:           req.Prompt,
		OutputSchemaPath: schemaPath,
		OnEvent: func(ev codex.Event) {
			if ev.Type == "thread.started" {
				onAcknowledged(ev.ThreadID)
			}
		},
	})
	if schemaPath != "" {
		if removeErr := os.Remove(schemaPath); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("runner: remove Codex output schema file: %w", removeErr))
		}
	}
	var text string
	for _, ev := range res.Events {
		if (ev.ItemType == "agentMessage" || ev.ItemType == "agent_message") && ev.Text != "" {
			text = ev.Text
		}
	}
	execution := Execution{Outcome: res.Outcome(), Text: text}
	if err != nil && res.Stderr != "" {
		err = fmt.Errorf("%w; codex stderr: %s", err, codex.Redact([]byte(res.Stderr)))
	} else if execution.Outcome == outcome.Failed {
		msg := res.FailMessage
		if msg == "" {
			msg = res.Stderr
		}
		if msg != "" {
			err = fmt.Errorf("codex turn failed: %s", codex.Redact([]byte(msg)))
		}
	}
	return execution, err
}
