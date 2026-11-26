package claudecode

import (
	"reflect"
	"testing"
)

func TestBuildArgsResumeUsesSavedSessionID(t *testing.T) {
	got := buildArgs(Options{SessionID: "session-1", Resume: true, MaxBudget: "0.05"})
	want := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--tools", "", "--strict-mcp-config", "--permission-mode", "dontAsk",
		"--max-budget-usd", "0.05", "--resume", "session-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs(resume) = %#v, want %#v", got, want)
	}
}
