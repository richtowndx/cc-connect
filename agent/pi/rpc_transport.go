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

	pending sync.Map // map[string]chan rpcOutcome

	onEvent rpcEventHandler
}

func newRPCTransport(in io.Reader, out io.Writer, onEvent rpcEventHandler) *rpcTransport {
	enc := json.NewEncoder(out)
	// We never emit HTML; disabling HTML escaping saves a few CPU cycles per
	// prompt command and avoids surprising output in tool args.
	enc.SetEscapeHTML(false)
	return &rpcTransport{
		in:      bufio.NewReaderSize(in, 64*1024),
		out:     out,
		enc:     enc,
		onEvent: onEvent,
	}
}

func (t *rpcTransport) readLoop(ctx context.Context) {
	for {
		// Check cancellation up front so a closed context can break the loop
		// even if the underlying read is blocked.
		if ctx.Err() != nil {
			return
		}
		line, err := t.readLine(ctx)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				// One record exceeded the size cap. readLine has already
				// drained up to the next newline, so framing is intact;
				// just log and move on.
				slog.Warn("pi: dropped oversized rpc line", "limit", 16*1024*1024)
				continue
			}
			if ctx.Err() == nil {
				t.cancelAll(fmt.Errorf("pi: rpc read closed: %w", err))
			}
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

// errLineTooLong is returned by readLine when a single JSONL record exceeds
// the safety limit. The caller (readLoop) treats it as a recoverable error:
// the oversized line is already drained from the buffer, so transport can
// skip it and continue parsing the next record.
var errLineTooLong = fmt.Errorf("pi: rpc line too long")

func (t *rpcTransport) readLine(ctx context.Context) ([]byte, error) {
	// Prevent unbounded memory growth if the subprocess emits an extremely large JSONL record.
	// Large tool outputs should be written to files and referenced instead.
	const maxLine = 16 * 1024 * 1024

	var line []byte
	for {
		frag, err := t.in.ReadSlice('\n')
		if len(line)+len(frag) > maxLine {
			// Drain until the next newline to re-sync framing, then return a
			// recoverable error so readLoop skips this record and continues
			// with subsequent ones — a single oversized line must NOT tear
			// down the entire RPC transport.
			for err == bufio.ErrBufferFull {
				_, err = t.in.ReadSlice('\n')
			}
			return nil, errLineTooLong
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
	v, ok := t.pending.LoadAndDelete(key)
	if !ok {
		slog.Debug("pi: unmatched rpc response", "id", key, "command", resp.Command)
		return
	}
	ch := v.(chan rpcOutcome)
	select {
	case ch <- rpcOutcome{resp: resp}:
	default:
	}
}

func (t *rpcTransport) cancelAll(err error) {
	t.pending.Range(func(k, v any) bool {
		ch := v.(chan rpcOutcome)
		select {
		case ch <- rpcOutcome{err: err}:
		default:
		}
		t.pending.Delete(k)
		return true
	})
}

func (t *rpcTransport) call(ctx context.Context, cmd map[string]any) (*rpcResponse, error) {
	id := t.nextID.Add(1)
	key := fmt.Sprintf("%d", id)
	cmd["id"] = key

	ch := make(chan rpcOutcome, 1)
	t.pending.Store(key, ch)

	if err := t.writeJSON(cmd); err != nil {
		t.pending.Delete(key)
		return nil, fmt.Errorf("pi: rpc write: %w", err)
	}

	select {
	case <-ctx.Done():
		t.pending.Delete(key)
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
