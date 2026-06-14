package mattermost_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestMattermostReceiveFromOtherUser tests if bot receives messages from other users
// Run this test and send a message from another client while it's running
func TestMattermostReceiveFromOtherUser(t *testing.T) {
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"

	// Connect WebSocket
	wsURL := fmt.Sprintf("ws://%s/api/v4/websocket", baseURL[7:])
	t.Logf("Connecting to: %s", wsURL)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Wait for hello
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Read error: %v", err)
		}
		var event struct{ Event string }
		json.Unmarshal(msg, &event)
		if event.Event == "hello" {
			t.Log("Hello received")
			break
		}
	}

	// Authenticate
	conn.WriteJSON(map[string]any{
		"seq":    1,
		"action": "authentication_challenge",
		"data":   map[string]string{"token": token},
	})

	// Wait for auth
	for {
		_, msg, _ := conn.ReadMessage()
		var resp struct {
			Event string `json:"event"`
			Seq   int64  `json:"seq"`
		}
		json.Unmarshal(msg, &resp)
		if resp.Event == "status_change" && resp.Seq == 1 {
			t.Log("Authenticated!")
			break
		}
	}

	t.Log("=====================================================")
	t.Log("🚀 WebSocket ready! Please send a message NOW!")
	t.Log("   Channel: Off-Topic")
	t.Log("   Monitoring for 30 seconds...")
	t.Log("=====================================================")

	// Track received messages
	var mu sync.Mutex
	postCount := 0
	typingCount := 0

	// Read messages with proper error handling
	// Don't use deadline - let it block indefinitely
	conn.SetReadDeadline(time.Time{}) // Disable deadline

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				t.Logf("WebSocket read error: %v", err)
				return
			}

			var payload struct {
				Event string          `json:"event"`
				Data  json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(msg, &payload); err != nil {
				continue
			}

			mu.Lock()
			switch payload.Event {
			case "posted":
				postCount++
				pc := postCount

				// Extract post data
				var data struct {
					Post string `json:"post"`
				}
				json.Unmarshal(payload.Data, &data)
				var post struct {
					Message   string `json:"message"`
					UserID    string `json:"user_id"`
					ChannelID string `json:"channel_id"`
				}
				json.Unmarshal([]byte(data.Post), &post)

				t.Log("\n=====================================================")
				t.Logf("📨 POST #%d RECEIVED!", pc)
				t.Logf("   User ID: %s", post.UserID)
				t.Logf("   Channel: %s", post.ChannelID)
				t.Logf("   Message: %s", post.Message)
				t.Log("=====================================================\n")

			case "typing":
				typingCount++
				var data struct {
					UserID string `json:"user_id"`
				}
				json.Unmarshal(payload.Data, &data)
				t.Logf("⌨️ Typing from user: %s", data.UserID)
			}
			mu.Unlock()
		}
	}()

	// Wait for 30 seconds then close
	time.Sleep(30 * time.Second)

	// Get final counts
	mu.Lock()
	pc := postCount
	tc := typingCount
	mu.Unlock()

	t.Log("\n=====================================================")
	t.Log("📊 MONITORING COMPLETE")
	t.Logf("   Posted events received: %d", pc)
	t.Logf("   Typing events received: %d", tc)
	if pc == 0 {
		t.Log("\n❌ No messages received from other users!")
		t.Log("   This indicates WebSocket push for other users may not be working.")
	} else {
		t.Log("\n✅ WebSocket push is working!")
	}
	t.Log("=====================================================")

	// Close connection
	conn.Close()
	<-done
}
