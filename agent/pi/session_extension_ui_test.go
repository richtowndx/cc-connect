package pi

import (
	"context"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestPiSession_ExtensionUIRequest_FireAndForgetIgnored(t *testing.T) {
	s := &piSession{
		events:    make(chan core.Event, 10),
		ctx:       context.Background(),
		extUIReqs: make(map[string]extUIRequest),
	}

	s.handleExtensionUIRequest(map[string]any{
		"type":   "extension_ui_request",
		"id":     "uuid-7",
		"method": "setWidget",
	})

	select {
	case ev := <-s.events:
		t.Fatalf("unexpected event: %+v", ev)
	default:
	}

	s.extMu.Lock()
	_, ok := s.extUIReqs["uuid-7"]
	s.extMu.Unlock()
	if ok {
		t.Fatalf("fire-and-forget request should not be stored")
	}
}

func TestPiSession_ExtensionUIRequest_DialogEmitted(t *testing.T) {
	s := &piSession{
		events:    make(chan core.Event, 10),
		ctx:       context.Background(),
		extUIReqs: make(map[string]extUIRequest),
	}

	s.handleExtensionUIRequest(map[string]any{
		"type":    "extension_ui_request",
		"id":      "uuid-2",
		"method":  "confirm",
		"title":   "Clear session?",
		"message": "All messages will be lost.",
	})

	ev := <-s.events
	if ev.Type != core.EventPermissionRequest || ev.ToolName != "AskUserQuestion" || ev.RequestID != "uuid-2" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if len(ev.Questions) != 1 {
		t.Fatalf("Questions len = %d, want 1", len(ev.Questions))
	}

	s.extMu.Lock()
	_, ok := s.extUIReqs["uuid-2"]
	s.extMu.Unlock()
	if !ok {
		t.Fatalf("dialog request should be stored")
	}
}
