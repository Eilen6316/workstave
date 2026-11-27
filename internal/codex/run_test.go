package codex

import (
	"reflect"
	"testing"
)

func TestBuildArgsResumeUsesSavedThreadWithoutCreateOptions(t *testing.T) {
	got := buildArgs(Options{Dir: `C:\work`, ThreadID: "thread-1", Model: "gpt-6-luna", Sandbox: "workspace-write", Prompt: "continue"})
	want := []string{"exec", "resume", "thread-1", "--json", "--skip-git-repo-check", "--model", "gpt-6-luna", "continue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs(resume) = %#v, want %#v", got, want)
	}
}

func TestBuildArgsCreateUsesSelectedModelAndReadOnlySandbox(t *testing.T) {
	got := buildArgs(Options{Dir: `C:\work`, Model: "gpt-6-luna", Sandbox: "read-only", Prompt: "start"})
	want := []string{"exec", "--json", "--skip-git-repo-check", "--model", "gpt-6-luna", "-s", "read-only", "-C", `C:\work`, "start"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs(create) = %#v, want %#v", got, want)
	}
}

func TestBuildArgsCreateWithConfigOverrides(t *testing.T) {
	got := buildArgs(Options{
		Dir:             `C:\work`,
		Model:           "gpt-6-luna",
		ConfigOverrides: []string{"features.web_search=false", "features.mcp=false"},
		Prompt:          "start",
	})
	want := []string{"exec", "--json", "--skip-git-repo-check", "-c", "features.web_search=false", "-c", "features.mcp=false", "--model", "gpt-6-luna", "-s", "read-only", "-C", `C:\work`, "start"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs(with overrides) = %#v, want %#v", got, want)
	}
}

func TestBuildArgsResumeWithConfigOverrides(t *testing.T) {
	got := buildArgs(Options{
		ThreadID:        "thread-1",
		ConfigOverrides: []string{"features.web_search=false"},
		Prompt:          "continue",
	})
	want := []string{"exec", "resume", "thread-1", "--json", "--skip-git-repo-check", "-c", "features.web_search=false", "continue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs(resume with overrides) = %#v, want %#v", got, want)
	}
}
