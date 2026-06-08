package pi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// piSession manages a multi-turn pi coding agent conversation.
// It runs a persistent `pi --mode rpc` subprocess and communicates via JSONL over stdin/stdout.
//
// Protocol reference (upstream): /data/code/node/pi/packages/coding-agent/docs/rpc.md
// (stdin: commands; stdout: responses + streamed events)
//
// Key requirements:
// - Stream assistant text as EventText (text_delta)
// - Emit EventToolUse / EventToolResult for tool execution events
// - Emit EventResult on agent_end (per user turn)
// - If the subprocess dies or pipes break, auto-restart and retry once.
type piSession struct {
	cmd      string
	workDir  string
	model    string
	thinking string
	extraEnv []string

	events chan core.Event

	ctx    context.Context
	cancel context.CancelFunc

	alive     atomic.Bool
	streaming atomic.Bool
	textSeen  atomic.Bool

	procMu     sync.Mutex
	proc       *exec.Cmd
	procCancel context.CancelFunc
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	tr         *rpcTransport
	wg         sync.WaitGroup

	sessionMu sync.RWMutex
	sessionID string

	thinkingBuf strings.Builder

	extMu     sync.Mutex
	extUIReqs map[string]extUIRequest

	// done is closed by Close() exactly once. emit() checks/sends on done
	// before touching s.events, so we never panic with "send on closed
	// channel" from a goroutine that outlives Close (e.g. the waiter in
	// startProcessLocked, which can race with Close during shutdown).
	done     chan struct{}
	doneOnce sync.Once

	// handshakeTimeout stores the timeout configured for this session so
	// that restart() — which is always a resume scenario — can honour the
	// user's value instead of silently falling back to the 30s resume default.
	handshakeTimeout time.Duration

	// Throttle for tool_execution_update events (long-running tool partial output).
	// We collapse bursts of partial results to ~5/s to avoid flooding the event
	// channel and starving out the actual result.
	toolUpdateMu sync.Mutex
	// lastToolUpdateAt is touched only under toolUpdateMu, so we never need
	// an atomic copy on the read side.
	lastToolUpdateAt time.Time

	// cleanMu + cleanScheduled implement a simple debounce: at most one
	// cleanAttachments goroutine is scheduled per debounceWindow, no matter
	// how many Send() calls happen in between. This prevents goroutine
	// explosions on chatty sessions that attach files frequently.
	cleanMu       sync.Mutex
	cleanScheduled bool
}

// debounceWindow is the minimum gap between two background attachment-cleanup
// runs. A single goroutine is plenty because stale files are not time-critical.
const debounceWindow = 5 * time.Minute

type extUIRequest struct {
	ID      string
	Method  string
	Title   string
	Message string
	Options []string
}

// handshakeTimeoutDefaultNew is the default timeout for the initial get_state
// handshake when starting a fresh pi session (no resume).
const handshakeTimeoutDefaultNew = 15 * time.Second

// handshakeTimeoutDefaultResume is the default timeout for get_state when
// resuming an existing pi session (--session or --continue). Resume sessions
// need more time because pi loads the JSONL history on startup.
const handshakeTimeoutDefaultResume = 30 * time.Second

// handshakeTimeoutMin is the absolute minimum allowed timeout. Values lower
// than this are clamped up to prevent flaky failures.
const handshakeTimeoutMin = 5 * time.Second

func newPiSession(ctx context.Context, cmd, workDir, model, thinking, resumeID string, extraEnv []string, handshakeTimeout time.Duration) (*piSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &piSession{
		cmd:       cmd,
		workDir:   workDir,
		model:     model,
		thinking:  thinking,
		extraEnv:  extraEnv,
		// Larger buffer reduces the chance that emit() blocks the readLoop
		// when the engine is briefly slow (e.g. platform API rate limits).
		events:           make(chan core.Event, 1024),
		ctx:              sessionCtx,
		cancel:           cancel,
		extUIReqs:        make(map[string]extUIRequest),
		done:             make(chan struct{}),
		handshakeTimeout: handshakeTimeout,
	}

	// Start process eagerly so StartSession fails fast if the binary is broken.
	if err := s.ensureRunning(sessionCtx, resumeID, handshakeTimeout); err != nil {
		_ = s.Close()
		return nil, err
	}

	s.alive.Store(true)
	return s, nil
}

func (s *piSession) ensureRunning(ctx context.Context, resumeID string, handshakeTimeout time.Duration) error {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	if s.proc != nil && s.stdin != nil && s.tr != nil {
		// If previously marked dead, respawn.
		if s.alive.Load() {
			return nil
		}
	}
	return s.startProcessLocked(ctx, resumeID, handshakeTimeout)
}

func (s *piSession) startProcessLocked(ctx context.Context, resumeID string, handshakeTimeout time.Duration) error {
	// Stop any old process.
	s.stopProcessLocked()

	procCtx, procCancel := context.WithCancel(ctx)
	s.procCancel = procCancel

	args := []string{"--mode", "rpc"}
	if strings.TrimSpace(resumeID) != "" {
		if resumeID == core.ContinueSession {
			args = append(args, "--continue")
		} else {
			args = append(args, "--session", resumeID)
		}
	}
	if strings.TrimSpace(s.model) != "" {
		args = append(args, "--model", strings.TrimSpace(s.model))
	}
	if strings.TrimSpace(s.thinking) != "" {
		args = append(args, "--thinking", strings.TrimSpace(s.thinking))
	}

	cmd := exec.CommandContext(procCtx, s.cmd, args...)
	cmd.Dir = s.workDir
	env := os.Environ()
	if len(s.extraEnv) > 0 {
		env = core.MergeEnv(env, s.extraEnv)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("pi: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pi: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("pi: stderr pipe: %w", err)
	}

	s.stdin = stdin
	s.stdout = stdout
	s.stderr = stderr
	s.proc = cmd
	s.tr = newRPCTransport(stdout, stdin, s.handleRPCEvent)

	if err := cmd.Start(); err != nil {
		s.stopProcessLocked()
		return fmt.Errorf("pi: start %s: %w", s.cmd, err)
	}

	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	// stderr logger (keep only a small tail to avoid unbounded memory usage)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(stderrDone)

		const maxTail = 8 * 1024
		tail := make([]byte, 0, maxTail)
		tmp := make([]byte, 4096)
		for {
			n, err := stderr.Read(tmp)
			if n > 0 {
				tail = append(tail, tmp[:n]...)
				if len(tail) > maxTail {
					tail = append([]byte(nil), tail[len(tail)-maxTail:]...)
				}
			}
			if err != nil {
				break
			}
		}
		msg := strings.TrimSpace(string(tail))
		if msg != "" && procCtx.Err() == nil {
			slog.Warn("pi: stderr", "stderr", truncStr(msg, 800))
		}
	}()

	// stdout reader / dispatcher
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(stdoutDone)
		s.tr.readLoop(procCtx)
	}()

	// Waiter: wait until pipes are drained, then reap the process.
	// (Calling cmd.Wait() early can close stdout/stderr pipes and race readers.)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-stdoutDone
		<-stderrDone
		err := cmd.Wait()
		if err != nil && procCtx.Err() == nil {
			slog.Error("pi: process exited", "error", err)
			s.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("pi exited: %w", err)})
			// Unblock the engine if it is waiting for the turn to finish.
			s.emit(core.Event{Type: core.EventResult, SessionID: s.CurrentSessionID(), Done: true})
		}
		s.alive.Store(false)
	}()

	// Handshake: verify RPC works and learn session ID.
	// Use the configured timeout or sensible defaults: resume sessions need
	// more time for pi to load JSONL history.
	timeout := handshakeTimeout
	if timeout <= 0 {
		if strings.TrimSpace(resumeID) != "" {
			timeout = handshakeTimeoutDefaultResume
		} else {
			timeout = handshakeTimeoutDefaultNew
		}
	}
	if timeout < handshakeTimeoutMin {
		timeout = handshakeTimeoutMin
	}
	handshakeCtx, cancel := context.WithTimeout(procCtx, timeout)
	defer cancel()
	if err := s.refreshState(handshakeCtx, s.tr); err != nil {
		s.stopProcessLocked()
		return err
	}

	s.alive.Store(true)
	return nil
}

func (s *piSession) stopProcessLocked() {
	if s.procCancel != nil {
		s.procCancel()
		s.procCancel = nil
	}
	// stdin/stdout/stderr will be closed by process termination.
	s.proc = nil
	s.stdin = nil
	s.stdout = nil
	s.stderr = nil
	s.tr = nil
	s.streaming.Store(false)
}

func (s *piSession) refreshState(ctx context.Context, tr *rpcTransport) error {
	if tr == nil {
		return fmt.Errorf("pi: rpc transport not running")
	}
	resp, err := tr.call(ctx, map[string]any{"type": "get_state"})
	if err != nil {
		return fmt.Errorf("pi: get_state: %w", err)
	}
	if err := resp.asError(); err != nil {
		return err
	}
	var data struct {
		SessionID string `json:"sessionId"`
	}
	if len(resp.Data) > 0 {
		_ = json.Unmarshal(resp.Data, &data)
	}
	if strings.TrimSpace(data.SessionID) != "" {
		s.sessionMu.Lock()
		s.sessionID = data.SessionID
		s.sessionMu.Unlock()
	}
	return nil
}

func (s *piSession) restart(ctx context.Context) error {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	resume := s.CurrentSessionID()
	if resume == "" {
		resume = core.ContinueSession
	}
	// restart is always a resume scenario: honour the user-configured
	// handshake timeout (falling back to the 30s resume default inside
	// startProcessLocked). Using 0 here would silently widen a user-set
	// 5s timeout back to 30s on every retry.
	return s.startProcessLocked(ctx, resume, s.handshakeTimeout)
}

func (s *piSession) rpcCall(ctx context.Context, cmd map[string]any) (*rpcResponse, error) {
	s.procMu.Lock()
	tr := s.tr
	s.procMu.Unlock()
	if tr == nil {
		return nil, fmt.Errorf("pi: rpc transport not running")
	}
	return tr.call(ctx, cmd)
}

func (s *piSession) rpcNotify(cmd map[string]any) error {
	s.procMu.Lock()
	tr := s.tr
	s.procMu.Unlock()
	if tr == nil {
		return fmt.Errorf("pi: rpc transport not running")
	}
	return tr.notify(cmd)
}

func (s *piSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if prompt == "" && len(images) == 0 && len(files) == 0 {
		return nil
	}
	if s.ctx.Err() != nil {
		return fmt.Errorf("pi: session closed")
	}

	// Attachments:
	// - RPC mode forbids @file args, so we pass images inline (base64) and save other files to disk.
	// - cleanAttachments now only purges stale files (>1h old) and runs in a
	//   goroutine so it never blocks the user-facing Send path. We use a
	//   debounce window to avoid spawning a fresh goroutine on every Send
	//   (chatty sessions would otherwise leak hundreds of short-lived goroutines
	//   doing redundant os.ReadDir+stat work).
	if len(files) > 0 {
		s.scheduleCleanup()
	}
	filePaths := core.SaveFilesToDisk(s.workDir, files)
	prompt = core.AppendFileRefs(prompt, filePaths)

	rpcImages := make([]map[string]any, 0, len(images))
	for _, img := range images {
		if len(img.Data) == 0 {
			continue
		}
		mime := img.MimeType
		if mime == "" {
			mime = "image/png"
		}
		rpcImages = append(rpcImages, map[string]any{
			"type":     "image",
			"data":     base64.StdEncoding.EncodeToString(img.Data),
			"mimeType": mime,
		})
	}

	if err := s.ensureRunning(s.ctx, s.CurrentSessionID(), 0); err != nil {
		return err
	}

	// If we're mid-streaming (e.g. /btw), steer.
	cmd := map[string]any{"type": "prompt", "message": prompt}
	if len(rpcImages) > 0 {
		cmd["images"] = rpcImages
	}
	if s.streaming.Load() {
		cmd["streamingBehavior"] = "steer"
	}

	// ★ OPTIMIZATION: fire-and-forget the prompt.
	// Previously we waited up to 60s for pi to acknowledge the prompt, but
	// the "prompt" response only confirms receipt — not completion. The actual
	// outcome is delivered via events (agent_start → text_delta* → agent_end).
	// Removing this wait slashes TTFT (time to first token) dramatically,
	// because Send now returns as soon as the bytes are written to stdin.
	//
	// Transient pipe errors are retried in-place; only a dead pipe triggers
	// a full process restart.
	if err := s.rpcNotify(cmd); err != nil {
		if s.isRetryableSendErr(err) {
			if restartErr := s.restart(s.ctx); restartErr == nil {
				return s.rpcNotify(cmd)
			}
		}
		return err
	}
	return nil
}

func (s *piSession) RespondPermission(requestID string, result core.PermissionResult) error {
	if requestID == "" {
		return nil
	}

	s.extMu.Lock()
	req, ok := s.extUIReqs[requestID]
	if ok {
		delete(s.extUIReqs, requestID)
	}
	s.extMu.Unlock()
	if !ok {
		// pi tool permission is not a thing; ignore unknown IDs.
		return nil
	}

	// Default: cancelled.
	resp := map[string]any{"type": "extension_ui_response", "id": requestID, "cancelled": true}

	if strings.EqualFold(result.Behavior, "allow") {
		ans := extractAskQuestionAnswer(result)
		switch req.Method {
		case "confirm":
			confirmed := isTruthyAnswer(ans)
			resp = map[string]any{"type": "extension_ui_response", "id": requestID, "confirmed": confirmed}
		case "select":
			resp = map[string]any{"type": "extension_ui_response", "id": requestID, "value": ans}
		case "input", "editor":
			resp = map[string]any{"type": "extension_ui_response", "id": requestID, "value": ans}
		default:
			resp = map[string]any{"type": "extension_ui_response", "id": requestID, "value": ans}
		}
	}

	return s.rpcNotify(resp)
}

func extractAskQuestionAnswer(result core.PermissionResult) string {
	if result.UpdatedInput == nil {
		return ""
	}
	answersAny, ok := result.UpdatedInput["answers"]
	if !ok {
		return ""
	}
	answers, ok := answersAny.(map[string]any)
	if !ok {
		return ""
	}
	if v, ok := answers["0"]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	// Fallback: pick any.
	for _, v := range answers {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

func isTruthyAnswer(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, ".!")
	switch s {
	case "y", "yes", "true", "1", "ok", "confirm", "allow", "允许", "同意", "是":
		return true
	case "n", "no", "false", "0", "deny", "拒绝", "否", "不":
		return false
	default:
		// Heuristic: treat explicit "no"-like answers as false; otherwise true.
		return s != "" && s != "cancel" && s != "取消"
	}
}

func (s *piSession) Events() <-chan core.Event { return s.events }

func (s *piSession) CurrentSessionID() string {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	return s.sessionID
}

func (s *piSession) Alive() bool { return s.alive.Load() }

func (s *piSession) Close() error {
	// Close is idempotent; mark the session as not-alive so any late emit
	// (from the waiter goroutine racing with shutdown) can short-circuit.
	s.alive.Store(false)
	// Signal done first. After this, emit() will not touch s.events again,
	// so the eventual close(s.events) cannot panic a concurrent send.
	s.doneOnce.Do(func() { close(s.done) })

	s.cancel()

	s.procMu.Lock()
	s.stopProcessLocked()
	s.procMu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		slog.Warn("pi: close timed out")
	}

	// By this point all wg-tracked goroutines have either finished or been
	// forcibly abandoned. No new emit() can reach s.events, so it is safe
	// to close it.
	close(s.events)
	return nil
}

// handleRPCEvent maps pi RPC stdout events into cc-connect core.Events.
//
// Reference: pi-desktop AgentManager.handlePiEvent + pi 0.78+ RPC protocol.
//
// Key differences from older pi versions:
//   - agent_end carries willRetry=true when an auto-retry is scheduled. In that
//     case we must NOT emit EventResult — another agent_start will follow.
//   - message_update now has text_start/text_end, thinking_start, toolcall_*
//     sub-events. The new ones are informational; only *_delta carries data.
//   - extension_error and auto_retry_* / compaction_* lifecycle events exist.
func (s *piSession) handleRPCEvent(env map[string]any) {
	typ, _ := env["type"].(string)

	switch typ {
	case "agent_start":
		s.streaming.Store(true)
		s.textSeen.Store(false)

	case "agent_end":
		s.streaming.Store(false)

		// willRetry: pi auto-retry will fire another agent_start. Hold off
		// finalizing the cc-connect turn until the retry completes.
		if willRetry, _ := env["willRetry"].(bool); willRetry {
			if msg := buildRetryThinkingMessage(env); msg != "" {
				s.emit(core.Event{Type: core.EventThinking, Content: msg})
			}
			return
		}

		// Surface an error if pi reported one.
		if errMsg := extractAgentEndError(env); errMsg != "" {
			s.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("pi: %s", errMsg)})
		}

		// Emit EventResult to end this turn.
		content := ""
		if !s.textSeen.Load() {
			content, _ = extractAssistantTextFromAgentEnd(env)
		}
		inTok, outTok := extractTokensFromAgentEnd(env)
		s.emit(core.Event{Type: core.EventResult, Content: content, SessionID: s.CurrentSessionID(), Done: true, InputTokens: inTok, OutputTokens: outTok})

	case "message_update":
		s.handleMessageUpdate(env)

	case "tool_execution_start":
		toolName, _ := env["toolName"].(string)
		args, _ := env["args"].(map[string]any)
		toolInput := summarizeToolArgs(args)
		s.emit(core.Event{Type: core.EventToolUse, ToolName: toolName, ToolInput: toolInput})

	case "tool_execution_end":
		toolName, _ := env["toolName"].(string)
		isErr, _ := env["isError"].(bool)
		result, _ := env["result"].(map[string]any)
		toolOut := extractToolResultText(result)
		status := "completed"
		success := true
		if isErr {
			status = "failed"
			success = false
		}
		s.emit(core.Event{Type: core.EventToolResult, ToolName: toolName, ToolResult: truncStr(toolOut, 800), ToolStatus: status, ToolSuccess: &success})

	case "extension_ui_request":
		s.handleExtensionUIRequest(env)

	case "tool_execution_update":
		// Pi streams partial output for long-running tools (notably bash).
		// Forward a trimmed version as EventThinking so the user sees real-time
		// progress — without this, a 30s `go test` looks identical to a hung agent.
		//
		// ★ OPTIMIZATION: throttle to ~5 events/sec. A 30s `go test` would
		// otherwise emit hundreds of partial results, flooding the events
		// channel and starving out higher-priority events (text deltas, results).
		partial, _ := env["partialResult"].(map[string]any)
		if partial == nil {
			return
		}
		txt := strings.TrimSpace(extractToolResultText(partial))
		if txt == "" {
			return
		}
		const toolUpdateInterval = 200 * time.Millisecond
		// Single critical section: read+check+update atomically. The previous
		// double-lock version had a TOCTOU window where two goroutines could
		// both see the interval as elapsed and both emit, defeating the throttle.
		s.toolUpdateMu.Lock()
		if !s.lastToolUpdateAt.IsZero() && time.Since(s.lastToolUpdateAt) < toolUpdateInterval {
			s.toolUpdateMu.Unlock()
			return
		}
		s.lastToolUpdateAt = time.Now()
		s.toolUpdateMu.Unlock()
		s.emit(core.Event{Type: core.EventThinking, Content: truncStr(txt, 400)})

	case "extension_error":
		errMsg, _ := env["error"].(string)
		if errMsg == "" {
			errMsg = "extension error"
		}
		s.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("pi: %s", errMsg)})

	case "auto_retry_start":
		// Show retry attempts as thinking so the user knows the agent hasn't
		// stalled. Without this, a slow retry looks identical to a hung turn.
		attempt, _ := env["attempt"].(float64)
		maxAttempts, _ := env["maxAttempts"].(float64)
		errMsg, _ := env["errorMessage"].(string)
		msg := fmt.Sprintf("auto-retry %d/%d", int(attempt), int(maxAttempts))
		if errMsg != "" {
			msg += ": " + truncStr(errMsg, 200)
		}
		s.emit(core.Event{Type: core.EventThinking, Content: msg})

	case "auto_retry_end":
		// No special action; agent_end will follow.

	case "compaction_start":
		s.emit(core.Event{Type: core.EventThinking, Content: "compacting context…"})

	case "compaction_end":
		// Compaction is informational; the next agent_end will close the turn.
		if errMsg, _ := env["errorMessage"].(string); errMsg != "" {
			s.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("pi: compaction failed: %s", errMsg)})
		}

	default:
		// ignore other lifecycle events (turn_start/end, message_start/end,
		// tool_execution_update, queue_update, session_info_changed, …)
	}
}

func (s *piSession) handleMessageUpdate(env map[string]any) {
	ame, _ := env["assistantMessageEvent"].(map[string]any)
	if ame == nil {
		return
	}
	st, _ := ame["type"].(string)
	switch st {
	case "text_start", "text_end":
		// Informational boundary events; nothing to emit.
	case "text_delta":
		delta, _ := ame["delta"].(string)
		if delta != "" {
			s.textSeen.Store(true)
			s.emit(core.Event{Type: core.EventText, Content: delta})
		}
	case "thinking_start":
		// Reset accumulator for a new thinking block.
		s.thinkingBuf.Reset()
	case "thinking_delta":
		delta, _ := ame["delta"].(string)
		if delta != "" {
			s.thinkingBuf.WriteString(delta)
		}
	case "thinking_end":
		// `content` may carry the final thinking text; fall back to accumulated deltas.
		final := ""
		if c, ok := ame["content"].(string); ok {
			final = c
		}
		if final == "" && s.thinkingBuf.Len() > 0 {
			final = s.thinkingBuf.String()
		}
		s.thinkingBuf.Reset()
		if final != "" {
			s.emit(core.Event{Type: core.EventThinking, Content: stripAnsi(final)})
		}
	case "toolcall_start", "toolcall_delta", "toolcall_end":
		// Inline tool-call metadata streamed before tool_execution_start fires.
		// We don't surface these to avoid duplicating the eventual
		// tool_execution_start event, which carries the canonical name+args.
	case "error":
		reason, _ := ame["reason"].(string)
		if reason := strings.TrimSpace(reason); reason != "" {
			s.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("pi: %s", reason)})
		}
	}
}

func (s *piSession) handleExtensionUIRequest(env map[string]any) {
	id, _ := env["id"].(string)
	method, _ := env["method"].(string)
	if id == "" || method == "" {
		return
	}

	method = strings.TrimSpace(method)
	// Per pi RPC spec, only dialog methods block and require a response.
	// Fire-and-forget methods must NOT be translated into AskUserQuestion, otherwise
	// cc-connect will block waiting for user input and the agent turn will stall.
	switch method {
	case "select", "confirm", "input", "editor":
		// handled below
	default:
		// Fire-and-forget: notify/setStatus/setWidget/setTitle/set_editor_text/... — ignore.
		return
	}

	title, _ := env["title"].(string)
	msg, _ := env["message"].(string)
	var options []string
	if raw, ok := env["options"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				options = append(options, s)
			}
		}
	}

	s.extMu.Lock()
	s.extUIReqs[id] = extUIRequest{ID: id, Method: method, Title: title, Message: msg, Options: options}
	s.extMu.Unlock()

	q := core.UserQuestion{Question: title}
	if q.Question == "" {
		q.Question = method
	}
	if msg != "" {
		q.Header = msg
	}

	switch method {
	case "confirm":
		q.Options = []core.UserQuestionOption{{Label: "Yes"}, {Label: "No"}}
	case "select":
		for _, opt := range options {
			q.Options = append(q.Options, core.UserQuestionOption{Label: opt})
		}
	case "input", "editor":
		// free-form: no options; engine will accept text.
	}

	s.emit(core.Event{Type: core.EventPermissionRequest, ToolName: "AskUserQuestion", RequestID: id, ToolInput: q.Question, ToolInputRaw: map[string]any{"id": id, "method": method}, Questions: []core.UserQuestion{q}})
}

func extractToolResultText(result map[string]any) string {
	if result == nil {
		return ""
	}
	content, _ := result["content"].([]any)
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			if t, ok := m["text"].(string); ok && t != "" {
				return t
			}
		}
	}
	return ""
}

func summarizeToolArgs(args map[string]any) string {
	if args == nil {
		return ""
	}
	for _, k := range []string{"description", "command", "file_path", "path", "pattern", "query", "url"} {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	b, _ := json.Marshal(args)
	return truncStr(string(b), 240)
}

func extractAssistantTextFromAgentEnd(env map[string]any) (string, bool) {
	msgsAny, _ := env["messages"].([]any)
	for i := len(msgsAny) - 1; i >= 0; i-- {
		m, _ := msgsAny[i].(map[string]any)
		if m == nil {
			continue
		}
		if role, _ := m["role"].(string); role != "assistant" {
			continue
		}
		var sb strings.Builder
		if content, ok := m["content"].([]any); ok {
			for _, c := range content {
				item, _ := c.(map[string]any)
				if item == nil {
					continue
				}
				if typ, _ := item["type"].(string); typ == "text" {
					if t, _ := item["text"].(string); t != "" {
						sb.WriteString(t)
					}
				}
			}
		}
		out := strings.TrimSpace(sb.String())
		return out, out != ""
	}
	return "", false
}

// extractAgentEndError mirrors pi-desktop's AgentManager.handlePiEvent error
// extraction cascade: top-level errorMessage → last message with stopReason=error →
// top-level error → last assistant content block of type=error.
// Returns "" when the agent ended cleanly.
func extractAgentEndError(env map[string]any) string {
	stopReason, _ := env["stopReason"].(string)

	if msg, _ := env["errorMessage"].(string); strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}

	msgsAny, _ := env["messages"].([]any)
	// Scan for messages explicitly marked as error stops.
	for i := len(msgsAny) - 1; i >= 0; i-- {
		m, _ := msgsAny[i].(map[string]any)
		if m == nil {
			continue
		}
		sr, _ := m["stopReason"].(string)
		if sr != "error" {
			continue
		}
		if msg, _ := m["errorMessage"].(string); strings.TrimSpace(msg) != "" {
			return strings.TrimSpace(msg)
		}
	}

	if msg, _ := env["error"].(string); strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}

	// Last assistant message's type=error content block.
	for i := len(msgsAny) - 1; i >= 0; i-- {
		m, _ := msgsAny[i].(map[string]any)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		if role != "assistant" {
			continue
		}
		content, _ := m["content"].([]any)
		for _, c := range content {
			item, _ := c.(map[string]any)
			if item == nil {
				continue
			}
			if typ, _ := item["type"].(string); typ == "error" {
				if t, _ := item["text"].(string); strings.TrimSpace(t) != "" {
					return strings.TrimSpace(t)
				}
				if t, _ := item["message"].(string); strings.TrimSpace(t) != "" {
					return strings.TrimSpace(t)
				}
			}
		}
	}

	if stopReason == "error" {
		return "agent returned unknown error"
	}
	return ""
}

// buildRetryThinkingMessage produces a short status string for an agent_end
// with willRetry=true, so the user understands why the turn hasn't finalized.
func buildRetryThinkingMessage(env map[string]any) string {
	if msg := extractAgentEndError(env); msg != "" {
		return "retrying after error: " + truncStr(msg, 200)
	}
	return "agent will retry…"
}

// stripAnsi removes ANSI escape sequences (terminal colors) from streaming
// thinking text. Models occasionally emit these; sending them to chat platforms
// produces ugly output.
func stripAnsi(s string) string {
	var sb strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); {
		if runes[i] != 0x1b {
			sb.WriteRune(runes[i])
			i++
			continue
		}
		// Potential CSI sequence: ESC [ params final-letter
		// Check if we have enough characters for ESC [
		if i+1 >= len(runes) || runes[i+1] != '[' {
			// Not a valid CSI sequence; emit the ESC as-is
			sb.WriteRune(runes[i])
			i++
			continue
		}
		// Look ahead to find a valid CSI final character within a reasonable limit.
		// We recognize common CSI final characters:
		// - A-Z: most cursor, erase, scroll commands
		// - h: SM (Set Mode, including private modes like ?25 for cursor visibility)
		// - l: RM (Reset Mode)
		// - m: SGR (Select Graphic Rendition, colors/styles)
		// - r: DECSTBM (Set Top and Bottom Margins)
		const maxLookahead = 32
		finalPos := -1
		for j := i + 2; j < len(runes) && j < i+2+maxLookahead; j++ {
			r := runes[j]
			// Check if this rune is a valid CSI final character
			isCSIChar := (r >= 'A' && r <= 'Z') || r == 'h' || r == 'l' || r == 'm' || r == 'r'
			if isCSIChar {
				finalPos = j
				break
			}
			// If we encounter a non-CSI byte (outside param/intermediate range),
			// this isn't a valid CSI sequence.
			isParamByte := (r >= 0x30 && r <= 0x3F) // 0-9, :, ;, <, =, >, ?
			isIntermediateByte := (r >= 0x20 && r <= 0x2F) // space to /
			if !isParamByte && !isIntermediateByte {
				break
			}
		}
		if finalPos == -1 {
			// No valid CSI final character found within lookahead limit;
			// emit ESC and continue (the '[' will be handled in next iteration if valid)
			sb.WriteRune(runes[i])
			i++
			continue
		}
		// Valid CSI sequence: skip from ESC to final character (inclusive)
		i = finalPos + 1
	}
	return sb.String()
}

func extractTokensFromAgentEnd(env map[string]any) (int, int) {
	msgsAny, _ := env["messages"].([]any)
	for i := len(msgsAny) - 1; i >= 0; i-- {
		m, _ := msgsAny[i].(map[string]any)
		if m == nil {
			continue
		}
		if role, _ := m["role"].(string); role != "assistant" {
			continue
		}
		usage, _ := m["usage"].(map[string]any)
		if usage == nil {
			return 0, 0
		}
		in := intFromAny(usage["input"])
		out := intFromAny(usage["output"])
		return in, out
	}
	return 0, 0
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	default:
		return 0
	}
}

// emit sends an event to the engine's consumer.
//
// High-priority events (Result, Error, PermissionRequest) ALWAYS block
// until delivered — dropping them would lose the user's turn or stall the
// engine indefinitely.
//
// Low-priority events (Text, ToolUse, ToolResult, Thinking) use a
// non-blocking send. If the consumer (engine) is briefly slow — e.g. a
// platform API rate limit, a slow UpdateMessage — the events channel
// could fill up. Blocking in that case would back up the readLoop and
// stall the entire pipeline (including the matching of pending RPC
// responses). Dropping a "thinking" line is harmless; the user just
// sees one fewer progress blip.
//
// All paths check s.done first: once Close() is in progress, emit becomes
// a no-op so we never race with close(s.events) and panic.
func (s *piSession) emit(evt core.Event) {
	if evt.Type == "" {
		return
	}
	select {
	case <-s.done:
		return
	default:
	}
	switch evt.Type {
	case core.EventResult, core.EventError, core.EventPermissionRequest:
		select {
		case s.events <- evt:
		case <-s.done:
		case <-s.ctx.Done():
		}
	default:
		select {
		case s.events <- evt:
		case <-s.done:
		default:
			slog.Debug("pi: drop low-priority event", "type", evt.Type)
		}
	}
}

// scheduleCleanup debounces background attachment cleanup so a chatty
// session does not spawn one goroutine per Send. At most one cleanup
// runs per debounceWindow. The actual work is done by cleanAttachments,
// which is also safe to call directly (e.g. from Close) without the
// debounce.
func (s *piSession) scheduleCleanup() {
	s.cleanMu.Lock()
	if s.cleanScheduled {
		s.cleanMu.Unlock()
		return
	}
	s.cleanScheduled = true
	s.cleanMu.Unlock()

	go func() {
		// Wait for the debounce window to settle so a burst of Sends
		// coalesces into a single cleanup pass.
		timer := time.NewTimer(debounceWindow)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.done:
			// Shutdown: drop the scheduled cleanup. New sessions get a
			// fresh s.cleanScheduled=false because this is a per-session
			// field; an old session being torn down never matters.
			s.cleanMu.Lock()
			s.cleanScheduled = false
			s.cleanMu.Unlock()
			return
		}
		cleanAttachments(s.workDir)
		s.cleanMu.Lock()
		s.cleanScheduled = false
		s.cleanMu.Unlock()
	}()
}

// cleanAttachments removes attachment files older than the retention window.
// Called from a goroutine on the Send path to keep I/O off the hot path.
func cleanAttachments(workDir string) {
	attachDir := filepath.Join(workDir, ".cc-connect", "attachments")
	entries, err := os.ReadDir(attachDir)
	if err != nil {
		return
	}
	threshold := time.Now().Add(-1 * time.Hour)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Keep recent files (the model may still be reading them). Only purge
		// files that are clearly stale, so a busy session doesn't pay for
		// repeated unlink() syscalls.
		if info.ModTime().Before(threshold) {
			_ = os.Remove(filepath.Join(attachDir, e.Name()))
		}
	}
}

func truncStr(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}

func (s *piSession) isRetryableSendErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return true
	}
	return strings.Contains(err.Error(), "broken pipe")
}
