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

// TestMattermostWebSocketSelfMessage tests if bot receives its own messages
func TestMattermostWebSocketSelfMessage(t *testing.T) {
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"
	channelID := "4kh7kgckdpyqfna34yrufzesha" // Off-Topic

	// Step 1: Connect WebSocket
	wsURL := fmt.Sprintf("ws://%s/api/v4/websocket", baseURL[7:])
	t.Logf("Connecting to WebSocket: %s", wsURL)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Step 2: Wait for hello and authenticate
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
	authMsg := map[string]any{
		"seq":    1,
		"action": "authentication_challenge",
		"data":   map[string]string{"token": token},
	}
	conn.WriteJSON(authMsg)

	// Wait for auth confirmation
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Read error: %v", err)
		}
		var resp struct {
			Event string `json:"event"`
			Seq   int64  `json:"seq"`
		}
		json.Unmarshal(msg, &resp)
		if resp.Event == "status_change" && resp.Seq == 1 {
			t.Log("Authenticated")
			break
		}
	}

	// Step 3: Start listening for posted events
	var receivedPosts []string
	var mu sync.Mutex
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-done:
				return
			default:
				conn.SetReadDeadline(time.Now().Add(1 * time.Second))
				_, msg, err := conn.ReadMessage()
				if err != nil {
					continue
				}
				var payload struct {
					Event string `json:"event"`
				}
				json.Unmarshal(msg, &payload)
				if payload.Event == "posted" {
					mu.Lock()
					receivedPosts = append(receivedPosts, string(msg))
					mu.Unlock()
					t.Logf("📨 Posted event received!")
				}
			}
		}
	}()

	// Give the listener time to start
	time.Sleep(500 * time.Millisecond)

	// Step 4: Send message via HTTP API
	testMessage := fmt.Sprintf("Bot self-test message - %s", time.Now().Format("15:04:05"))
	t.Logf("Sending message via HTTP API: %s", testMessage)

	payload := map[string]any{
		"channel_id": channelID,
		"message":    testMessage,
	}
	_, err = makeAPIRequest(baseURL, token, "POST", "/posts", payload)
	if err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}
	t.Log("Message sent via API")

	// Step 5: Wait for WebSocket to receive it
	t.Log("Waiting 5 seconds for WebSocket to receive the message...")
	time.Sleep(5 * time.Second)
	close(done)

	mu.Lock()
	count := len(receivedPosts)
	mu.Unlock()

	if count > 0 {
		t.Logf("✅ SUCCESS: Received %d posted events via WebSocket", count)
	} else {
		t.Log("❌ FAILED: No posted events received via WebSocket")
		t.Log("This means WebSocket is not receiving messages pushed by the server")
	}
}

// TestMattermostWebSocketOtherUserMessage tests if bot receives messages from other users
// Run this test, then send a message from another client
func TestMattermostWebSocketOtherUserMessage(t *testing.T) {
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"

	// Connect WebSocket
	wsURL := fmt.Sprintf("ws://%s/api/v4/websocket", baseURL[7:])
	t.Logf("Connecting to WebSocket: %s", wsURL)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Authenticate
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Read error: %v", err)
		}
		var event struct{ Event string }
		json.Unmarshal(msg, &event)
		if event.Event == "hello" {
			break
		}
	}
	conn.WriteJSON(map[string]any{
		"seq":    1,
		"action": "authentication_challenge",
		"data":   map[string]string{"token": token},
	})
	for {
		_, msg, _ := conn.ReadMessage()
		var resp struct {
			Event string `json:"event"`
			Seq   int64  `json:"seq"`
		}
		json.Unmarshal(msg, &resp)
		if resp.Event == "status_change" && resp.Seq == 1 {
			break
		}
	}

	t.Log("=====================================================")
	t.Log("WebSocket connected and authenticated!")
	t.Log("Please send a message from another client NOW!")
	t.Log("Monitoring for 30 seconds...")
	t.Log("=====================================================")

	// Listen for messages
	var receivedPosts []string
	var mu sync.Mutex
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-done:
				return
			default:
				conn.SetReadDeadline(time.Now().Add(1 * time.Second))
				_, msg, err := conn.ReadMessage()
				if err != nil {
					continue
				}
				var payload struct {
					Event string          `json:"event"`
					Data  json.RawMessage `json:"data"`
				}
				json.Unmarshal(msg, &payload)

				t.Logf("Received event: %s", payload.Event)

				if payload.Event == "posted" {
					mu.Lock()
					receivedPosts = append(receivedPosts, string(msg))
					mu.Unlock()

					// Extract message content
					var data struct {
						Post string `json:"post"`
					}
					json.Unmarshal(payload.Data, &data)
					var post struct {
						Message string `json:"message"`
						UserID  string `json:"user_id"`
					}
					json.Unmarshal([]byte(data.Post), &post)

					t.Log("=====================================================")
					t.Log("📨 NEW MESSAGE RECEIVED!")
					t.Logf("   From user: %s", post.UserID)
					t.Logf("   Message: %s", post.Message)
					t.Log("=====================================================")
				}
			}
		}
	}()

	// Wait 30 seconds
	time.Sleep(30 * time.Second)
	close(done)

	mu.Lock()
	count := len(receivedPosts)
	mu.Unlock()

	t.Log("=====================================================")
	t.Logf("Monitoring complete. Received %d posted events", count)
	if count == 0 {
		t.Log("❌ No messages received from other users")
		t.Log("This suggests WebSocket push for other users is not working")
	}
	t.Log("=====================================================")
}
