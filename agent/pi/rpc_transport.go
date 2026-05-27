package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

type rpcOutcome struct {
	resp *rpcResponse
	err  error
}

type rpcResponse struct {
	Type    string          `json:"type"`
	ID      json.RawMessage `json:"id,omitempty"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

func (r *rpcResponse) asError() error {
	if r == nil {
		return fmt.Errorf("pi: rpc response is nil")
	}
	if r.Success {
		return nil
	}
	if r.Error != "" {
		return fmt.Errorf("pi: rpc %s failed: %s", r.Command, r.Error)
	}
	return fmt.Errorf("pi: rpc %s failed", r.Command)
}

type rpcEventHandler func(env map[string]any)

// rpcTransport implements strict JSONL (LF-delimited) request/response over stdio.
// It follows pi's RPC framing rules from packages/coding-agent/docs/rpc.md.
//
// - Commands include a caller-provided id field.
// - Responses include {type:"response", id, command, success, data?, error?}.
// - Events are any non-response objects.
type rpcTransport struct {
	in  *bufio.Reader
	out io.Writer
	enc *json.Encoder
	mu  sync.Mutex

	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[string]chan rpcOutcome

	onEvent rpcEventHandler
}

func newRPCTransport(in io.Reader, out io.Writer, onEvent rpcEventHandler) *rpcTransport {
	return &rpcTransport{
		in:      bufio.NewReader(in),
		out:     out,
		enc:     json.NewEncoder(out),
		pending: make(map[string]chan rpcOutcome),
		onEvent: onEvent,
	}
}

func (t *rpcTransport) readLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line, err := t.readLine(ctx)
		if err != nil {
			t.cancelAll(fmt.Errorf("pi: rpc read closed: %w", err))
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var env map[string]any
		if err := json.Unmarshal(line, &env); err != nil {
			slog.Debug("pi: skip non-json line", "line", string(line))
			continue
		}

		if typ, _ := env["type"].(string); typ == "response" {
			var resp rpcResponse
			if err := json.Unmarshal(line, &resp); err != nil {
				slog.Debug("pi: bad response json", "error", err)
				continue
			}
			if len(resp.ID) == 0 {
				slog.Debug("pi: response missing id", "command", resp.Command)
				continue
			}
			t.completePending(resp.ID, &resp)
			continue
		}

		if t.onEvent != nil {
			t.onEvent(env)
		}
	}
}

func (t *rpcTransport) readLine(ctx context.Context) ([]byte, error) {
	// Prevent unbounded memory growth if the subprocess emits an extremely large JSONL record.
	// Large tool outputs should be written to files and referenced instead.
	const maxLine = 16 * 1024 * 1024

	var line []byte
	for {
		frag, err := t.in.ReadSlice('\n')
		if len(line)+len(frag) > maxLine {
			// Drain until the next newline to re-sync framing, then fail.
			for err == bufio.ErrBufferFull {
				_, err = t.in.ReadSlice('\n')
			}
			return nil, fmt.Errorf("pi: rpc line too long (> %d bytes)", maxLine)
		}
		line = append(line, frag...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}

	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return line, nil
}

func jsonIDKey(id json.RawMessage) string {
	id = bytes.TrimSpace(id)
	var n json.Number
	if json.Unmarshal(id, &n) == nil {
		return string(n)
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s
	}
	return string(id)
}

func (t *rpcTransport) completePending(id json.RawMessage, resp *rpcResponse) {
	key := jsonIDKey(id)
	t.pendingMu.Lock()
	ch, ok := t.pending[key]
	delete(t.pending, key)
	t.pendingMu.Unlock()
	if !ok {
		slog.Debug("pi: unmatched rpc response", "id", key, "command", resp.Command)
		return
	}
	select {
	case ch <- rpcOutcome{resp: resp}:
	default:
	}
}

func (t *rpcTransport) cancelAll(err error) {
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	for k, ch := range t.pending {
		select {
		case ch <- rpcOutcome{err: err}:
		default:
		}
		delete(t.pending, k)
	}
}

func (t *rpcTransport) call(ctx context.Context, cmd map[string]any) (*rpcResponse, error) {
	id := t.nextID.Add(1)
	key := fmt.Sprintf("%d", id)
	cmd["id"] = key

	ch := make(chan rpcOutcome, 1)
	t.pendingMu.Lock()
	t.pending[key] = ch
	t.pendingMu.Unlock()

	if err := t.writeJSON(cmd); err != nil {
		t.pendingMu.Lock()
		delete(t.pending, key)
		t.pendingMu.Unlock()
		return nil, fmt.Errorf("pi: rpc write: %w", err)
	}

	select {
	case <-ctx.Done():
		t.pendingMu.Lock()
		delete(t.pending, key)
		t.pendingMu.Unlock()
		return nil, ctx.Err()
	case out := <-ch:
		if out.err != nil {
			return nil, out.err
		}
		return out.resp, nil
	}
}

func (t *rpcTransport) notify(cmd map[string]any) error {
	return t.writeJSON(cmd)
}

func (t *rpcTransport) writeJSON(v any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.enc.Encode(v)
}
