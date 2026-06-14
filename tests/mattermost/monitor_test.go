package mattermost_test

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestMattermostWebSocketMonitor continuously monitors WebSocket for messages
// Run this test, then have another user send a message to see if it's received in real-time
func TestMattermostWebSocketMonitor(t *testing.T) {
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"

	// Connect to WebSocket
	wsURL := fmt.Sprintf("ws://%s/api/v4/websocket", baseURL[7:])
	t.Logf("Connecting to: %s", wsURL)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("Failed to connect to WebSocket: %v", err)
	}
	defer conn.Close()

	t.Log("WebSocket connected!")

	// Wait for hello
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read: %v", err)
		}
		var event struct {
			Event string `json:"event"`
		}
		json.Unmarshal(message, &event)
		if event.Event == "hello" {
			t.Log("Hello received, authenticating...")
			break
		}
	}

	// Authenticate
	authMsg := map[string]any{
		"seq":    1,
		"action": "authentication_challenge",
		"data": map[string]string{
			"token": token,
		},
	}
	if err := conn.WriteJSON(authMsg); err != nil {
		t.Fatalf("Failed to authenticate: %v", err)
	}

	// Wait for auth confirmation
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read: %v", err)
		}
		var resp struct {
			Event    string `json:"event"`
			Seq      int64  `json:"seq"`
			SeqReply int64  `json:"seq_reply"`
			Status   string `json:"status"`
		}
		json.Unmarshal(message, &resp)
		t.Logf("Auth response: %s", string(message))
		if (resp.Event == "status_change" && resp.Seq == 1) || (resp.SeqReply == 1 && resp.Status == "OK") {
			t.Log("Authenticated! Now monitoring for messages...")
			break
		}
	}

	// Monitor for messages
	t.Log("=====================================================")
	t.Log("MONITORING STARTED - Waiting for messages...")
	t.Log("Please send a message from another client to test")
	t.Log("Monitoring for 60 seconds...")
	t.Log("=====================================================")

	var mu sync.Mutex
	messageCount := 0
	postCount := 0

	// Set up ping handler to keep connection alive
	conn.SetPingHandler(func(appData string) error {
		t.Log("Received ping from server")
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	// Start a goroutine to send periodic pings
	stopPing := make(chan struct{})
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, []byte("keepalive"), time.Now().Add(5*time.Second)); err != nil {
					log.Printf("Ping failed: %v", err)
					return
				}
				t.Log("Sent keepalive ping")
			}
		}
	}()

	// Read messages without timeout
	conn.SetReadDeadline(time.Time{}) // No timeout

	go func() {
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Read error: %v", err)
				return
			}

			mu.Lock()
			messageCount++
			mc := messageCount
			mu.Unlock()

			var payload struct {
				Event string          `json:"event"`
				Data  json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(message, &payload); err != nil {
				continue
			}

			timestamp := time.Now().Format("15:04:05.000")

			switch payload.Event {
			case "posted":
				mu.Lock()
				postCount++
				pc := postCount
				mu.Unlock()

				// Extract post info
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

				t.Logf("\n[%s] 📨 POST #%d received!", timestamp, pc)
				t.Logf("   Channel: %s", post.ChannelID)
				t.Logf("   User: %s", post.UserID)
				t.Logf("   Message: %s", post.Message)
				t.Log("-----------------------------------------------------")

			case "typing":
				var data struct {
					UserID    string `json:"user_id"`
					ChannelID string `json:"channel_id"`
				}
				json.Unmarshal(payload.Data, &data)
				t.Logf("[%s] ⌨️ Typing: user=%s channel=%s", timestamp, data.UserID, data.ChannelID)

			default:
				t.Logf("[%s] 📨 Event #%d: %s", timestamp, mc, payload.Event)
			}
		}
	}()

	// Wait for 60 seconds
	time.Sleep(60 * time.Second)
	close(stopPing)
	<-pingDone

	mu.Lock()
	pc := postCount
	mc := messageCount
	mu.Unlock()

	t.Log("=====================================================")
	t.Logf("MONITORING COMPLETE")
	t.Logf("Total messages received: %d", mc)
	t.Logf("Total posts received: %d", pc)
	t.Log("=====================================================")
}

// TestMattermostWebSocketKeepAlive tests WebSocket keepalive behavior
func TestMattermostWebSocketKeepAlive(t *testing.T) {
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"

	wsURL := fmt.Sprintf("ws://%s/api/v4/websocket", baseURL[7:])
	t.Logf("Connecting to: %s", wsURL)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	// Wait for hello and authenticate
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read: %v", err)
		}
		var event struct {
			Event string `json:"event"`
		}
		json.Unmarshal(message, &event)
		if event.Event == "hello" {
			break
		}
	}

	authMsg := map[string]any{
		"seq":    1,
		"action": "authentication_challenge",
		"data": map[string]string{
			"token": token,
		},
	}
	conn.WriteJSON(authMsg)

	// Wait for auth
	for {
		_, message, _ := conn.ReadMessage()
		var resp struct {
			Event string `json:"event"`
			Seq   int64  `json:"seq"`
		}
		json.Unmarshal(message, &resp)
		if resp.Event == "status_change" && resp.Seq == 1 {
			break
		}
	}

	t.Log("Connected and authenticated!")

	// Set up ping/pong handlers
	lastPong := time.Now()
	conn.SetPingHandler(func(appData string) error {
		t.Logf("Server ping received: %s", appData)
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})
	conn.SetPongHandler(func(appData string) error {
		lastPong = time.Now()
		t.Logf("Pong received: %s", appData)
		return nil
	})

	// Monitor for 30 seconds
	t.Log("Monitoring connection for 30 seconds...")
	conn.SetReadDeadline(time.Time{})

	go func() {
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				t.Logf("Read error: %v", err)
				return
			}
			var payload struct {
				Event string `json:"event"`
			}
			json.Unmarshal(message, &payload)
			if payload.Event != "" {
				t.Logf("Event: %s", payload.Event)
			}
		}
	}()

	// Send periodic pings
	for i := 0; i < 6; i++ {
		time.Sleep(5 * time.Second)
		if err := conn.WriteControl(websocket.PingMessage, []byte(fmt.Sprintf("ping-%d", i)), time.Now().Add(5*time.Second)); err != nil {
			t.Logf("Ping %d failed: %v", i, err)
		} else {
			t.Logf("Ping %d sent", i)
		}
	}

	t.Logf("Last pong: %v", lastPong)
	t.Log("Connection still alive!")
}
