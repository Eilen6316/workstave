package claudecode

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRealInspect(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run the billable Inspect probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	report, err := Inspect(ctx, t.TempDir(), "0.02")
	if err != nil {
		t.Fatalf("Inspect error: %v", err)
	}

	t.Logf("GatewayURL:         %q", report.GatewayURL)
	t.Logf("ServedByAnthropic:  %v", report.ServedByAnthropic)
	t.Logf("InjectionDetected:  %v", report.InjectionDetected)
	t.Logf("RequestDigest:      %s", report.RequestDigest)
	t.Logf("CLIPrefix:          %q", report.CLIPrefix)
	t.Logf("LocalSegments (%d):", len(report.LocalSegments))
	for i, s := range report.LocalSegments {
		t.Logf("  [%d] %q", i, truncate(s, 120))
	}
	t.Logf("WireSegments (%d):", len(report.WireSegments))
	for i, s := range report.WireSegments {
		t.Logf("  [%d] %q", i, truncate(s, 120))
	}

	if report.CLIPrefix == "" {
		t.Error("CLIPrefix is empty — prompt_snapshot cliPrefix not found in session JSONL")
	}
	if !strings.Contains(report.CLIPrefix, "Claude Code") {
		t.Errorf("CLIPrefix does not mention Claude Code: %q", report.CLIPrefix)
	}
	if len(report.LocalSegments) == 0 {
		t.Error("LocalSegments is empty — prompt_snapshot systemPrompt not found in session JSONL")
	}
	if len(report.WireSegments) == 0 {
		t.Error("WireSegments is empty — api-request-shape not found in session JSONL")
	}
	if report.RequestDigest == "" {
		t.Error("RequestDigest is empty")
	}
	if report.InjectionDetected {
		t.Logf("INJECTION DETECTED: wire segments differ from local snapshot")
		t.Logf("Local count=%d  Wire count=%d", len(report.LocalSegments), len(report.WireSegments))
		for i := range report.WireSegments {
			if i < len(report.LocalSegments) && strings.TrimSpace(report.LocalSegments[i]) != strings.TrimSpace(report.WireSegments[i]) {
				t.Logf("  segment[%d] differs", i)
				t.Logf("    local: %q", truncate(report.LocalSegments[i], 200))
				t.Logf("    wire:  %q", truncate(report.WireSegments[i], 200))
			}
		}
		// Not a hard failure — this is a diagnostic, not an enforcement. The caller decides.
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
