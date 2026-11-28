//go:build windows

package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// TestRunDeliversMultiLinePromptThroughCmdWrapper runs Run against a .cmd shim shaped like the
// npm wrapper. The shim records its argv and stdin, so the test proves that a multi-line prompt
// arrives complete on stdin and never depends on argv, without calling a real model.
func TestRunDeliversMultiLinePromptThroughCmdWrapper(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed; the npm-style shim needs it")
	}
	dir := t.TempDir()
	script := `const fs = require("fs");
fs.writeFileSync(__dirname + "/argv.json", JSON.stringify(process.argv.slice(2)));
fs.writeFileSync(__dirname + "/stdin.txt", fs.readFileSync(0));
console.log(JSON.stringify({type:"thread.started",thread_id:"shim-thread"}));
console.log(JSON.stringify({type:"turn.completed"}));
`
	if err := os.WriteFile(filepath.Join(dir, "record.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	shim := "@ECHO off\r\nnode \"%~dp0record.js\" %*\r\n"
	bin := filepath.Join(dir, "shim.cmd")
	if err := os.WriteFile(bin, []byte(shim), 0o600); err != nil {
		t.Fatal(err)
	}

	prompt := "line one: instructions\nline two: confirmed JWT\nline three: user message"
	for name, o := range map[string]Options{
		"create": {Bin: bin, Dir: dir, Prompt: prompt},
		"resume": {Bin: bin, Dir: dir, ThreadID: "thread-1", Prompt: prompt},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := Run(context.Background(), o)
			if err != nil || !res.Completed || res.ThreadID != "shim-thread" {
				t.Fatalf("Run result=%+v err=%v, want the shim's completed thread", res, err)
			}
			stdin, err := os.ReadFile(filepath.Join(dir, "stdin.txt"))
			if err != nil || string(stdin) != prompt {
				t.Fatalf("shim stdin=%q err=%v, want the full multi-line prompt", stdin, err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "argv.json"))
			if err != nil {
				t.Fatal(err)
			}
			var argv []string
			if err := json.Unmarshal(raw, &argv); err != nil {
				t.Fatal(err)
			}
			if len(argv) == 0 || argv[len(argv)-1] != "-" {
				t.Fatalf("argv=%v, want the stdin marker \"-\" as the final argument", argv)
			}
			if reflect.DeepEqual(argv, []string{}) {
				t.Fatal("shim recorded no argv")
			}
		})
	}
}
