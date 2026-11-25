// Package appserver is a minimal synchronous JSON-RPC client for `codex app-server` over stdio.
// It has no background goroutines: every Call reads the stream itself, so its lifetime is the
// caller's. Notifications seen while waiting for a response are kept in Notes, in order.
package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"workstave/internal/procgroup"
)

// Msg is one JSON-RPC line. The app-server protocol omits the jsonrpc field.
type Msg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object returned by the server.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Client owns one `codex app-server` process.
type Client struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	sc     *bufio.Scanner
	next   int
	Notes  []Msg
	Stderr strings.Builder
}

// Start launches the app-server on stdio. Cancelling ctx kills the whole process tree.
func Start(ctx context.Context, dir string) (*Client, error) {
	c := &Client{}
	c.cmd = exec.CommandContext(ctx, "codex", "app-server")
	c.cmd.Dir = dir
	procgroup.Configure(c.cmd)
	c.cmd.Cancel = func() error { return procgroup.Kill(c.cmd.Process.Pid) }
	c.cmd.Stderr = &c.Stderr
	in, err := c.cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("appserver: stdin pipe: %w", err)
	}
	out, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("appserver: stdout pipe: %w", err)
	}
	c.in = in
	c.sc = bufio.NewScanner(out)
	c.sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	if err := c.cmd.Start(); err != nil {
		return nil, fmt.Errorf("appserver: start: %w", err)
	}
	return c, nil
}

// Pid returns the OS pid of the app-server wrapper process.
func (c *Client) Pid() int { return c.cmd.Process.Pid }

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(b, '\n'))
	return err
}

// Notify sends a notification (no response expected).
func (c *Client) Notify(method string, params any) error {
	return c.send(map[string]any{"method": method, "params": params})
}

// Call sends a request and reads the stream until its response arrives.
// Server-initiated requests are answered with an error and recorded in Notes.
func (c *Client) Call(method string, params any) (json.RawMessage, error) {
	c.next++
	id := c.next
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("appserver: send %s: %w", method, err)
	}
	for c.sc.Scan() {
		var m Msg
		if err := json.Unmarshal(c.sc.Bytes(), &m); err != nil {
			return nil, fmt.Errorf("appserver: %s: bad line: %w", method, err)
		}
		switch {
		case m.Method == "" && string(m.ID) == strconv.Itoa(id):
			if m.Error != nil {
				return nil, m.Error
			}
			return m.Result, nil
		case m.Method != "" && len(m.ID) > 0:
			err := c.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "workstave: unhandled server request"}})
			if err != nil {
				return nil, fmt.Errorf("appserver: reply to server request %s: %w", m.Method, err)
			}
			c.Notes = append(c.Notes, m)
		default:
			c.Notes = append(c.Notes, m)
		}
	}
	err := c.sc.Err()
	if err == nil {
		err = io.EOF
	}
	return nil, fmt.Errorf("appserver: %s: stream ended: %w", method, err)
}

// Close closes stdin, kills the process tree and reaps the process. The wait error is not
// returned because a killed process always reports one.
func (c *Client) Close() error {
	_ = c.in.Close()
	killErr := procgroup.Kill(c.cmd.Process.Pid)
	_ = c.cmd.Wait()
	return killErr
}
