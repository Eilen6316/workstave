package codex

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildArgsResumeUsesSavedThreadWithoutCreateOptions(t *testing.T) {
	got := buildArgs(Options{Dir: `C:\work`, ThreadID: "thread-1", Model: "gpt-6-luna", Sandbox: "workspace-write", Prompt: "continue"})
	want := []string{"exec", "resume", "thread-1", "--json", "--skip-git-repo-check", "--model", "gpt-6-luna", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs(resume) = %#v, want %#v", got, want)
	}
}

func TestBuildArgsCreateUsesSelectedModelAndReadOnlySandbox(t *testing.T) {
	got := buildArgs(Options{Dir: `C:\work`, Model: "gpt-6-luna", Sandbox: "read-only", Prompt: "start"})
	want := []string{"exec", "--json", "--skip-git-repo-check", "--model", "gpt-6-luna", "-s", "read-only", "-C", `C:\work`, "-"}
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
	want := []string{"exec", "--json", "--skip-git-repo-check", "-c", "features.web_search=false", "-c", "features.mcp=false", "--model", "gpt-6-luna", "-s", "read-only", "-C", `C:\work`, "-"}
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
	want := []string{"exec", "resume", "thread-1", "--json", "--skip-git-repo-check", "-c", "features.web_search=false", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs(resume with overrides) = %#v, want %#v", got, want)
	}
}

func TestBuildArgsPassesOutputSchemaPathForCreateAndResume(t *testing.T) {
	for name, options := range map[string]Options{
		"create": {OutputSchemaPath: `C:\Temp\schema files\output.json`},
		"resume": {ThreadID: "thread-1", OutputSchemaPath: `C:\Temp\schema files\output.json`},
	} {
		t.Run(name, func(t *testing.T) {
			args := buildArgs(options)
			for i, arg := range args {
				if arg == "--output-schema" {
					if i+1 >= len(args) || args[i+1] != options.OutputSchemaPath {
						t.Fatalf("--output-schema args=%#v, want exact path %q", args, options.OutputSchemaPath)
					}
					return
				}
			}
			t.Fatalf("--output-schema missing from args %#v", args)
		})
	}
}

// A multi-line prompt must never reach argv: the npm .cmd wrapper on Windows keeps only
// the first line of such an argument and drops the rest without any error.
func TestBuildArgsKeepsPromptOutOfArgv(t *testing.T) {
	prompt := "line one\nline two"
	for _, o := range []Options{{Dir: `C:\work`, Prompt: prompt}, {ThreadID: "thread-1", Prompt: prompt}} {
		for _, arg := range buildArgs(o) {
			if strings.Contains(arg, "line") {
				t.Fatalf("prompt leaked into argv: %#v", buildArgs(o))
			}
		}
	}
}
