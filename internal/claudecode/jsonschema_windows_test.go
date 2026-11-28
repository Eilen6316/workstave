//go:build windows

package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRunPassesJSONSchemaIntactThroughCmdWrapper runs Run against a .cmd shim shaped like the npm
// wrapper. The shim records its argv, so the test shows whether a schema that contains double
// quotes reaches the program unchanged. It does not call a real model.
func TestRunPassesJSONSchemaIntactThroughCmdWrapper(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed; the npm-style shim needs it")
	}
	dir := t.TempDir()
	script := `require("fs").writeFileSync(__dirname + "/argv.json", JSON.stringify(process.argv.slice(2)));
`
	if err := os.WriteFile(filepath.Join(dir, "record.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "shim.cmd")
	if err := os.WriteFile(bin, []byte("@ECHO off\r\nnode \"%~dp0record.js\" %*\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	schema := `{"type":"object","properties":{"reply":{"type":"string"},"changes":{"type":"array","items":{"type":"object"}}},"required":["reply","changes"]}`
	if _, err := Run(context.Background(), Options{Bin: bin, Dir: dir, SessionID: "s-1", Prompt: "hi", JSONSchema: schema}); err != nil {
		t.Fatalf("Run through the shim: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "argv.json"))
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		t.Fatal(err)
	}
	found := false
	for i, arg := range argv {
		if arg == "--json-schema" && i+1 < len(argv) {
			found = true
			if argv[i+1] != schema {
				t.Fatalf("schema arrived altered through the .cmd wrapper:\n got %q\nwant %q", argv[i+1], schema)
			}
		}
	}
	if !found {
		t.Fatalf("--json-schema missing from argv: %v", argv)
	}
}
