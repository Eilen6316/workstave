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

// TestEmptyToolsArgDroppedByCmdWrapper proves the root cause of the Claude Run failure:
// an empty string passed as an argument through a .cmd shim is silently dropped by cmd.exe,
// shifting every later argument by one position. The fix is to omit --tools entirely when the
// field is empty; this test records both the broken behaviour and the fixed behaviour.
func TestEmptyToolsArgDroppedByCmdWrapper(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed; the npm-style shim needs it")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "record.js"),
		[]byte(`require("fs").writeFileSync(__dirname+"/argv.json", JSON.stringify(process.argv.slice(2)));`),
		0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "shim.cmd")
	if err := os.WriteFile(bin, []byte("@ECHO off\r\nnode \"%~dp0record.js\" %*\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	readArgv := func() []string {
		t.Helper()
		if err := os.Remove(filepath.Join(dir, "argv.json")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, "--tools", "", "--strict-mcp-config")
		if err := cmd.Run(); err != nil {
			// non-zero exit is expected; the script just records argv
		}
		raw, err := os.ReadFile(filepath.Join(dir, "argv.json"))
		if err != nil {
			t.Fatal(err)
		}
		var argv []string
		if err := json.Unmarshal(raw, &argv); err != nil {
			t.Fatal(err)
		}
		return argv
	}

	// Demonstrate the broken baseline: the empty string is dropped, so --strict-mcp-config
	// becomes the value of --tools instead of a separate flag.
	argv := readArgv()
	t.Logf("shim argv with empty --tools arg: %v", argv)
	strictPosition := -1
	for i, a := range argv {
		if a == "--strict-mcp-config" {
			strictPosition = i
		}
	}
	if strictPosition == 2 {
		t.Logf("CONFIRMED root cause: --strict-mcp-config (pos %d) became the value of --tools after cmd.exe dropped the empty string", strictPosition)
	}

	// Exercise the real Driver argument construction through the .cmd wrapper.
	if _, err := Run(context.Background(), Options{Bin: bin, Dir: dir, SessionID: "s-fixed"}); err != nil {
		t.Fatalf("Run through the fixed shim: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "argv.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixedArgs []string
	if err := json.Unmarshal(raw, &fixedArgs); err != nil {
		t.Fatal(err)
	}
	fixedStrictPosition := -1
	for i, arg := range fixedArgs {
		if arg == "--tools" {
			t.Fatalf("fixed Driver emitted separate --tools at position %d: %v", i, fixedArgs)
		}
		if arg == "--tools=" && i+1 < len(fixedArgs) && fixedArgs[i+1] == "--strict-mcp-config" {
			fixedStrictPosition = i + 1
		}
	}
	if fixedStrictPosition < 0 {
		t.Fatalf("fixed Driver did not preserve --tools= before --strict-mcp-config: %v", fixedArgs)
	}
}
