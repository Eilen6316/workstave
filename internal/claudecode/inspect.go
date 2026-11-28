package claudecode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PromptReport describes the system prompt layers observed in one diagnostic invocation.
// It separates what the CLI assembles locally from what actually leaves the machine, so
// a gateway that modifies the request is detectable without relying on model cooperation.
type PromptReport struct {
	// CLIPrefix is the identity string the CLI binary hard-codes before the user system prompt.
	// This is the vendor (Anthropic) injection, present in every invocation regardless of config.
	CLIPrefix string
	// LocalSegments is the full system prompt array the CLI assembled before sending, taken from
	// the prompt_snapshot attachment. Includes CLIPrefix, user config, and any AppendSystemPrompt.
	LocalSegments []string
	// WireSegments is the system prompt array recorded in the api-request-shape, which is what
	// the CLI signed and sent over the network. If a gateway modifies the request, WireSegments
	// will differ from LocalSegments.
	WireSegments []string
	// RequestDigest is the CLI's own hash of the outbound request. Use it as a stable baseline:
	// the same prompt+config always produces the same digest; a gateway that alters the payload
	// will change the digest the next time the model returns a different response pattern.
	RequestDigest string
	// ServedByAnthropic is false when the CLI detected a non-Anthropic endpoint (ANTHROPIC_BASE_URL
	// points elsewhere). It does not by itself mean injection occurred.
	ServedByAnthropic bool
	// GatewayURL is the value of ANTHROPIC_BASE_URL at call time, or empty for the default endpoint.
	GatewayURL string
	// InjectionDetected is true when WireSegments and LocalSegments differ in content, indicating
	// the gateway modified the system prompt before forwarding it to the model.
	InjectionDetected bool
}

// Inspect runs one minimal diagnostic invocation and reads the session JSONL it writes to disk.
// It returns a PromptReport that separates the CLI's own injections from any gateway-added content.
// The invocation uses the caller-supplied Dir and a fresh session ID; it never resumes.
// The diagnostic prompt is a no-op request that costs roughly one input token.
func Inspect(ctx context.Context, dir string, maxBudget string) (PromptReport, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	if maxBudget == "" {
		maxBudget = "0.02"
	}
	sessionID, err := newInspectID()
	if err != nil {
		return PromptReport{}, fmt.Errorf("claudecode: inspect: generate session id: %w", err)
	}
	// --system-prompt-snapshot requires --append-system-prompt to be non-empty.
	// A single space is the smallest legal non-empty value that adds no semantic content.
	_, runErr := Run(ctx, Options{
		Dir:                 dir,
		SessionID:           sessionID,
		Prompt:              `{"reply":"ok","changes":[]}`,
		MaxBudget:           maxBudget,
		CaptureSystemPrompt: true,
		AppendSystemPrompt:  " ",
	})

	// Read the session file even when Run returned an error — a non-zero exit still writes the
	// JSONL and may contain the snapshot we need.
	report, parseErr := parseSessionReport(dir, sessionID)
	report.GatewayURL = os.Getenv("ANTHROPIC_BASE_URL")

	// CLIPrefix is the first wire segment when it is the standard Anthropic identity string.
	// The CLI always prepends it as a separate system block before the user config segments.
	if report.CLIPrefix == "" && len(report.WireSegments) > 0 && strings.HasPrefix(report.WireSegments[0], "You are Claude Code") {
		report.CLIPrefix = report.WireSegments[0]
	}

	report.InjectionDetected = injectionDetected(report.CLIPrefix, report.LocalSegments, report.WireSegments)

	if parseErr != nil {
		if runErr != nil {
			return report, fmt.Errorf("claudecode: inspect run: %v; read session: %w", runErr, parseErr)
		}
		return report, fmt.Errorf("claudecode: inspect: read session: %w", parseErr)
	}
	return report, runErr
}

// newInspectID generates a random RFC-4122 v4 UUID for the diagnostic session.
func newInspectID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// sessionPath returns the path Claude Code uses to persist a session's JSONL.
// Claude encodes the working directory by replacing path separators and colons with dashes,
// then writes <home>/.claude/projects/<encoded-dir>/<session-id>.jsonl.
func sessionPath(dir, sessionID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("claudecode: home dir: %w", err)
	}
	encoded := encodeDirForClaude(dir)
	return filepath.Join(home, ".claude", "projects", encoded, sessionID+".jsonl"), nil
}

// encodeDirForClaude replicates Claude Code's directory-to-project-folder encoding:
// each colon and path separator becomes a dash, so C:\Users\foo → C--Users-foo.
func encodeDirForClaude(dir string) string {
	r := strings.NewReplacer(":", "-", "\\", "-", "/", "-")
	return r.Replace(dir)
}

// parseSessionReport reads the session JSONL and extracts the three prompt layers.
func parseSessionReport(dir, sessionID string) (PromptReport, error) {
	path, err := sessionPath(dir, sessionID)
	if err != nil {
		return PromptReport{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return PromptReport{}, fmt.Errorf("claudecode: open session file %q: %w", path, err)
	}
	defer f.Close()

	var report PromptReport
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil {
			continue
		}
		switch string(raw["type"]) {
		case `"attachment"`:
			var att struct {
				Attachment struct {
					Type         string   `json:"type"`
					CLIPrefix    string   `json:"cliPrefix"`
					SystemPrompt []string `json:"systemPrompt"`
				} `json:"attachment"`
			}
			if err := json.Unmarshal(sc.Bytes(), &att); err != nil {
				continue
			}
			if att.Attachment.Type == "prompt_snapshot" {
				if att.Attachment.CLIPrefix != "" && report.CLIPrefix == "" {
					report.CLIPrefix = att.Attachment.CLIPrefix
				}
				// Use only the first snapshot — it matches the api-request-shape timestamp.
				// Later snapshots reflect mid-session system turns and will differ in segment
				// count from what was actually sent over the wire, causing false positives.
				if len(att.Attachment.SystemPrompt) > 0 && len(report.LocalSegments) == 0 {
					report.LocalSegments = att.Attachment.SystemPrompt
				}
			}
		case `"api-request-shape"`:
			var shape struct {
				Shape struct {
					System []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"system"`
				} `json:"shape"`
			}
			if err := json.Unmarshal(sc.Bytes(), &shape); err != nil {
				continue
			}
			for _, seg := range shape.Shape.System {
				if seg.Text != "" {
					report.WireSegments = append(report.WireSegments, seg.Text)
				}
			}
		case `"api-request"`:
			var req struct {
				RequestDigest string `json:"requestDigest"`
			}
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				continue
			}
			if req.RequestDigest != "" {
				report.RequestDigest = req.RequestDigest
			}
		case `"system"`:
			// system/init carries served_by_anthropic.
			var init struct {
				Subtype           string `json:"subtype"`
				ServedByAnthropic bool   `json:"served_by_anthropic"`
			}
			if err := json.Unmarshal(sc.Bytes(), &init); err != nil {
				continue
			}
			if init.Subtype == "init" {
				report.ServedByAnthropic = init.ServedByAnthropic
			}
		}
	}
	return report, sc.Err()
}

// injectionDetected compares the non-whitespace content of local and wire segments.
// The CLI splits the system prompt into multiple segments locally but merges them into fewer
// blocks over the wire (cliPrefix as block 0, everything else as block 1). The wire version
// also adds \n\n between logical sections that the local snapshot omits. We strip all
// whitespace before comparing so structural and spacing differences don't trigger false positives.
// The diagnostic " " AppendSystemPrompt segment is excluded before comparison.
func injectionDetected(cliPrefix string, local, wire []string) bool {
	// Filter out the diagnostic single-space segment added by --append-system-prompt.
	filtered := local[:0:0]
	for _, s := range local {
		if strings.TrimSpace(s) != "" {
			filtered = append(filtered, s)
		}
	}
	// Expected: cliPrefix followed by all non-empty local segments concatenated, all whitespace removed.
	expected := stripWS(cliPrefix + strings.Join(filtered, ""))
	// Actual: all wire segments concatenated (cliPrefix is already wire[0]), all whitespace removed.
	actual := stripWS(strings.Join(wire, ""))
	return expected != actual
}

// stripWS removes every whitespace character from s.
func stripWS(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var wsRun = strings.NewReplacer("\r\n", "\n")

// normaliseWS collapses every run of whitespace characters to a single space and trims.
func normaliseWS(s string) string {
	s = wsRun.Replace(s)
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		isSpace := r == ' ' || r == '\t' || r == '\n' || r == '\r'
		if isSpace {
			if !prevSpace {
				b.WriteByte(' ')
			}
			prevSpace = true
		} else {
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}
