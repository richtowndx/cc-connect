package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Test server configuration - can be overridden via environment variables
var (
	testBaseURL = getEnvOrDefault("MATTERMOST_TEST_URL", "http://10.8.5.5:8065")
	testToken   = getEnvOrDefault("MATTERMOST_TEST_TOKEN", "61sjo4awxf8rjbefrxm17xenfa")
)

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// skipIfNoConnection skips the test if server is not reachable
func skipIfNoConnection(t *testing.T) bool {
	resp, err := http.Get(testBaseURL + "/api/v4/system/ping")
	if err != nil {
		t.Skipf("Server not reachable: %v", err)
		return true
	}
	resp.Body.Close()
	return false
}

// skipIfNoAuth skips the test if authentication fails
func skipIfNoAuth(t *testing.T) *Client {
	client := NewClient(testBaseURL, testToken)
	user, err := client.GetMe()
	if err != nil {
		t.Skipf("Authentication failed (set MATTERMOST_TEST_TOKEN env for valid token): %v", err)
		return nil
	}
	t.Logf("Authenticated as: %s (ID: %s)", user.Username, user.ID)
	return client
}

// TestClientConnectivity tests basic API connectivity
func TestClientConnectivity(t *testing.T) {
	if skipIfNoConnection(t) {
		return
	}
	skipIfNoAuth(t)
}

// TestNormalizeBaseURL tests URL normalization
func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"http://example.com", "http://example.com"},
		{"http://example.com/", "http://example.com"},
		{"http://example.com/api/v4", "http://example.com"},
		{"http://example.com/api/v4/", "http://example.com"},
		{"https://mattermost.example.com", "https://mattermost.example.com"},
		{"", ""},
	}

	for _, tt := range tests {
		result := normalizeBaseURL(tt.input)
		if result != tt.expected {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// TestNewClient tests client creation
func TestNewClient(t *testing.T) {
	client := NewClient("http://example.com/api/v4/", "test-token")
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	if client.baseURL != "http://example.com" {
		t.Errorf("baseURL = %q, want http://example.com", client.baseURL)
	}
	if client.apiBaseURL != "http://example.com/api/v4" {
		t.Errorf("apiBaseURL = %q, want http://example.com/api/v4", client.apiBaseURL)
	}
	if client.token != "test-token" {
		t.Errorf("token = %q, want test-token", client.token)
	}
}

// TestBuildWebSocketURL tests WebSocket URL construction
func TestBuildWebSocketURL(t *testing.T) {
	tests := []struct {
		baseURL  string
		expected string
	}{
		{"http://example.com", "ws://example.com/api/v4/websocket"},
		{"https://example.com", "wss://example.com/api/v4/websocket"},
	}

	for _, tt := range tests {
		client := NewClient(tt.baseURL, "token")
		result := client.buildWebSocketURL()
		if result != tt.expected {
			t.Errorf("buildWebSocketURL() = %q, want %q", result, tt.expected)
		}
	}
}

// TestNewPlatform tests platform creation
func TestNewPlatform(t *testing.T) {
	tests := []struct {
		name      string
		opts      map[string]any
		wantErr   bool
		errSubstr string
	}{
		{
			name: "valid options",
			opts: map[string]any{
				"base_url": "http://example.com",
				"token":    "test-token",
			},
			wantErr: false,
		},
		{
			name: "missing base_url",
			opts: map[string]any{
				"token": "test-token",
			},
			wantErr:   true,
			errSubstr: "base_url is required",
		},
		{
			name: "missing token",
			opts: map[string]any{
				"base_url": "http://example.com",
			},
			wantErr:   true,
			errSubstr: "token is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform, err := New(tt.opts)
			if tt.wantErr {
				if err == nil {
					t.Errorf("New() expected error, got nil")
				} else if tt.errSubstr != "" && !contains(err.Error(), tt.errSubstr) {
					t.Errorf("New() error = %v, want containing %q", err, tt.errSubstr)
				}
			} else {
				if err != nil {
					t.Errorf("New() unexpected error: %v", err)
				}
				if platform == nil {
					t.Error("New() returned nil platform")
				}
				if platform != nil && platform.Name() != "mattermost" {
					t.Errorf("Name() = %q, want mattermost", platform.Name())
				}
			}
		})
	}
}

// TestReconstructReplyCtx tests session key reconstruction
func TestReconstructReplyCtx(t *testing.T) {
	platform := &Platform{}

	tests := []struct {
		sessionKey string
		wantErr    bool
		channelID  string
	}{
		{"mattermost:abc123", false, "abc123"},
		{"mattermost:abc123:user456", false, "abc123"},
		{"invalid:key", true, ""},
		{"telegram:abc123", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.sessionKey, func(t *testing.T) {
			rctx, err := platform.ReconstructReplyCtx(tt.sessionKey)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ReconstructReplyCtx() expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("ReconstructReplyCtx() unexpected error: %v", err)
				}
				rc, ok := rctx.(replyContext)
				if !ok {
					t.Errorf("ReconstructReplyCtx() returned wrong type: %T", rctx)
				} else if rc.channelID != tt.channelID {
					t.Errorf("channelID = %q, want %q", rc.channelID, tt.channelID)
				}
			}
		})
	}
}

// TestGetMe tests user retrieval (requires valid auth)
func TestGetMe(t *testing.T) {
	if skipIfNoConnection(t) {
		return
	}
	client := skipIfNoAuth(t)
	if client == nil {
		return
	}
	// Already authenticated in skipIfNoAuth
}

// TestWebSocketConnect tests WebSocket connection (requires valid auth)
func TestWebSocketConnect(t *testing.T) {
	if skipIfNoConnection(t) {
		return
	}
	client := skipIfNoAuth(t)
	if client == nil {
		return
	}

	ws := NewWSConnection(client, testToken)
	if ws == nil {
		t.Fatal("Failed to create WebSocket connection")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := ws.Connect(ctx)
	if err != nil {
		t.Fatalf("WebSocket connect failed: %v", err)
	}
	defer ws.Close()

	t.Log("WebSocket connected successfully")
	// Test passes - we successfully connected and authenticated
}

// TestSendMessageIntegration tests sending a real message to Mattermost
func TestSendMessageIntegration(t *testing.T) {
	if skipIfNoConnection(t) {
		return
	}
	client := skipIfNoAuth(t)
	if client == nil {
		return
	}

	// Get user info
	user, err := client.GetMe()
	if err != nil {
		t.Fatalf("GetMe failed: %v", err)
	}
	t.Logf("Bot user: %s (ID: %s)", user.Username, user.ID)

	// Get teams
	teamsBody, err := client.request("GET", fmt.Sprintf("/users/%s/teams", user.ID), nil)
	if err != nil {
		t.Fatalf("GetTeams failed: %v", err)
	}

	var teams []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(teamsBody, &teams); err != nil {
		t.Fatalf("Parse teams failed: %v", err)
	}

	if len(teams) == 0 {
		t.Fatal("No teams found for bot user")
	}

	teamID := teams[0].ID
	t.Logf("Using team: %s (ID: %s)", teams[0].DisplayName, teamID)

	// Get channels
	channelsBody, err := client.request("GET", fmt.Sprintf("/users/me/teams/%s/channels", teamID), nil)
	if err != nil {
		t.Fatalf("GetChannels failed: %v", err)
	}

	var channels []Channel
	if err := json.Unmarshal(channelsBody, &channels); err != nil {
		t.Fatalf("Parse channels failed: %v", err)
	}

	// Find a public channel (type "O")
	var targetChannel *Channel
	for _, ch := range channels {
		if ch.Type == "O" {
			targetChannel = &ch
			break
		}
	}

	if targetChannel == nil {
		// Fallback to DM channel
		dmChannel, err := client.CreateDirectChannel([]string{user.ID, user.ID})
		if err != nil {
			t.Fatalf("No public channel and DM creation failed: %v", err)
		}
		targetChannel = dmChannel
	}

	t.Logf("Using channel: %s (ID: %s, Type: %s)", targetChannel.DisplayName, targetChannel.ID, targetChannel.Type)

	// Send a test message with markdown
	testMessage := fmt.Sprintf(`**cc-connect Integration Test** 🚀

This is an automated test message from cc-connect Mattermost platform.

- ✅ API Connection
- ✅ Authentication
- ✅ Message Sending
- ✅ Markdown Support

Test completed at: %s`, time.Now().Format(time.RFC3339))

	post, err := client.CreatePost(targetChannel.ID, testMessage, "", nil)
	if err != nil {
		t.Fatalf("CreatePost failed: %v", err)
	}

	t.Logf("✅ Message sent successfully! Post ID: %s", post.ID)

	// Verify the post
	if post.ID == "" {
		t.Fatal("Post ID should not be empty")
	}
	if post.ChannelID != targetChannel.ID {
		t.Fatalf("Post channel ID mismatch: got %s, want %s", post.ChannelID, targetChannel.ID)
	}

	// Clean up - delete the test post
	_, err = client.request("DELETE", fmt.Sprintf("/posts/%s", post.ID), nil)
	if err != nil {
		t.Logf("Warning: failed to delete test post: %v", err)
	} else {
		t.Log("Test post deleted")
	}
}

// TestSendFileIntegration tests file upload to Mattermost
func TestSendFileIntegration(t *testing.T) {
	if skipIfNoConnection(t) {
		return
	}
	client := skipIfNoAuth(t)
	if client == nil {
		return
	}

	// Create a DM channel
	user, err := client.GetMe()
	if err != nil {
		t.Fatalf("GetMe failed: %v", err)
	}

	channel, err := client.CreateDirectChannel([]string{user.ID, user.ID})
	if err != nil {
		t.Skipf("CreateDirectChannel failed: %v", err)
	}

	// Create test file content
	testContent := "This is a test file from cc-connect integration test.\nGenerated at: " + time.Now().Format(time.RFC3339)
	testData := []byte(testContent)

	// Upload the file
	fileInfo, err := client.UploadFile(channel.ID, testData, "test_file.txt", "text/plain")
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	t.Logf("✅ File uploaded successfully! File ID: %s, Name: %s", fileInfo.ID, fileInfo.Name)

	// Create a post with the file attachment
	post, err := client.CreatePost(channel.ID, "Test file attachment from cc-connect", "", []string{fileInfo.ID})
	if err != nil {
		t.Fatalf("CreatePost with file failed: %v", err)
	}

	t.Logf("✅ Post with file created! Post ID: %s", post.ID)

	// Clean up
	_, _ = client.request("DELETE", fmt.Sprintf("/posts/%s", post.ID), nil)
}

// TestFullIntegration tests the full platform lifecycle (requires valid auth)
func TestFullIntegration(t *testing.T) {
	if skipIfNoConnection(t) {
		return
	}
	_ = skipIfNoAuth(t)
	// Skip in short mode
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	opts := map[string]any{
		"base_url": testBaseURL,
		"token":    testToken,
	}

	platform, err := New(opts)
	if err != nil {
		t.Fatalf("Failed to create platform: %v", err)
	}

	t.Logf("Platform created: %s", platform.Name())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handler := func(p core.Platform, msg *core.Message) {
		t.Logf("Received message: %+v", msg)
	}

	err = platform.Start(handler)
	if err != nil {
		t.Fatalf("Platform start failed: %v", err)
	}
	defer platform.Stop()

	t.Log("Platform started successfully")

	// Wait for connection
	select {
	case <-ctx.Done():
		t.Log("Context timeout reached")
	case <-time.After(3 * time.Second):
		t.Log("Integration test completed")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
