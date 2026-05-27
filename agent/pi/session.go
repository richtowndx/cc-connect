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

	turnMu   sync.Mutex
	textSeen bool

	thinkingBuf strings.Builder

	extMu     sync.Mutex
	extUIReqs map[string]extUIRequest
}

type extUIRequest struct {
	ID      string
	Method  string
	Title   string
	Message string
	Options []string
}

func newPiSession(ctx context.Context, cmd, workDir, model, _mode, thinking, resumeID string, extraEnv []string) (*piSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &piSession{
		cmd:       cmd,
		workDir:   workDir,
		model:     model,
		thinking:  thinking,
		extraEnv:  extraEnv,
		events:    make(chan core.Event, 128),
		ctx:       sessionCtx,
		cancel:    cancel,
		extUIReqs: make(map[string]extUIRequest),
	}

	// Start process eagerly so StartSession fails fast if the binary is broken.
	if err := s.ensureRunning(sessionCtx, resumeID); err != nil {
		_ = s.Close()
		return nil, err
	}

	s.alive.Store(true)
	return s, nil
}

func (s *piSession) ensureRunning(ctx context.Context, resumeID string) error {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	if s.proc != nil && s.stdin != nil && s.tr != nil {
		// If previously marked dead, respawn.
		if s.alive.Load() {
			return nil
		}
	}
	return s.startProcessLocked(ctx, resumeID)
}

func (s *piSession) startProcessLocked(ctx context.Context, resumeID string) error {
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
	handshakeCtx, cancel := context.WithTimeout(procCtx, 5*time.Second)
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
	return s.startProcessLocked(ctx, resume)
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
	cleanAttachments(s.workDir)

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

	if err := s.ensureRunning(s.ctx, s.CurrentSessionID()); err != nil {
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

	ctx, cancel := context.WithTimeout(s.ctx, 60*time.Second)
	defer cancel()

	resp, err := s.rpcCall(ctx, cmd)
	if err != nil {
		// Attempt one restart + retry.
		if restartErr := s.restart(s.ctx); restartErr == nil {
			resp, err = s.rpcCall(ctx, cmd)
		}
	}
	if err != nil {
		return err
	}
	if err := resp.asError(); err != nil {
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
	if !s.alive.CompareAndSwap(true, false) {
		// still stop process and close events only once
	}
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

	select {
	case <-s.events:
		// drain if someone wrote after cancel; best-effort
	default:
	}
	close(s.events)
	return nil
}

// handleRPCEvent maps pi RPC stdout events into cc-connect core.Events.
func (s *piSession) handleRPCEvent(env map[string]any) {
	typ, _ := env["type"].(string)

	switch typ {
	case "agent_start":
		s.streaming.Store(true)
		s.turnMu.Lock()
		s.textSeen = false
		s.turnMu.Unlock()

	case "agent_end":
		s.streaming.Store(false)
		// Emit EventResult to end this turn.
		content := ""
		s.turnMu.Lock()
		seen := s.textSeen
		s.turnMu.Unlock()
		if !seen {
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

	default:
		// ignore other lifecycle events
	}
}

func (s *piSession) handleMessageUpdate(env map[string]any) {
	ame, _ := env["assistantMessageEvent"].(map[string]any)
	if ame == nil {
		return
	}
	st, _ := ame["type"].(string)
	switch st {
	case "text_delta":
		delta, _ := ame["delta"].(string)
		if delta != "" {
			s.turnMu.Lock()
			s.textSeen = true
			s.turnMu.Unlock()
			s.emit(core.Event{Type: core.EventText, Content: delta})
		}
	case "thinking_delta":
		delta, _ := ame["delta"].(string)
		if delta != "" {
			s.thinkingBuf.WriteString(delta)
		}
	case "thinking_end":
		if s.thinkingBuf.Len() > 0 {
			s.emit(core.Event{Type: core.EventThinking, Content: s.thinkingBuf.String()})
			s.thinkingBuf.Reset()
		}
	}
}

func (s *piSession) handleExtensionUIRequest(env map[string]any) {
	id, _ := env["id"].(string)
	method, _ := env["method"].(string)
	if id == "" || method == "" {
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
	default:
		for _, opt := range options {
			q.Options = append(q.Options, core.UserQuestionOption{Label: opt})
		}
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

func (s *piSession) emit(evt core.Event) {
	if evt.Type == "" {
		return
	}
	select {
	case s.events <- evt:
	case <-s.ctx.Done():
	}
}

func cleanAttachments(workDir string) {
	attachDir := filepath.Join(workDir, ".cc-connect", "attachments")
	entries, err := os.ReadDir(attachDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
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
