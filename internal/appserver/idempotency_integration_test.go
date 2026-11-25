package appserver

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Real-harness test: starts `codex app-server` with the user's own configuration and makes
// billable requests. It only runs when WORKSTAVE_REAL_HARNESS=1. It records whether a repeated
// clientUserMessageId is deduplicated; it does not assume either outcome.

func mustCall(t *testing.T, c *Client, method string, params any) json.RawMessage {
	t.Helper()
	r, err := c.Call(method, params)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return r
}

func TestRealClientUserMessageIDRetry(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run against the real Codex app-server (billable)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dir := t.TempDir()
	c, err := Start(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	init := mustCall(t, c, "initialize", map[string]any{"clientInfo": map[string]any{"name": "workstave", "version": "0.0.0"}})
	t.Logf("initialize result: %.300s", init)
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}

	raw := mustCall(t, c, "thread/start", map[string]any{"cwd": dir, "sandbox": "read-only", "approvalPolicy": "never"})
	var ts struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &ts); err != nil || ts.Thread.ID == "" {
		t.Fatalf("thread/start gave no thread id: err=%v raw=%.300s", err, raw)
	}
	t.Logf("thread id: %s", ts.Thread.ID)

	params := map[string]any{
		"threadId":            ts.Thread.ID,
		"clientUserMessageId": "ws-idem-1",
		"input":               []any{map[string]any{"type": "text", "text": "Reply with exactly the word: pong"}},
	}
	for i := 1; i <= 2; i++ {
		r, err := c.Call("turn/start", params)
		t.Logf("turn/start attempt %d: err=%v result=%.200s", i, err, r)
	}

	// Call is the only reader, so keep issuing a cheap request to drain notifications.
	for i := 0; i < 20; i++ {
		time.Sleep(2 * time.Second)
		if _, err := c.Call("thread/loaded/list", map[string]any{}); err != nil {
			t.Logf("drain call %d: %v", i, err)
			break
		}
	}

	counts := map[string]int{}
	userMsgs := 0
	for _, n := range c.Notes {
		counts[n.Method]++
		if n.Method == "item/completed" && strings.Contains(string(n.Params), "userMessage") {
			userMsgs++
		}
	}
	t.Logf("turn/started=%d turn/completed=%d userMessage items=%d", counts["turn/started"], counts["turn/completed"], userMsgs)
	t.Logf("all notification counts: %v", counts)
	t.Logf("stderr: %q", c.Stderr.String())
}
