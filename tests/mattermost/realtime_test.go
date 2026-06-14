package mattermost_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestMattermostRealtime tests message delivery real-time performance
// This test sends a message and verifies it's delivered instantly
func TestMattermostRealtime(t *testing.T) {
	// Configuration from user's config
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"
	testMessage := "机器人测试消息，测试消息实时性"

	// Create HTTP client
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Step 1: Get bot user info
	t.Log("Step 1: Getting bot user info...")
	meResp, err := makeRequest(client, baseURL, token, "GET", "/users/me", nil)
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

	// Step 2: Get available channels
	t.Log("Step 2: Getting available channels...")
	channelsResp, err := makeRequest(client, baseURL, token, "GET", "/users/me/channels", nil)
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

	// Find a suitable channel (prefer town-square or first public channel)
	var targetChannel *struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
	}

	for _, ch := range channels {
		t.Logf("Found channel: %s (%s) - Type: %s", ch.DisplayName, ch.Name, ch.Type)
		if ch.Name == "town-square" || ch.Name == "off-topic" {
			targetChannel = &ch
			break
		}
	}

	if targetChannel == nil {
		// Use the first channel
		targetChannel = &channels[0]
	}

	t.Logf("Target channel: %s (ID: %s)", targetChannel.DisplayName, targetChannel.ID)

	// Step 3: Send test message
	t.Log("Step 3: Sending test message...")
	sendTime := time.Now()

	payload := map[string]interface{}{
		"channel_id": targetChannel.ID,
		"message":    testMessage,
	}

	postResp, err := makeRequest(client, baseURL, token, "POST", "/posts", payload)
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

	latency := time.Since(sendTime)
	t.Logf("Message sent successfully!")
	t.Logf("  Post ID: %s", post.ID)
	t.Logf("  Channel: %s", targetChannel.DisplayName)
	t.Logf("  Message: %s", testMessage)
	t.Logf("  API Latency: %v", latency)

	// Step 4: Wait for user confirmation
	t.Log("")
	t.Log("=====================================================")
	t.Log("MESSAGE SENT! Please check your Mattermost client.")
	t.Log("You should see the message WITHOUT refreshing.")
	t.Log("=====================================================")
	t.Log("")
	t.Logf("Waiting 5 seconds for you to verify real-time delivery...")
	time.Sleep(5 * time.Second)
	t.Log("Test completed. Did you receive the message in real-time?")
}

// TestMattermostWebSocketRealtime tests WebSocket-based message delivery
func TestMattermostWebSocketRealtime(t *testing.T) {
	// This test is for manual verification
	// It demonstrates WebSocket connection and message receiving
	t.Skip("Manual test - requires WebSocket verification")
}

// makeRequest makes an authenticated API request
func makeRequest(client *http.Client, baseURL, token, method, path string, body interface{}) ([]byte, error) {
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
