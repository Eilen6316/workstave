package claudecode

import (
	"reflect"
	"testing"
)

func TestBuildArgsResumeUsesSavedSessionID(t *testing.T) {
	got := buildArgs(Options{SessionID: "session-1", Resume: true, MaxBudget: "0.05", Model: "claude-haiku-4-5-20251001"})
	want := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--tools=", "--strict-mcp-config", "--permission-mode", "dontAsk",
		"--max-budget-usd", "0.05", "--model", "claude-haiku-4-5-20251001", "--resume", "session-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs(resume) = %#v, want %#v", got, want)
	}
}

// When Tools is empty the equals form --tools= is used so cmd.exe does not drop the token.
// A bare empty-string argument is silently discarded by the .cmd wrapper, which shifts
// --strict-mcp-config into the value slot of --tools and disables it.
func TestBuildArgsOmitsToolsFlagWhenEmpty(t *testing.T) {
	args := buildArgs(Options{SessionID: "s"})
	for i, arg := range args {
		if arg == "--tools" {
			t.Fatalf("buildArgs emitted separate --tools flag at position %d (should use --tools= form): %#v", i, args)
		}
	}
	found := false
	for _, arg := range args {
		if arg == "--tools=" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("buildArgs did not emit --tools= for empty Tools: %#v", args)
	}
}

func TestBuildArgsIncludesToolsFlagWhenSet(t *testing.T) {
	got := buildArgs(Options{SessionID: "s", Tools: "Bash"})
	found := false
	for i, arg := range got {
		if arg == "--tools" && i+1 < len(got) && got[i+1] == "Bash" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("buildArgs did not include --tools Bash: %#v", got)
	}
}
