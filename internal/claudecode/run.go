// Package claudecode drives the Claude Code CLI in print mode with stream-json output.
// It reports what was observed on the wire; it does not infer success from exit codes alone.
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"workstave/internal/procgroup"
)

// Event is one JSONL line from stdout. Raw keeps the original bytes for evidence.
type Event struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	IsError   bool            `json:"is_error"`
	SessionID string          `json:"session_id"`
	Result    string          `json:"result"`
	Raw       json.RawMessage `json:"-"`
}

// Options describes one print-mode invocation.
type Options struct {
	Bin       string // executable name or path; defaults to "claude"
	Dir       string // working directory; also decides where the session file is persisted
	SessionID string // UUID assigned by the caller before launch
	Resume    bool   // resume SessionID instead of creating it
	Model     string // optional model override for this invocation
	Prompt    string // sent on stdin
	MaxBudget string // USD cap, e.g. "0.05"
	// Tools is the built-in tool set passed to --tools; empty means none.
	Tools string
	// AllowedTools is passed to --allowedTools when non-empty (e.g. "Bash(ping *)").
	AllowedTools string
	// JSONSchema, when non-empty, is passed to --json-schema so the final answer is returned as
	// schema-validated structured output instead of free text.
	JSONSchema string
	// OnEvent, if set, is called synchronously for each parsed event, in order, on the
	// goroutine that called Run. It lets the caller cancel the context mid-run.
	OnEvent func(Event)
	// OnProcess, if set, is called once with the OS pid right after the process starts.
	OnProcess func(pid int)
}

// Result is what one invocation actually produced.
type Result struct {
	Events   []Event
	ExitCode int
	Stderr   string
}

func buildArgs(o Options) []string {
	a := []string{"-p", "--output-format", "stream-json", "--verbose"}
	// Use the equals form --tools= for an empty set so cmd.exe does not drop the token: a bare
	// empty-string argument is silently discarded by the .cmd wrapper, shifting --strict-mcp-config
	// into the value slot of --tools and disabling it.
	if o.Tools != "" {
		a = append(a, "--tools", o.Tools)
	} else {
		a = append(a, "--tools=")
	}
	a = append(a, "--strict-mcp-config", "--permission-mode", "dontAsk")
	if o.AllowedTools != "" {
		a = append(a, "--allowedTools", o.AllowedTools)
	}
	if o.JSONSchema != "" {
		a = append(a, "--json-schema", o.JSONSchema)
	}
	if o.MaxBudget != "" {
		a = append(a, "--max-budget-usd", o.MaxBudget)
	}
	if o.Model != "" {
		a = append(a, "--model", o.Model)
	}
	if o.Resume {
		return append(a, "--resume", o.SessionID)
	}
	return append(a, "--session-id", o.SessionID)
}

// Run starts the CLI, reads every event until stdout closes, and waits for the process.
// A non-zero exit is reported in Result.ExitCode, not as an error; unparsable lines are errors.
func Run(ctx context.Context, o Options) (Result, error) {
	bin := o.Bin
	if bin == "" {
		bin = "claude"
	}
	cmd := exec.CommandContext(ctx, bin, buildArgs(o)...)
	cmd.Dir = o.Dir
	procgroup.Configure(cmd)
	// Context cancellation kills the whole process tree, not just the direct child.
	cmd.Cancel = func() error { return procgroup.Kill(cmd.Process.Pid) }
	cmd.Stdin = strings.NewReader(o.Prompt)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("claudecode: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("claudecode: start %q: %w", bin, err)
	}
	// On Windows the tree is contained in a Job Object so an owner crash cannot leave it running.
	if err := procgroup.Attach(cmd.Process.Pid); err != nil {
		_ = procgroup.Kill(cmd.Process.Pid)
		_ = cmd.Wait()
		return Result{}, fmt.Errorf("claudecode: contain process tree: %w", err)
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
		line := append([]byte(nil), sc.Bytes()...)
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			parseErrs = append(parseErrs, fmt.Errorf("claudecode: line %d: %w", len(res.Events)+len(parseErrs)+1, err))
			continue
		}
		ev.Raw = line
		res.Events = append(res.Events, ev)
		if o.OnEvent != nil {
			o.OnEvent(ev)
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	res.Stderr = stderr.String()

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("claudecode: wait: %w", waitErr)
	}
	if scanErr != nil {
		parseErrs = append(parseErrs, fmt.Errorf("claudecode: read stdout: %w", scanErr))
	}
	return res, errors.Join(parseErrs...)
}
