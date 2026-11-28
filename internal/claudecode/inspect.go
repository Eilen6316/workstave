package claudecode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type InjectionResult string

const (
	InjectionDetected             InjectionResult = "detected"
	InjectionNotDetected          InjectionResult = "not_detected"
	InjectionInsufficientEvidence InjectionResult = "insufficient_evidence"
)

// PromptReport describes the system prompt layers observed in one diagnostic invocation.
// It compares the local prompt snapshot with the request shape the CLI itself recorded. It has
// no gateway-side or model-side receipt, so it cannot prove what a gateway forwarded.
type PromptReport struct {
	// CLIPrefix is the first system segment in the CLI-local api-request-shape record.
	CLIPrefix string
	// LocalSegments is the full system prompt array the CLI assembled before sending, taken from
	// the first prompt_snapshot attachment. It does not include CLIPrefix; it holds the user
	// configuration and any AppendSystemPrompt.
	LocalSegments []string
	// WireSegments is the system prompt array in the CLI-local api-request-shape record. This
	// records the request shape observed by the CLI, not a gateway-side or model-side receipt.
	WireSegments []string
	// RequestDigest is the CLI-recorded digest for the api-request matching the selected shapeHash.
	// It does not prove that a gateway forwarded the same bytes to a model.
	RequestDigest string
	// ServedByAnthropic is false when the CLI detected a non-Anthropic endpoint (ANTHROPIC_BASE_URL
	// points elsewhere). It does not by itself mean injection occurred.
	ServedByAnthropic bool
	// GatewayURL contains only the scheme and host from ANTHROPIC_BASE_URL.
	GatewayURL string
	// InjectionDetected compares the local snapshot with its CLI-recorded request shape.
	InjectionDetected InjectionResult
}

// Inspect runs one minimal diagnostic invocation and reads the session JSONL it writes to disk.
// It returns a PromptReport comparing the local snapshot with the CLI-recorded request shape.
// The result is a weak local signal, not proof about what a gateway forwarded.
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
	report.GatewayURL = safeGatewayURL(os.Getenv("ANTHROPIC_BASE_URL"))
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

func safeGatewayURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
}

// parseSessionReport reads the first prompt snapshot, the first later CLI request shape, and the
// request digest correlated with that shape by shapeHash.
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
	var snapshotTime time.Time
	var selectedShapeHash string
	snapshotSeen := false
	shapeSelected := false
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
					SystemPrompt []string `json:"systemPrompt"`
				} `json:"attachment"`
			}
			if err := json.Unmarshal(sc.Bytes(), &att); err != nil {
				continue
			}
			if att.Attachment.Type == "prompt_snapshot" && !snapshotSeen {
				snapshotSeen = true
				report.LocalSegments = att.Attachment.SystemPrompt
				snapshotTime, _ = time.Parse(time.RFC3339Nano, stringValue(raw["timestamp"]))
			}
		case `"api-request-shape"`:
			var shape struct {
				ShapeHash string `json:"shapeHash"`
				Timestamp string `json:"timestamp"`
				Shape     struct {
					System []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"system"`
				} `json:"shape"`
			}
			if err := json.Unmarshal(sc.Bytes(), &shape); err != nil {
				continue
			}
			if !snapshotSeen || shapeSelected || shape.ShapeHash == "" || snapshotTime.IsZero() {
				continue
			}
			shapeTime, err := time.Parse(time.RFC3339Nano, shape.Timestamp)
			if err != nil || shapeTime.Before(snapshotTime) {
				continue
			}
			shapeSelected = true
			selectedShapeHash = shape.ShapeHash
			for _, seg := range shape.Shape.System {
				if seg.Text != "" {
					report.WireSegments = append(report.WireSegments, seg.Text)
				}
			}
			if len(report.WireSegments) != 0 {
				report.CLIPrefix = report.WireSegments[0]
			}
		case `"api-request"`:
			var req struct {
				ShapeHash     string `json:"shapeHash"`
				RequestDigest string `json:"requestDigest"`
			}
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				continue
			}
			if shapeSelected && req.ShapeHash == selectedShapeHash && req.RequestDigest != "" {
				report.RequestDigest = req.RequestDigest
			}
		case `"system"`:
			// system/init carries served_by_anthropic.
			var init struct {
				Subtype           string `json:"subtype"`
				ServedByAnthropic *bool  `json:"served_by_anthropic"`
			}
			if err := json.Unmarshal(sc.Bytes(), &init); err != nil {
				continue
			}
			if init.Subtype == "init" && init.ServedByAnthropic != nil {
				report.ServedByAnthropic = *init.ServedByAnthropic
			}
		}
	}
	return report, sc.Err()
}

func stringValue(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// injectionDetected compares the non-whitespace content of local and CLI-recorded request segments.
// The CLI splits the system prompt into multiple segments locally but merges them into fewer
// blocks over the wire (cliPrefix as block 0, everything else as block 1). The wire version
// also adds \n\n between logical sections that the local snapshot omits. We strip all
// whitespace before comparing so structural and spacing differences don't trigger false positives.
// The diagnostic " " AppendSystemPrompt segment is excluded before comparison.
func injectionDetected(cliPrefix string, local, wire []string) InjectionResult {
	// The first request segment must be the known Claude Code identity prefix. Otherwise the
	// comparison would use an unverified segment as its own baseline, so evidence is insufficient.
	if !strings.HasPrefix(cliPrefix, "You are Claude Code") || len(local) == 0 || len(wire) == 0 {
		return InjectionInsufficientEvidence
	}
	// Filter out the diagnostic single-space segment added by --append-system-prompt.
	filtered := make([]string, 0, len(local))
	for _, s := range local {
		if strings.TrimSpace(s) != "" {
			filtered = append(filtered, s)
		}
	}
	// Expected: cliPrefix followed by all non-empty local segments concatenated, all whitespace removed.
	expected := stripWS(cliPrefix + strings.Join(filtered, ""))
	// Actual: all wire segments concatenated (cliPrefix is already wire[0]), all whitespace removed.
	actual := stripWS(strings.Join(wire, ""))
	if expected != actual {
		return InjectionDetected
	}
	return InjectionNotDetected
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
