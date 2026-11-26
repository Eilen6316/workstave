package codex

import (
	"reflect"
	"testing"
)

func TestBuildArgsResumeUsesSavedThreadWithoutCreateOptions(t *testing.T) {
	got := buildArgs(Options{Dir: `C:\work`, ThreadID: "thread-1", Sandbox: "workspace-write", Prompt: "continue"})
	want := []string{"exec", "resume", "thread-1", "--json", "--skip-git-repo-check", "continue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs(resume) = %#v, want %#v", got, want)
	}
}
