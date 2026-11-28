package claudecode

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeDirForClaude(t *testing.T) {
	cases := []struct {
		dir  string
		want string
	}{
		{`C:\Users\LYX\AppData\Local\Temp`, `C--Users-LYX-AppData-Local-Temp`},
		{`/home/user/work`, `-home-user-work`},
		{`C:\Users\LYX\Desktop\Workstave`, `C--Users-LYX-Desktop-Workstave`},
	}
	for _, c := range cases {
		if got := encodeDirForClaude(c.dir); got != c.want {
			t.Errorf("encodeDirForClaude(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

func TestInjectionDetected(t *testing.T) {
	cases := []struct {
		desc      string
		cliPrefix string
		local     []string
		wire      []string
		want      InjectionResult
	}{
		{
			desc:      "clean: wire merges cliPrefix + local into two blocks",
			cliPrefix: "You are Claude Code.",
			local:     []string{"Do X.", "Do Y."},
			wire:      []string{"You are Claude Code.", "Do X.Do Y."},
			want:      InjectionNotDetected,
		},
		{
			desc:      "clean: extra whitespace differences are normalised away",
			cliPrefix: "You are Claude Code.",
			local:     []string{"\nDo X.\n", "Do Y."},
			// wire[1] is local segments concatenated verbatim, with no added spaces
			wire: []string{"You are Claude Code.", "\nDo X.\nDo Y."},
			want: InjectionNotDetected,
		},
		{
			desc:      "injection: gateway appended extra text",
			cliPrefix: "You are Claude Code.",
			local:     []string{"Do X."},
			wire:      []string{"You are Claude Code.", "Do X.", "INJECTED"},
			want:      InjectionDetected,
		},
		{
			desc:      "injection: gateway replaced a segment",
			cliPrefix: "You are Claude Code.",
			local:     []string{"Do X."},
			wire:      []string{"You are Claude Code.", "Do SOMETHING ELSE."},
			want:      InjectionDetected,
		},
		{desc: "missing CLI prefix is insufficient evidence", local: []string{"Do X."}, wire: []string{"Do X."}, want: InjectionInsufficientEvidence},
		{desc: "unknown CLI prefix is insufficient evidence", cliPrefix: "CLI prefix", local: []string{"Do X."}, wire: []string{"CLI prefix", "Do X."}, want: InjectionInsufficientEvidence},
		{desc: "missing local segments is insufficient evidence", cliPrefix: "You are Claude Code.", wire: []string{"You are Claude Code."}, want: InjectionInsufficientEvidence},
		{desc: "missing request segments is insufficient evidence", cliPrefix: "You are Claude Code.", local: []string{"Do X."}, want: InjectionInsufficientEvidence},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := injectionDetected(c.cliPrefix, c.local, c.wire)
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestSafeGatewayURL(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{"https://user:secret@gateway.example:8443/v1/messages?token=secret#fragment", "https://gateway.example:8443"},
		{"https://gateway.example/v1?api_key=secret", "https://gateway.example"},
		{"", ""},
		{"not a URL", ""},
	} {
		if got := safeGatewayURL(test.input); got != test.want {
			t.Errorf("safeGatewayURL(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestParseSessionReportUsesFirstSnapshotShapeAndMatchingRequest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	dir := `C:\work\inspect`
	sessionID := "session-inspect"
	path := filepath.Join(home, ".claude", "projects", encodeDirForClaude(dir), sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	write := func(value any) {
		t.Helper()
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{
		"type": "attachment", "timestamp": "2026-11-28T10:00:00.000Z",
		"attachment": map[string]any{"type": "prompt_snapshot", "systemPrompt": []string{"Local first", "Local second"}},
	})
	write(map[string]any{
		"type": "api-request-shape", "timestamp": "2026-11-28T10:00:00.010Z", "shapeHash": "first-shape",
		"shape": map[string]any{"system": []any{
			map[string]any{"type": "text", "text": "You are Claude Code, test prefix."},
			map[string]any{"type": "text", "text": "Local firstLocal second"},
		}},
	})
	write(map[string]any{"type": "api-request", "shapeHash": "other-shape", "requestDigest": "wrong-digest"})
	write(map[string]any{"type": "api-request", "shapeHash": "first-shape", "requestDigest": "first-digest"})
	write(map[string]any{
		"type": "api-request-shape", "timestamp": "2026-11-28T10:01:00.000Z", "shapeHash": "second-shape",
		"shape": map[string]any{"system": []any{map[string]any{"type": "text", "text": "Later injected content"}}},
	})
	write(map[string]any{"type": "api-request", "shapeHash": "second-shape", "requestDigest": "second-digest"})
	write(map[string]any{
		"type": "attachment", "timestamp": "2026-11-28T10:01:00.000Z",
		"attachment": map[string]any{"type": "prompt_snapshot", "systemPrompt": []string{"Later local prompt"}},
	})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := parseSessionReport(dir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if report.CLIPrefix != "You are Claude Code, test prefix." || strings.Join(report.LocalSegments, "|") != "Local first|Local second" ||
		strings.Join(report.WireSegments, "|") != "You are Claude Code, test prefix.|Local firstLocal second" || report.RequestDigest != "first-digest" {
		t.Fatalf("report mixed request records or selected a later snapshot: %+v", report)
	}
	if got := injectionDetected(report.CLIPrefix, report.LocalSegments, report.WireSegments); got != InjectionNotDetected {
		t.Fatalf("first matched request injection result=%q, want %q", got, InjectionNotDetected)
	}
}

func TestNewInspectIDIsValidUUID(t *testing.T) {
	id, err := newInspectID()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("newInspectID() = %q, want UUID with 5 hyphen-separated parts", id)
	}
	lengths := []int{8, 4, 4, 4, 12}
	for i, part := range parts {
		if len(part) != lengths[i] {
			t.Errorf("part %d %q: length %d, want %d", i, part, len(part), lengths[i])
		}
	}
	// version nibble must be 4
	if parts[2][0] != '4' {
		t.Errorf("UUID version nibble = %c, want 4", parts[2][0])
	}
}
