package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestSummarizeToolArgs(t *testing.T) {
	if got := summarizeToolArgs(nil); got != "" {
		t.Fatalf("summarizeToolArgs(nil) = %q", got)
	}
	if got := summarizeToolArgs(map[string]any{"command": "ls -la"}); got != "ls -la" {
		t.Fatalf("summarizeToolArgs(command) = %q", got)
	}
	if got := summarizeToolArgs(map[string]any{"file_path": "/tmp/a.txt"}); got != "/tmp/a.txt" {
		t.Fatalf("summarizeToolArgs(file_path) = %q", got)
	}
}

func TestCleanAttachments(t *testing.T) {
	tmp := t.TempDir()
	attachDir := filepath.Join(tmp, ".cc-connect", "attachments")
	if err := os.MkdirAll(attachDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attachDir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanAttachments(tmp)
	entries, _ := os.ReadDir(attachDir)
	if len(entries) != 0 {
		t.Fatalf("expected attachments cleaned, got %d files", len(entries))
	}
}

func newTestAgent(t *testing.T, workDir string, extraEnv map[string]string) *Agent {
	// We cannot exec the test binary directly with custom flags like "--mode" because
	// the Go test runner will reject unknown flags before running TestHelperProcess.
	// Use a tiny shell wrapper that invokes the test binary with -test.run and passes
	// all pi args after "--".
	wrapper := filepath.Join(workDir, "pi-helper.sh")
	wrapperBody := "#!/bin/sh\n" +
		"exec \"" + os.Args[0] + "\" -test.run TestHelperProcess -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(wrapperBody), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	ag, err := New(map[string]any{
		"cmd":      wrapper,
		"work_dir": workDir,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	a := ag.(*Agent)
	var envPairs []string
	envPairs = append(envPairs, "GO_WANT_HELPER_PROCESS=1")
	for k, v := range extraEnv {
		envPairs = append(envPairs, k+"="+v)
	}
	a.SetSessionEnv(envPairs)
	return a
}

func drainFor(d time.Duration, ch <-chan core.Event) {
	deadline := time.After(d)
	for {
		select {
		case <-deadline:
			return
		case <-ch:
			// drain
		default:
			time.Sleep(1 * time.Millisecond)
		}
	}
}

func TestPiSession_JSONPromptFlow(t *testing.T) {
	workDir := t.TempDir()
	a := newTestAgent(t, workDir, map[string]string{
		"PI_HELPER_SESSION_ID": "sess-123",
	})

	sessAny, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession error = %v", err)
	}
	defer sessAny.Close()

	if err := sessAny.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send error = %v", err)
	}

	evCh := sessAny.Events()
	var gotText strings.Builder
	gotToolUse := false
	gotToolResult := false
	var gotResult *core.Event

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for gotResult == nil {
		select {
		case <-timer.C:
			t.Fatal("timeout waiting for EventResult")
		case ev := <-evCh:
			switch ev.Type {
			case core.EventText:
				gotText.WriteString(ev.Content)
			case core.EventToolUse:
				gotToolUse = true
			case core.EventToolResult:
				gotToolResult = true
			case core.EventResult:
				tmp := ev
				gotResult = &tmp
			}
		}
	}

	if gotText.String() != "Hello from helper" {
		t.Fatalf("text = %q, want %q", gotText.String(), "Hello from helper")
	}
	if !gotToolUse {
		t.Fatal("expected tool use event")
	}
	if !gotToolResult {
		t.Fatal("expected tool result event")
	}
	if gotResult.SessionID != "sess-123" {
		t.Fatalf("result SessionID = %q, want sess-123", gotResult.SessionID)
	}
	if !gotResult.Done {
		t.Fatal("expected result Done")
	}
	if gotResult.InputTokens != 123 || gotResult.OutputTokens != 45 {
		t.Fatalf("tokens = (%d,%d), want (123,45)", gotResult.InputTokens, gotResult.OutputTokens)
	}
}

func TestPiAgent_AvailableModels_RPC(t *testing.T) {
	workDir := t.TempDir()
	modelsJSON := `[
		{"id":"claude-sonnet-4-20250514","name":"Claude Sonnet 4","provider":"anthropic"},
		{"id":"gpt-4.1","name":"GPT-4.1","provider":"openai"}
	]`
	a := newTestAgent(t, workDir, map[string]string{
		"PI_HELPER_MODELS_JSON": modelsJSON,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := a.AvailableModels(ctx)
	want := []core.ModelOption{
		{Name: "anthropic/claude-sonnet-4-20250514", Desc: "Claude Sonnet 4", Alias: "claude-sonnet-4-20250514"},
		{Name: "openai/gpt-4.1", Desc: "GPT-4.1", Alias: "gpt-4.1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableModels() = %#v, want %#v", got, want)
	}
}

func TestPiAgent_SetModel_AppliesToNextSession(t *testing.T) {
	workDir := t.TempDir()
	a := newTestAgent(t, workDir, map[string]string{
		"PI_HELPER_EXPECT_MODEL": "openai/gpt-4.1",
		"PI_HELPER_SESSION_ID":   "sess-model",
	})
	a.SetModel("openai/gpt-4.1")

	sessAny, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession error = %v", err)
	}
	defer sessAny.Close()

	// Trigger a prompt so the helper validates --model.
	if err := sessAny.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send error = %v", err)
	}
}

func TestPiAgent_SetWorkDir_AppliesToNextSession(t *testing.T) {
	workDir1 := t.TempDir()
	workDir2 := t.TempDir()
	a := newTestAgent(t, workDir1, map[string]string{
		"PI_HELPER_EXPECT_CWD":     workDir2,
		"PI_HELPER_SESSION_ID":     "sess-wd",
		"PI_HELPER_NO_PROMPT_EXIT": "1",
	})

	a.SetWorkDir(workDir2)

	sessAny, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession error = %v", err)
	}
	defer sessAny.Close()

	// Trigger a prompt so the helper validates CWD.
	_ = sessAny.Send("hello", nil, nil)
}

// --- Helper process (fake `pi --mode rpc`) ---

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	mode := ""
	modelArg := ""
	sessionArg := ""
	hasContinue := false
	for i := 0; i < len(os.Args); i++ {
		if os.Args[i] == "--mode" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
		}
		if os.Args[i] == "--model" && i+1 < len(os.Args) {
			modelArg = os.Args[i+1]
		}
		if os.Args[i] == "--session" && i+1 < len(os.Args) {
			sessionArg = os.Args[i+1]
		}
		if os.Args[i] == "--continue" || os.Args[i] == "-c" {
			hasContinue = true
		}
	}

	if exp := os.Getenv("PI_HELPER_EXPECT_MODEL"); exp != "" {
		if modelArg != exp {
			fmt.Fprintf(os.Stderr, "helper: --model = %q, want %q\n", modelArg, exp)
			os.Exit(2)
		}
	}
	if exp := os.Getenv("PI_HELPER_EXPECT_CWD"); exp != "" {
		cwd, _ := os.Getwd()
		if cwd != exp {
			fmt.Fprintf(os.Stderr, "helper: cwd = %q, want %q\n", cwd, exp)
			os.Exit(2)
		}
	}

	sessionID := strings.TrimSpace(sessionArg)
	if sessionID == "" {
		sessionID = strings.TrimSpace(os.Getenv("PI_HELPER_SESSION_ID"))
	}
	if sessionID == "" {
		sessionID = "sess-default"
	}

	// --- JSON mode: single-shot print-mode output ---
	if mode == "json" {
		enc := json.NewEncoder(os.Stdout)
		cwd, _ := os.Getwd()
		_ = enc.Encode(map[string]any{"type": "session", "id": sessionID, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "cwd": cwd})
		// Simulate a normal turn.
		_ = enc.Encode(map[string]any{"type": "agent_start"})
		_ = enc.Encode(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "Hello from helper"}})
		_ = enc.Encode(map[string]any{"type": "tool_execution_start", "toolName": "bash", "args": map[string]any{"command": "echo hi"}})
		_ = enc.Encode(map[string]any{"type": "tool_execution_end", "toolName": "bash", "isError": false, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}})
		_ = enc.Encode(map[string]any{"type": "agent_end", "messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Hello from helper"}}, "usage": map[string]any{"input": 123, "output": 45}}}})
		_ = hasContinue // silence unused; only here to prove parsing works.
		os.Exit(0)
	}

	// --- RPC mode: used by AvailableModels() helper ---
	if mode != "rpc" {
		fmt.Fprintf(os.Stderr, "helper: expected --mode rpc or --mode json, got %q\n", mode)
		os.Exit(2)
	}

	r := bufio.NewReader(os.Stdin)
	enc := json.NewEncoder(os.Stdout)

	for {
		lineBytes, err := r.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		line := strings.TrimSuffix(string(lineBytes), "\n")
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var cmd map[string]any
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			_ = enc.Encode(map[string]any{"type": "response", "command": "parse", "success": false, "error": err.Error()})
			continue
		}
		typ, _ := cmd["type"].(string)
		id := cmd["id"]

		sendResp := func(command string, success bool, data any, errMsg string) {
			resp := map[string]any{"type": "response", "command": command, "success": success}
			if id != nil {
				resp["id"] = id
			}
			if data != nil {
				resp["data"] = data
			}
			if errMsg != "" {
				resp["error"] = errMsg
			}
			_ = enc.Encode(resp)
		}

		switch typ {
		case "get_state":
			sendResp("get_state", true, map[string]any{"sessionId": sessionID, "isStreaming": false}, "")
		case "get_available_models":
			raw := os.Getenv("PI_HELPER_MODELS_JSON")
			var models any
			if raw != "" {
				_ = json.Unmarshal([]byte(raw), &models)
			}
			if models == nil {
				models = []any{}
			}
			sendResp("get_available_models", true, map[string]any{"models": models}, "")
		default:
			sendResp(typ, false, nil, "unknown command")
		}
	}
}
