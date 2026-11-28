// Package codex drives `codex exec --json` and reports what was observed on the wire.
// A failed turn is determined by a turn.failed event, never by item-level error events,
// which Codex also uses for non-fatal configuration warnings.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"workstave/internal/procgroup"
)

// Event is one JSONL line from stdout. Raw is redacted before it is stored.
type Event struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	ItemType string          `json:"-"`
	Text     string          `json:"-"`
	Raw      json.RawMessage `json:"-"`
}

// Options describes one invocation.
type Options struct {
	Bin              string // executable; defaults to "codex"
	Dir              string // working root passed via -C and used as the process directory
	ThreadID         string // when set, resume this thread instead of creating one
	Model            string // optional model override for this invocation
	Prompt           string // sent on stdin
	Sandbox          string // defaults to read-only
	OutputSchemaPath string // optional JSON Schema file passed via --output-schema
	// ConfigOverrides are passed via -c key=value to override config.toml settings.
	// Use to disable features or restrict permissions, e.g. "features.web_search=false".
	ConfigOverrides []string
	// OnEvent, if set, is called synchronously for each parsed event, in order.
	OnEvent func(Event)
	// OnProcess, if set, is called once with the OS pid right after the process starts,
	// so the caller can own tree termination.
	OnProcess func(pid int)
}

// Result is what one invocation actually produced.
type Result struct {
	Events      []Event
	ThreadID    string // from thread.started; empty if the stream never produced one
	Failed      bool   // true only if a turn.failed event was observed
	FailMessage string // the error.message from the turn.failed event, if any
	Completed   bool   // true only if a turn.completed event was observed
	ExitCode    int
	Stderr      string
}

var secretPattern = regexp.MustCompile(`sk-[A-Za-z0-9*_\-]{6,}`)

// Redact masks API-key-like strings. Codex echoes partial keys inside error messages.
func Redact(b []byte) []byte {
	return secretPattern.ReplaceAll(b, []byte("sk-<redacted>"))
}

func buildArgs(o Options) []string {
	sandbox := o.Sandbox
	if sandbox == "" {
		sandbox = "read-only"
	}
	// `codex exec resume` accepts neither -s nor -C (verified against the real CLI), so a
	// resumed turn cannot be given a new sandbox or root; the process directory carries cwd.
	// Config overrides are allowed on resume.
	if o.ThreadID != "" {
		a := []string{"exec", "resume", o.ThreadID, "--json", "--skip-git-repo-check"}
		for _, override := range o.ConfigOverrides {
			a = append(a, "-c", override)
		}
		if o.Model != "" {
			a = append(a, "--model", o.Model)
		}
		if o.OutputSchemaPath != "" {
			a = append(a, "--output-schema", o.OutputSchemaPath)
		}
		// The prompt is read from stdin ("-"): a multi-line argument is cut at its first
		// newline by the npm .cmd wrapper on Windows, which silently drops the rest.
		return append(a, "-")
	}
	a := []string{"exec", "--json", "--skip-git-repo-check"}
	for _, override := range o.ConfigOverrides {
		a = append(a, "-c", override)
	}
	if o.Model != "" {
		a = append(a, "--model", o.Model)
	}
	if o.OutputSchemaPath != "" {
		a = append(a, "--output-schema", o.OutputSchemaPath)
	}
	a = append(a, "-s", sandbox, "-C", o.Dir, "-")
	return a
}

// Run starts codex, reads every event until stdout closes, and waits for the process.
// A non-zero exit is reported in Result.ExitCode; unparsable lines are returned as errors.
func Run(ctx context.Context, o Options) (Result, error) {
	bin := o.Bin
	if bin == "" {
		bin = "codex"
	}
	cmd := exec.CommandContext(ctx, bin, buildArgs(o)...)
	cmd.Dir = o.Dir
	cmd.Stdin = strings.NewReader(o.Prompt)
	procgroup.Configure(cmd)
	// Context cancellation kills the whole process tree, not just the direct child.
	cmd.Cancel = func() error { return procgroup.Kill(cmd.Process.Pid) }
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("codex: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("codex: start %q: %w", bin, err)
	}
	// On Windows the tree is contained in a Job Object so an owner crash cannot leave it running.
	if err := procgroup.Attach(cmd.Process.Pid); err != nil {
		_ = procgroup.Kill(cmd.Process.Pid)
		_ = cmd.Wait()
		return Result{}, fmt.Errorf("codex: contain process tree: %w", err)
	}
	defer procgroup.Release(cmd.Process.Pid)

	if o.OnProcess != nil {
		o.OnProcess(cmd.Process.Pid)
	}

	var res Result
	var parseErrs []error
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := Redact(append([]byte(nil), sc.Bytes()...))
		var env struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Error    struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			parseErrs = append(parseErrs, fmt.Errorf("codex: line %d: %w", len(res.Events)+len(parseErrs)+1, err))
			continue
		}
		ev := Event{Type: env.Type, ThreadID: env.ThreadID, ItemType: env.Item.Type, Text: env.Item.Text, Raw: line}
		switch ev.Type {
		case "thread.started":
			res.ThreadID = ev.ThreadID
		case "turn.failed":
			res.Failed = true
			res.FailMessage = env.Error.Message
		case "turn.completed":
			res.Completed = true
		}
		res.Events = append(res.Events, ev)
		if o.OnEvent != nil {
			o.OnEvent(ev)
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	res.Stderr = string(Redact([]byte(stderr.String())))

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("codex: wait: %w", waitErr)
	}
	if scanErr != nil {
		parseErrs = append(parseErrs, fmt.Errorf("codex: read stdout: %w", scanErr))
	}
	return res, errors.Join(parseErrs...)
}
