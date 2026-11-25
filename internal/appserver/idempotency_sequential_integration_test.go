package appserver

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Real-harness test: repeats turn/start with the same clientUserMessageId after the first turn
// has completed. It records the outcome and assumes neither deduplication nor a second turn.
func TestRealClientUserMessageIDAfterCompletion(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run against the real Codex app-server (billable)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()
	c, err := Start(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	mustCall(t, c, "initialize", map[string]any{"clientInfo": map[string]any{"name": "workstave", "version": "0.0.0"}})
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
		"clientUserMessageId": "ws-idem-seq-1",
		"input":               []any{map[string]any{"type": "text", "text": "Reply with exactly the word: pong"}},
	}
	completed := func() int {
		n := 0
		for _, m := range c.Notes {
			if m.Method == "turn/completed" {
				n++
			}
		}
		return n
	}
	send := func(label string) string {
		r, err := c.Call("turn/start", params)
		t.Logf("%s: err=%v result=%.160s", label, err, r)
		var tr struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(r, &tr)
		return tr.Turn.ID
	}
	wait := func(want int) {
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) && completed() < want {
			time.Sleep(2 * time.Second)
			if _, err := c.Call("thread/loaded/list", map[string]any{}); err != nil {
				t.Logf("drain: %v", err)
				return
			}
		}
	}

	id1 := send("first turn/start")
	wait(1)
	t.Logf("turn/completed after first: %d", completed())
	id2 := send("second turn/start (same id, after completion)")
	wait(2)
	t.Logf("first turn id=%s second turn id=%s same=%v", id1, id2, id1 != "" && id1 == id2)
	t.Logf("turn/completed total: %d", completed())
	t.Logf("stderr: %q", c.Stderr.String())
}
