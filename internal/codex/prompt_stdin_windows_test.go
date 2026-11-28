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
const args = process.argv.slice(2);
fs.writeFileSync(__dirname + "/argv.json", JSON.stringify(args));
fs.writeFileSync(__dirname + "/stdin.txt", fs.readFileSync(0));
const schemaIndex = args.indexOf("--output-schema");
if (schemaIndex >= 0) fs.writeFileSync(__dirname + "/schema-readback.json", fs.readFileSync(args[schemaIndex + 1]));
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
	schema := `{"type":"object","properties":{"reply":{"type":"string"},"changes":{"type":"array"}},"required":["reply","changes"]}`
	schemaDir := filepath.Join(dir, "schema files")
	if err := os.Mkdir(schemaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(schemaDir, "output.json")
	if err := os.WriteFile(schemaPath, []byte(schema), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"create": {Bin: bin, Dir: dir, Prompt: prompt, OutputSchemaPath: schemaPath},
		"resume": {Bin: bin, Dir: dir, ThreadID: "thread-1", Prompt: prompt, OutputSchemaPath: schemaPath},
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
			schemaIndex := -1
			for i, arg := range argv {
				if arg == "--output-schema" {
					schemaIndex = i
					break
				}
			}
			if schemaIndex < 0 || schemaIndex+1 == len(argv) || argv[schemaIndex+1] != schemaPath {
				t.Fatalf("shim argv=%v, want the complete output schema path %q", argv, schemaPath)
			}
			readback, err := os.ReadFile(filepath.Join(dir, "schema-readback.json"))
			if err != nil || string(readback) != schema {
				t.Fatalf("schema read through .cmd shim=%q err=%v, want the original schema", readback, err)
			}
		})
	}
}
