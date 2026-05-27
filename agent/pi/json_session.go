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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// piJSONSession runs pi in single-shot JSON mode (pi --mode json).
// Each Send spawns a new subprocess, streams JSON events from stdout, then exits.
// Session continuity is provided by reusing pi's session ID via --session <id>.
//
// Notes (pi upstream): --mode json maps to print-mode.ts and emits JSON events
// (plus a session header) then exits.
// Source: /data/code/node/pi/packages/coding-agent/src/modes/print-mode.ts
//
// This mode avoids long-lived subprocess state and the RPC extension UI dialog protocol.
// It also means fire-and-forget UI updates (setWidget/setStatus) are typically no-ops
// because extensions run with a no-op UI context in print mode.
//
// This session implementation MUST always emit an EventResult (Done=true) per Send,
// otherwise the engine will wait indefinitely.

type piJSONSession struct {
	cmd      string
	workDir  string
	model    string
	thinking string
	extraEnv []string

	useContinue bool

	events chan core.Event

	ctx    context.Context
	cancel context.CancelFunc

	alive atomic.Bool

	sessionMu sync.RWMutex
	sessionID string
}

func newPiJSONSession(ctx context.Context, cmd, workDir, model, thinking, resumeID string, extraEnv []string) (*piJSONSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &piJSONSession{
		cmd:         cmd,
		workDir:     workDir,
		model:       model,
		thinking:    thinking,
		extraEnv:    extraEnv,
		events:      make(chan core.Event, 256),
		ctx:         sessionCtx,
		cancel:      cancel,
		sessionID:   strings.TrimSpace(resumeID),
		useContinue: strings.TrimSpace(resumeID) == core.ContinueSession,
	}
	s.alive.Store(true)
	return s, nil
}

func (s *piJSONSession) Events() <-chan core.Event { return s.events }

func (s *piJSONSession) CurrentSessionID() string {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	return s.sessionID
}

func (s *piJSONSession) Alive() bool { return s.alive.Load() && s.ctx.Err() == nil }

func (s *piJSONSession) Close() error {
	if !s.alive.CompareAndSwap(true, false) {
		return nil
	}
	s.cancel()
	close(s.events)
	return nil
}

func (s *piJSONSession) RespondPermission(_ string, _ core.PermissionResult) error {
	// JSON print mode does not support interactive extension UI dialogs.
	return nil
}

func (s *piJSONSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if prompt == "" && len(images) == 0 && len(files) == 0 {
		return nil
	}
	if s.ctx.Err() != nil {
		return fmt.Errorf("pi: session closed")
	}

	// Avoid embedding file contents into the prompt (token blow-up). Save files locally and reference paths.
	cleanAttachments(s.workDir)
	filePaths := core.SaveFilesToDisk(s.workDir, files)
	prompt = core.AppendFileRefs(prompt, filePaths)

	// Write images to disk and pass as @file args so pi attaches them as ImageContent.
	var fileArgs []string
	if len(images) > 0 {
		attachDir := filepath.Join(s.workDir, ".cc-connect", "attachments")
		_ = os.MkdirAll(attachDir, 0o755)
		for i, img := range images {
			if len(img.Data) == 0 {
				continue
			}
			ext := ".png"
			mt := strings.ToLower(strings.TrimSpace(img.MimeType))
			switch mt {
			case "image/jpeg", "image/jpg":
				ext = ".jpg"
			case "image/webp":
				ext = ".webp"
			}
			path := filepath.Join(attachDir, fmt.Sprintf("img_%d%s", i+1, ext))
			if err := os.WriteFile(path, img.Data, 0o644); err != nil {
				return fmt.Errorf("pi: save image: %w", err)
			}
			fileArgs = append(fileArgs, "@"+path)
		}
	}

	args := []string{"--mode", "json"}
	if sid := strings.TrimSpace(s.CurrentSessionID()); sid != "" {
		args = append(args, "--session", sid)
	} else if s.useContinue {
		// Continue the most recent pi session in this workDir.
		// (Useful when cc-connect doesn't have a persisted session ID yet.)
		args = append(args, "--continue")
	}
	if m := strings.TrimSpace(s.model); m != "" {
		args = append(args, "--model", m)
	}
	if th := strings.TrimSpace(s.thinking); th != "" {
		args = append(args, "--thinking", th)
	}
	args = append(args, fileArgs...)
	args = append(args, prompt)

	cmd := exec.CommandContext(s.ctx, s.cmd, args...)
	cmd.Dir = s.workDir
	cmd.Env = core.MergeEnv(os.Environ(), s.extraEnv)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pi: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("pi: stderr pipe: %w", err)
	}

	start := time.Now()
	slog.Info("pi: spawn json mode", "args", redactPiArgs(args), "work_dir", s.workDir)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("pi: start: %w", err)
	}

	stderrTail := &tailBuffer{max: 8 * 1024}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderrTail, stderr)
	}()

	resultEmitted := false
	textSeen := false
	thinkingBuf := strings.Builder{}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var env map[string]any
		if err := json.Unmarshal(line, &env); err != nil {
			slog.Debug("pi: json mode skip non-json line", "line", string(line))
			continue
		}
		typ, _ := env["type"].(string)
		switch typ {
		case "session":
			if id, _ := env["id"].(string); strings.TrimSpace(id) != "" {
				s.sessionMu.Lock()
				s.sessionID = id
				s.sessionMu.Unlock()
			}
		case "agent_start":
			s.emit(core.Event{Type: core.EventThinking, Content: "pi: started"})
		case "message_update":
			ame, _ := env["assistantMessageEvent"].(map[string]any)
			if ame != nil {
				st, _ := ame["type"].(string)
				switch st {
				case "text_delta":
					delta, _ := ame["delta"].(string)
					if delta != "" {
						textSeen = true
						s.emit(core.Event{Type: core.EventText, Content: delta})
					}
				case "thinking_delta":
					delta, _ := ame["delta"].(string)
					if delta != "" {
						thinkingBuf.WriteString(delta)
					}
				case "thinking_end":
					if thinkingBuf.Len() > 0 {
						s.emit(core.Event{Type: core.EventThinking, Content: thinkingBuf.String()})
						thinkingBuf.Reset()
					}
				case "error":
					reason, _ := ame["reason"].(string)
					reason = strings.TrimSpace(reason)
					if reason == "" {
						reason = "message_update error"
					}
					s.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("pi: %s", reason)})
				}
			}
		case "tool_execution_start":
			toolName, _ := env["toolName"].(string)
			args, _ := env["args"].(map[string]any)
			s.emit(core.Event{Type: core.EventToolUse, ToolName: toolName, ToolInput: summarizeToolArgs(args)})
		case "tool_execution_update":
			partial, _ := env["partialResult"].(map[string]any)
			if partial != nil {
				if txt := strings.TrimSpace(extractToolResultText(partial)); txt != "" {
					s.emit(core.Event{Type: core.EventThinking, Content: truncStr(txt, 400)})
				}
			}
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
		case "agent_end":
			// End of this turn.
			inTok, outTok := extractTokensFromAgentEnd(env)
			content := ""
			if !textSeen {
				content, _ = extractAssistantTextFromAgentEnd(env)
			}
			s.emit(core.Event{Type: core.EventResult, Content: content, SessionID: s.CurrentSessionID(), Done: true, InputTokens: inTok, OutputTokens: outTok})
			resultEmitted = true
		}
	}

	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	<-stderrDone

	elapsed := time.Since(start)
	if scanErr != nil {
		slog.Warn("pi: json mode scan", "error", scanErr, "elapsed", elapsed)
	}
	if waitErr != nil {
		errText := strings.TrimSpace(stderrTail.String())
		if errText != "" {
			waitErr = fmt.Errorf("%w: %s", waitErr, truncStr(errText, 800))
		}
		s.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("pi: process failed: %w", waitErr)})
	}

	// Ensure the engine unblocks.
	if !resultEmitted {
		s.emit(core.Event{Type: core.EventResult, SessionID: s.CurrentSessionID(), Done: true})
	}

	// Return errors to Send caller (engine will show MsgError), but only after emitting events.
	if waitErr != nil {
		return waitErr
	}
	if scanErr != nil {
		return scanErr
	}
	return nil
}

func (s *piJSONSession) emit(evt core.Event) {
	if evt.Type == "" {
		return
	}
	select {
	case s.events <- evt:
	case <-s.ctx.Done():
	}
}

// tailBuffer keeps the last N bytes written.
// It is an io.Writer.
type tailBuffer struct {
	max int
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func redactPiArgs(args []string) []string {
	// Don't leak secrets if they ever appear in args (generally they shouldn't).
	out := make([]string, 0, len(args))
	skipNext := false
	for i, a := range args {
		if skipNext {
			skipNext = false
			out = append(out, "***")
			continue
		}
		if a == "--api-key" && i+1 < len(args) {
			out = append(out, a)
			skipNext = true
			continue
		}
		out = append(out, a)
	}
	return out
}

func isRetryableJSONModeErr(err error) bool {
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
