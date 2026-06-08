package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("pi", New)
}

// Agent drives the pi coding agent CLI.
//
// Two execution modes are supported:
//
//   - "rpc" (default): runs a persistent `pi --mode rpc` subprocess and multiplexes
//     multi-turn conversations over JSONL on stdin/stdout. This is the recommended
//     mode for complex agent tasks — it supports streaming, abort, extension UI
//     dialogs, auto-retry, compaction, and all upstream pi features.
//
//   - "json": single-shot `pi --mode json` per Send. Simpler but cannot handle
//     extension UI dialogs, complex tool chains, or auto-retry. Kept as a
//     fallback for older pi versions or restricted environments.
//
// The execution mode is selected via the `exec_mode` option in
// [projects.agent.options]. We deliberately do NOT read the generic `mode` key —
// that key is reserved for permission modes (default/acceptEdits/yolo/...) by
// convention across all cc-connect agents. pi does not currently support
// permission modes, so `mode` is silently ignored.
type Agent struct {
	cmd      string // path to pi binary
	workDir  string
	model    string
	thinking string // reasoning effort: off, minimal, low, medium, high, xhigh
	mode     string // "rpc" (default) or "json"
	// If true, when cc-connect doesn't have a persisted session ID yet, start
	// with pi's --continue semantics (resume latest session in workDir).
	// WARNING: this may merge contexts across different cc-connect sessions that share the same workDir.
	continueOnEmpty bool
	// Timeout for the initial get_state handshake when starting/reconnecting to
	// a pi RPC process. Larger sessions may need more time for pi to load its
	// JSONL history. Zero means use defaults (15s for new, 30s for resume).
	handshakeTimeout time.Duration
	sessionEnv       []string
	mu               sync.Mutex
}

func New(opts map[string]any) (core.Agent, error) {
	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}
	model, _ := opts["model"].(string)
	continueOnEmpty := false
	if v, ok := opts["continue"].(bool); ok {
		continueOnEmpty = v
	} else if v, ok := opts["continue"].(string); ok {
		continueOnEmpty = strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
	}

	cmd, _ := opts["cmd"].(string)
	if cmd == "" {
		// Backwards-compatible alias
		cmd, _ = opts["command"].(string)
	}
	if cmd == "" {
		cmd = "pi"
	}

	mode := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", opts["exec_mode"])))
	if mode == "" || mode == "<nil>" {
		mode = "rpc"
	}
	if mode != "rpc" && mode != "json" {
		return nil, fmt.Errorf("pi: invalid exec_mode %q (must be \"rpc\" or \"json\")", mode)
	}

	if _, err := exec.LookPath(cmd); err != nil {
		return nil, fmt.Errorf("pi: %q not found in PATH, install with: npm install -g @earendil-works/pi-coding-agent", cmd)
	}

	handshakeTimeout := parseDurationOption(opts, "handshake_timeout", 0)
	if handshakeTimeout < 0 {
		handshakeTimeout = 0
	}

	return &Agent{
		cmd:              cmd,
		workDir:          workDir,
		model:            model,
		mode:             mode,
		continueOnEmpty:  continueOnEmpty,
		handshakeTimeout: handshakeTimeout,
	}, nil
}

func (a *Agent) Name() string           { return "pi" }
func (a *Agent) CLIBinaryName() string  { return "pi" }
func (a *Agent) CLIDisplayName() string { return "Pi" }

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = model
	slog.Info("pi: model changed", "model", model)
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

func (a *Agent) AvailableModels(ctx context.Context) []core.ModelOption {
	// Pi has a first-class RPC command for listing configured models.
	// We spawn a short-lived `pi --mode rpc --no-session` process and call
	// get_available_models. This avoids interfering with the user's ongoing
	// interactive sessions.
	if ctx == nil {
		ctx = context.Background()
	}

	a.mu.Lock()
	cmdPath := a.cmd
	workDir := a.workDir
	extraEnv := append([]string{}, a.sessionEnv...)
	a.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	procCtx, procCancel := context.WithCancel(callCtx)
	defer procCancel()

	cmd := exec.CommandContext(procCtx, cmdPath, "--mode", "rpc", "--no-session")
	cmd.Dir = workDir
	cmd.Env = core.MergeEnv(os.Environ(), extraEnv)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.Warn("pi: AvailableModels stdin pipe", "error", err)
		return nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Warn("pi: AvailableModels stdout pipe", "error", err)
		return nil
	}
	// Avoid blocking on stderr; we don't need it for this one-shot call.
	cmd.Stderr = io.Discard

	tr := newRPCTransport(stdout, stdin, nil)
	if err := cmd.Start(); err != nil {
		slog.Warn("pi: AvailableModels start", "error", err)
		return nil
	}

	done := make(chan struct{})
	go func() {
		tr.readLoop(procCtx)
		close(done)
	}()

	resp, err := tr.call(callCtx, map[string]any{"type": "get_available_models"})
	// Stop the helper process no matter what.
	procCancel()
	_ = cmd.Wait()
	<-done

	if err != nil {
		slog.Warn("pi: get_available_models failed", "error", err)
		return nil
	}
	if err := resp.asError(); err != nil {
		slog.Warn("pi: get_available_models error", "error", err)
		return nil
	}

	var data struct {
		Models []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Provider string `json:"provider"`
		} `json:"models"`
	}
	if len(resp.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		slog.Warn("pi: parse get_available_models", "error", err)
		return nil
	}

	out := make([]core.ModelOption, 0, len(data.Models))
	for _, m := range data.Models {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		name := m.ID
		if strings.TrimSpace(m.Provider) != "" {
			name = m.Provider + "/" + m.ID
		}
		opt := core.ModelOption{Name: name, Desc: strings.TrimSpace(m.Name)}
		// Convenient alias: allow `/model switch <id>`.
		opt.Alias = m.ID
		out = append(out, opt)
	}
	return out
}

func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionEnv = env
}

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	workDir := a.workDir
	model := a.model
	thinking := a.thinking
	mode := a.mode
	extraEnv := append([]string{}, a.sessionEnv...)
	cmd := a.cmd
	continueOnEmpty := a.continueOnEmpty
	handshakeTimeout := a.handshakeTimeout
	a.mu.Unlock()
	if strings.TrimSpace(sessionID) == "" && continueOnEmpty {
		sessionID = core.ContinueSession
	}
	if mode == "json" {
		return newPiJSONSession(ctx, cmd, workDir, model, thinking, sessionID, extraEnv)
	}
	return newPiSession(ctx, cmd, workDir, model, thinking, sessionID, extraEnv, handshakeTimeout)
}

func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	a.mu.Lock()
	workDir := a.workDir
	a.mu.Unlock()

	sessDir := piSessionDir(workDir)
	if sessDir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(sessDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("pi: read session dir: %w", err)
	}

	var sessions []core.AgentSessionInfo
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		sessionID, summary, msgCount := scanPiSession(filepath.Join(sessDir, name))
		if sessionID == "" {
			continue
		}

		sessions = append(sessions, core.AgentSessionInfo{
			ID:           sessionID,
			Summary:      summary,
			MessageCount: msgCount,
			ModifiedAt:   info.ModTime(),
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})

	return sessions, nil
}

func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	a.mu.Lock()
	workDir := a.workDir
	a.mu.Unlock()

	sessDir := piSessionDir(workDir)
	if sessDir == "" {
		return fmt.Errorf("pi: cannot determine session directory")
	}

	path := findSessionFile(sessDir, sessionID)
	if path == "" {
		return fmt.Errorf("pi: session %q not found", sessionID)
	}
	return os.Remove(path)
}

func (a *Agent) Stop() error { return nil }

// ── MemoryFileProvider ───────────────────────────────────────

func (a *Agent) ProjectMemoryFile() string {
	a.mu.Lock()
	workDir := a.workDir
	a.mu.Unlock()

	absDir, err := filepath.Abs(workDir)
	if err != nil {
		absDir = a.workDir
	}
	return filepath.Join(absDir, "AGENTS.md")
}

func (a *Agent) GlobalMemoryFile() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".pi", "AGENTS.md")
}

// ── ReasoningEffortSwitcher ──────────────────────────────────

func (a *Agent) SetReasoningEffort(effort string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.thinking = effort
	slog.Info("pi: thinking level changed", "level", effort)
}

func (a *Agent) GetReasoningEffort() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.thinking
}

func (a *Agent) AvailableReasoningEfforts() []string {
	return []string{"off", "minimal", "low", "medium", "high", "xhigh"}
}

// ── GetWorkDir (for /status display) ─────────────────────────

func (a *Agent) SetWorkDir(dir string) {
	a.mu.Lock()
	a.workDir = dir
	a.mu.Unlock()
	slog.Info("pi: work dir changed", "work_dir", dir)
}

func (a *Agent) SetMode(mode string) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = "rpc"
	}
	a.mu.Lock()
	a.mode = m
	a.mu.Unlock()
}

// parseDurationOption extracts a duration from opts as either a Go duration
// string (e.g. "30s", "1m") or a number of seconds. Returns defaultValue when
// the key is absent/unparsable; parse failures are logged at debug level so
// misconfigurations (e.g. "30 seconds" with a space, which is NOT a valid
// Go duration string) don't fail silently.
func parseDurationOption(opts map[string]any, key string, defaultValue time.Duration) time.Duration {
	v, ok := opts[key]
	if !ok {
		return defaultValue
	}
	switch x := v.(type) {
	case string:
		if d, err := time.ParseDuration(strings.TrimSpace(x)); err == nil {
			return d
		} else {
			slog.Debug("pi: ignoring unparsable duration option", "key", key, "value", x, "default", defaultValue, "error", err)
		}
	case float64:
		return time.Duration(x) * time.Second
	case int:
		return time.Duration(x) * time.Second
	case int64:
		return time.Duration(x) * time.Second
	case time.Duration:
		return x
	default:
		slog.Debug("pi: ignoring duration option with unsupported type", "key", key, "type", fmt.Sprintf("%T", v), "default", defaultValue)
	}
	return defaultValue
}

func (a *Agent) GetWorkDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workDir
}

// ── HistoryProvider ──────────────────────────────────────────

func (a *Agent) GetSessionHistory(_ context.Context, sessionID string, limit int) ([]core.HistoryEntry, error) {
	a.mu.Lock()
	workDir := a.workDir
	a.mu.Unlock()

	sessDir := piSessionDir(workDir)
	if sessDir == "" {
		return nil, nil
	}

	sessFile := findSessionFile(sessDir, sessionID)
	if sessFile == "" {
		return nil, nil
	}

	return readPiHistory(sessFile, limit)
}

// ── SkillProvider ────────────────────────────────────────────

func (a *Agent) SkillDirs() []string {
	a.mu.Lock()
	workDir := a.workDir
	a.mu.Unlock()

	absDir, err := filepath.Abs(workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{filepath.Join(absDir, ".pi", "skills")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".pi", "skills"))
	}
	return dirs
}

// ── Session helpers ──────────────────────────────────────────

// findSessionFile locates the .jsonl file for a given session UUID in sessDir.
// Session files are named: <timestamp>_<uuid>.jsonl — this function extracts
// the UUID portion and matches exactly to avoid partial-match vulnerabilities.
func findSessionFile(sessDir, sessionID string) string {
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		// Extract UUID: strip .jsonl, then take everything after the last "_".
		base := strings.TrimSuffix(name, ".jsonl")
		if idx := strings.LastIndex(base, "_"); idx >= 0 {
			if base[idx+1:] == sessionID {
				return filepath.Join(sessDir, name)
			}
		}
	}
	return ""
}

// piSessionDir returns the pi session directory for the given workDir.
// Pi encodes the absolute path as: replace "/" with "-", wrap with "--".
// e.g. /home/user/project → --home-user-project--
func piSessionDir(workDir string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	absDir, err := filepath.Abs(workDir)
	if err != nil {
		return ""
	}
	encoded := "--" + strings.ReplaceAll(strings.TrimPrefix(absDir, "/"), "/", "-") + "--"
	return filepath.Join(homeDir, ".pi", "agent", "sessions", encoded)
}

// scanPiSession reads a pi session .jsonl file and extracts the session ID,
// a summary (first user message), and a message count.
func scanPiSession(path string) (sessionID, summary string, msgCount int) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		switch entry["type"] {
		case "session":
			if id, ok := entry["id"].(string); ok {
				sessionID = id
			}
		case "message":
			msg, _ := entry["message"].(map[string]any)
			if msg == nil {
				continue
			}
			role, _ := msg["role"].(string)
			if role == "user" || role == "assistant" {
				msgCount++
			}
			// Use first user message as summary.
			if role == "user" && summary == "" {
				content, _ := msg["content"].([]any)
				for _, c := range content {
					item, _ := c.(map[string]any)
					if item != nil {
						if text, ok := item["text"].(string); ok && text != "" {
							summary = text
							runes := []rune(summary)
							if len(runes) > 80 {
								summary = string(runes[:80]) + "..."
							}
							break
						}
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("pi: scan session error", "path", path, "error", err)
	}
	return
}

// readPiHistory reads user/assistant messages from a pi session file.
func readPiHistory(path string, limit int) ([]core.HistoryEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	var all []core.HistoryEntry
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry["type"] != "message" {
			continue
		}
		msg, _ := entry["message"].(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}

		var text string
		content, _ := msg["content"].([]any)
		for _, c := range content {
			item, _ := c.(map[string]any)
			if item != nil {
				if t, ok := item["text"].(string); ok && t != "" {
					text = t
					break
				}
			}
		}
		if text == "" {
			continue
		}
		all = append(all, core.HistoryEntry{Role: role, Content: text})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("pi: read history: %w", err)
	}

	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}
