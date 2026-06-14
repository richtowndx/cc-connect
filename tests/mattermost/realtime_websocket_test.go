package mattermost_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestMattermostWebSocketRealtimeMessage tests real-time message delivery via WebSocket
func TestMattermostWebSocketRealtimeMessage(t *testing.T) {
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"
	testMessage := fmt.Sprintf("机器人测试消息，测试消息实时性 - %s", time.Now().Format("15:04:05"))

	// Step 1: Get bot user info
	t.Log("Step 1: Getting bot user info...")
	meResp, err := makeAPIRequest(baseURL, token, "GET", "/users/me", nil)
	if err != nil {
		t.Fatalf("Failed to get bot user: %v", err)
	}
	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(meResp, &me); err != nil {
		t.Fatalf("Failed to parse user response: %v", err)
	}
	t.Logf("Bot user: %s (ID: %s)", me.Username, me.ID)

	// Step 2: Get channels
	t.Log("Step 2: Getting available channels...")
	channelsResp, err := makeAPIRequest(baseURL, token, "GET", "/users/me/channels", nil)
	if err != nil {
		t.Fatalf("Failed to get channels: %v", err)
	}

	var channels []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
	}
	if err := json.Unmarshal(channelsResp, &channels); err != nil {
		t.Fatalf("Failed to parse channels response: %v", err)
	}

	if len(channels) == 0 {
		t.Fatal("No channels available")
	}

	var targetChannel *struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
	}

	for i := range channels {
		if channels[i].Name == "town-square" || channels[i].Name == "off-topic" {
			targetChannel = &channels[i]
			break
		}
	}

	if targetChannel == nil {
		targetChannel = &channels[0]
	}

	t.Logf("Target channel: %s (ID: %s)", targetChannel.DisplayName, targetChannel.ID)

	// Step 3: Connect to WebSocket
	t.Log("Step 3: Connecting to WebSocket...")
	wsURL := fmt.Sprintf("ws://%s/api/v4/websocket", baseURL[7:])

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

	t.Logf("WebSocket connected to: %s", wsURL)

	// Step 4: Wait for hello event first
	t.Log("Step 4: Waiting for hello event...")
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read message: %v", err)
		}

		var event struct {
			Event string `json:"event"`
		}
		json.Unmarshal(message, &event)
		t.Logf("Received: %s", string(message))

		if event.Event == "hello" {
			t.Log("Hello event received, sending authentication...")
			break
		}
	}

	// Step 5: Send authentication challenge
	authMsg := map[string]any{
		"seq":    1,
		"action": "authentication_challenge",
		"data": map[string]string{
			"token": token,
		},
	}
	if err := conn.WriteJSON(authMsg); err != nil {
		t.Fatalf("Failed to send auth: %v", err)
	}
	t.Log("Authentication challenge sent")

	// Step 6: Wait for authentication confirmation
	// Mattermost sends status_change with seq=1 after successful auth
	t.Log("Step 6: Waiting for authentication confirmation...")
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	authConfirmed := false
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read auth response: %v", err)
		}

		var resp struct {
			Event     string `json:"event"`
			Status    string `json:"status"`
			Seq       int64  `json:"seq"`
			SeqReply  int64  `json:"seq_reply"`
		}
		json.Unmarshal(message, &resp)
		t.Logf("Received: %s", string(message))

		// Check for authentication success indicators
		// Mattermost may send seq_reply=1 with status=OK, or status_change with seq=1
		if resp.SeqReply == 1 && resp.Status == "OK" {
			t.Log("Authentication confirmed via seq_reply!")
			authConfirmed = true
			break
		}
		if resp.Event == "status_change" && resp.Seq == 1 {
			t.Log("Authentication confirmed via status_change!")
			authConfirmed = true
			break
		}
		// Timeout after a few messages
		if resp.Seq > 1 || resp.Event == "" {
			authConfirmed = true
			break
		}
	}

	if !authConfirmed {
		t.Log("Warning: Authentication status unclear, proceeding anyway...")
	}

	// Step 7: Set up message listener
	t.Log("Step 7: Setting up message listener...")

	var receivedMsg map[string]any
	var msgMu sync.Mutex
	msgReceived := make(chan struct{}, 1)

	go func() {
		for {
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("WebSocket read error: %v", err)
				return
			}

			var payload map[string]any
			if err := json.Unmarshal(message, &payload); err != nil {
				continue
			}

			event, _ := payload["event"].(string)
			t.Logf("Received WebSocket event: %s", event)

			if event == "posted" {
				msgMu.Lock()
				receivedMsg = payload
				msgMu.Unlock()
				select {
				case msgReceived <- struct{}{}:
				default:
				}
				return
			}
		}
	}()

	// Step 8: Send test message via HTTP API
	t.Log("Step 8: Sending test message via HTTP API...")
	time.Sleep(500 * time.Millisecond) // Give WebSocket time to be ready

	sendTime := time.Now()
	payload := map[string]any{
		"channel_id": targetChannel.ID,
		"message":    testMessage,
	}

	postResp, err := makeAPIRequest(baseURL, token, "POST", "/posts", payload)
	if err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	var post struct {
		ID        string `json:"id"`
		CreateAt  int64  `json:"create_at"`
		Message   string `json:"message"`
		ChannelID string `json:"channel_id"`
	}
	if err := json.Unmarshal(postResp, &post); err != nil {
		t.Fatalf("Failed to parse post response: %v", err)
	}

	apiLatency := time.Since(sendTime)
	t.Logf("Message sent via API! Post ID: %s, Latency: %v", post.ID, apiLatency)

	// Step 9: Wait for WebSocket to receive the message
	t.Log("Step 9: Waiting for WebSocket to receive the message...")

	select {
	case <-msgReceived:
		totalLatency := time.Since(sendTime)
		msgMu.Lock()
		msg := receivedMsg
		msgMu.Unlock()

		t.Log("=====================================================")
		t.Log("SUCCESS! Message received via WebSocket in real-time!")
		t.Logf("Total latency: %v", totalLatency)

		if data, ok := msg["data"].(map[string]any); ok {
			if postData, ok := data["post"]; ok {
				var p struct {
					Message string `json:"message"`
				}
				switch v := postData.(type) {
				case string:
					json.Unmarshal([]byte(v), &p)
				case map[string]any:
					data, _ := json.Marshal(v)
					json.Unmarshal(data, &p)
				}
				t.Logf("Message content: %s", p.Message)
			}
		}
		t.Log("=====================================================")

	case <-time.After(10 * time.Second):
		t.Log("=====================================================")
		t.Log("FAILED! Message NOT received via WebSocket within 10 seconds!")
		t.Log("This indicates WebSocket real-time push is NOT working.")
		t.Log("=====================================================")
		t.Fatal("WebSocket did not receive the posted message in real-time")
	}
}

// TestMattermostWebSocketConnection tests basic WebSocket connectivity
func TestMattermostWebSocketConnection(t *testing.T) {
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"

	wsURL := fmt.Sprintf("ws://%s/api/v4/websocket", baseURL[7:])
	t.Logf("Connecting to: %s", wsURL)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, resp, err := dialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			t.Fatalf("WebSocket dial failed: %v (status %d)", err, resp.StatusCode)
		}
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer conn.Close()

	t.Log("WebSocket connection established!")

	// Read messages for 5 seconds
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	messageCount := 0
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			break
		}
		messageCount++
		t.Logf("Message %d: %s", messageCount, string(message))
	}

	t.Logf("Received %d messages", messageCount)
}

// makeAPIRequest makes an authenticated API request
func makeAPIRequest(baseURL, token, method, path string, body any) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	fullURL := baseURL + "/api/v4" + path

	var reqBody *bytes.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(jsonData)
	} else {
		reqBody = bytes.NewReader([]byte{})
	}

	req, err := http.NewRequest(method, fullURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}
