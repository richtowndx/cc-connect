package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestNormalizeMode(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "default"},
		{"default", "default"},
		{"yolo", "yolo"},
		{"YOLO", "yolo"},
		{"bypass", "yolo"},
		{"auto-approve", "yolo"},
		{"unknown", "default"},
	}
	for _, tt := range tests {
		if got := normalizeMode(tt.in); got != tt.want {
			t.Errorf("normalizeMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

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
		"exec \"" + os.Args[0] + "\" -test.run TestHelperProcess -test.v -- \"$@\"\n"
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

func TestPiSession_RPCPromptFlow(t *testing.T) {
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

func TestPiSession_AutoRestartAfterExit(t *testing.T) {
	workDir := t.TempDir()
	counterFile := filepath.Join(workDir, "counter.txt")
	a := newTestAgent(t, workDir, map[string]string{
		"PI_HELPER_SESSION_ID":              "sess-r",
		"PI_HELPER_COUNTER_FILE":            counterFile,
		"PI_HELPER_EXIT_AFTER_STATE_ONCE":   "1",
		"PI_HELPER_EXIT_AFTER_STATE_SIGNAL": "SIGTERM",
	})

	sessAny, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession error = %v", err)
	}
	defer sessAny.Close()

	// First helper run exits after get_state; it may have emitted exit error/result.
	drainFor(50*time.Millisecond, sessAny.Events())

	if err := sessAny.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send error = %v", err)
	}

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			t.Fatal("timeout waiting for EventResult")
		case ev := <-sessAny.Events():
			if ev.Type == core.EventResult {
				return
			}
		}
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
	})
	a.SetModel("openai/gpt-4.1")

	sessAny, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession error = %v", err)
	}
	_ = sessAny.Close()
}

func TestPiSession_ExtensionUIConfirm_RoundTrip(t *testing.T) {
	workDir := t.TempDir()
	a := newTestAgent(t, workDir, map[string]string{
		"PI_HELPER_SESSION_ID":      "sess-ui",
		"PI_HELPER_EXTENSION_UI":    "confirm",
		"PI_HELPER_EXTENSION_UI_ID": "ui-1",
	})

	sessAny, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession error = %v", err)
	}
	defer sessAny.Close()

	if err := sessAny.Send("trigger ui", nil, nil); err != nil {
		t.Fatalf("Send error = %v", err)
	}

	var req *core.Event
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for req == nil {
		select {
		case <-timer.C:
			t.Fatal("timeout waiting for permission request")
		case ev := <-sessAny.Events():
			if ev.Type == core.EventPermissionRequest {
				tmp := ev
				req = &tmp
			}
		}
	}

	if req.RequestID != "ui-1" {
		t.Fatalf("RequestID = %q, want ui-1", req.RequestID)
	}
	if req.ToolName != "AskUserQuestion" {
		t.Fatalf("ToolName = %q, want AskUserQuestion", req.ToolName)
	}

	// Simulate engine AskUserQuestion response: answers["0"] contains the selection.
	_ = sessAny.RespondPermission(req.RequestID, core.PermissionResult{
		Behavior: "allow",
		UpdatedInput: map[string]any{
			"answers": map[string]any{"0": "Yes"},
		},
	})

	// Now expect EventResult.
	for {
		select {
		case <-timer.C:
			t.Fatal("timeout waiting for EventResult")
		case ev := <-sessAny.Events():
			if ev.Type == core.EventResult {
				return
			}
		}
	}
}

// --- Helper process (fake `pi --mode rpc`) ---

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	// Minimal arg check.
	hasRPC := false
	modelArg := ""
	for i := 0; i < len(os.Args); i++ {
		if os.Args[i] == "--mode" && i+1 < len(os.Args) && os.Args[i+1] == "rpc" {
			hasRPC = true
		}
		if os.Args[i] == "--model" && i+1 < len(os.Args) {
			modelArg = os.Args[i+1]
		}
	}
	if !hasRPC {
		fmt.Fprintln(os.Stderr, "helper: missing --mode rpc")
		os.Exit(2)
	}
	if exp := os.Getenv("PI_HELPER_EXPECT_MODEL"); exp != "" {
		if modelArg != exp {
			fmt.Fprintf(os.Stderr, "helper: --model = %q, want %q\n", modelArg, exp)
			os.Exit(2)
		}
	}

	sessionID := os.Getenv("PI_HELPER_SESSION_ID")
	if sessionID == "" {
		sessionID = "sess-default"
	}

	counterFile := os.Getenv("PI_HELPER_COUNTER_FILE")
	exitAfterStateOnce := os.Getenv("PI_HELPER_EXIT_AFTER_STATE_ONCE") == "1"
	extUI := os.Getenv("PI_HELPER_EXTENSION_UI")
	extUIID := os.Getenv("PI_HELPER_EXTENSION_UI_ID")
	if extUIID == "" {
		extUIID = "ui-1"
	}

	incCounter := func() int {
		if counterFile == "" {
			return 1
		}
		b, _ := os.ReadFile(counterFile)
		n := 0
		if len(b) > 0 {
			if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				n = v
			}
		}
		n++
		_ = os.WriteFile(counterFile, []byte(strconv.Itoa(n)), 0o644)
		return n
	}

	r := bufio.NewReader(os.Stdin)
	enc := json.NewEncoder(os.Stdout)

	runNum := incCounter()

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
			if exitAfterStateOnce && runNum == 1 {
				os.Exit(0)
			}

		case "get_available_models":
			// Used by Agent.AvailableModels().
			raw := os.Getenv("PI_HELPER_MODELS_JSON")
			var models any
			if raw != "" {
				_ = json.Unmarshal([]byte(raw), &models)
			}
			if models == nil {
				models = []any{}
			}
			sendResp("get_available_models", true, map[string]any{"models": models}, "")

		case "prompt":
			sendResp("prompt", true, nil, "")
			if os.Getenv("PI_HELPER_NO_PROMPT_EXIT") == "1" {
				os.Exit(0)
			}

			// Optional extension UI dialog
			if extUI == "confirm" {
				_ = enc.Encode(map[string]any{"type": "extension_ui_request", "id": extUIID, "method": "confirm", "title": "Confirm?", "message": "Proceed?"})
				// Wait for extension_ui_response
				for {
					l2, err := r.ReadBytes('\n')
					if err != nil {
						os.Exit(0)
					}
					l2s := strings.TrimSuffix(string(l2), "\n")
					l2s = strings.TrimSuffix(l2s, "\r")
					var resp map[string]any
					if json.Unmarshal([]byte(l2s), &resp) != nil {
						continue
					}
					if resp["type"] == "extension_ui_response" && resp["id"] == extUIID {
						break
					}
				}
			}

			// Stream events
			_ = enc.Encode(map[string]any{"type": "agent_start"})
			_ = enc.Encode(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "Hello from helper"}})
			_ = enc.Encode(map[string]any{"type": "tool_execution_start", "toolName": "bash", "args": map[string]any{"command": "echo hi"}})
			_ = enc.Encode(map[string]any{"type": "tool_execution_end", "toolName": "bash", "isError": false, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}})
			_ = enc.Encode(map[string]any{"type": "agent_end", "messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Hello from helper"}}, "usage": map[string]any{"input": 123, "output": 45}}}})

		default:
			sendResp(typ, false, nil, "unknown command")
		}
	}
}
