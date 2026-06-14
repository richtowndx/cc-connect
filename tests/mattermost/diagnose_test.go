package mattermost_test

import (
	"io"
	"net/http"
	"testing"
	"time"
)

// TestMattermostServerConfig checks Mattermost server configuration
func TestMattermostServerConfig(t *testing.T) {
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"

	client := &http.Client{Timeout: 30 * time.Second}

	// Get server config
	t.Log("Checking Mattermost server configuration...")

	// Try to get config (may require admin privileges)
	req, _ := http.NewRequest("GET", baseURL+"/api/v4/config", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Logf("Failed to get config: %v", err)
	} else {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 200 {
			t.Logf("Config response: %s", string(body))
		} else {
			t.Logf("Config request status: %d, body: %s", resp.StatusCode, string(body))
		}
	}

	// Check websocket endpoint
	t.Log("\nChecking WebSocket endpoint availability...")
	req, _ = http.NewRequest("GET", baseURL+"/api/v4/websocket", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil {
		t.Logf("WebSocket endpoint check failed: %v", err)
	} else {
		defer resp.Body.Close()
		t.Logf("WebSocket endpoint status: %d (expected 400 or upgrade required)", resp.StatusCode)
	}

	// Get system info
	t.Log("\nChecking system info...")
	req, _ = http.NewRequest("GET", baseURL+"/api/v4/system/ping", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Logf("Ping failed: %v", err)
	} else {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Ping response: %s", string(body))
	}

	// Check if user is online
	t.Log("\nChecking user status...")
	req, _ = http.NewRequest("GET", baseURL+"/api/v4/users/me/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil {
		t.Logf("Status check failed: %v", err)
	} else {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("User status: %s", string(body))
	}
}

// TestMattermostClusterInfo checks cluster configuration
func TestMattermostClusterInfo(t *testing.T) {
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"

	client := &http.Client{Timeout: 30 * time.Second}

	// Check cluster status
	req, _ := http.NewRequest("GET", baseURL+"/api/v4/cluster/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Logf("Cluster status check failed: %v", err)
	} else {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Cluster status: %s", string(body))
	}

	// Check analytics (active users)
	req, _ = http.NewRequest("GET", baseURL+"/api/v4/analytics/old?name=user_posts_per_day", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil {
		t.Logf("Analytics check failed: %v", err)
	} else {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Analytics: %s", string(body))
	}
}

// TestMattermostNotifyCheck checks notification settings
func TestMattermostNotifyCheck(t *testing.T) {
	baseURL := "http://10.8.5.5:8065"
	token := "r8yec61g3t8stq6bwzu5mizw5e"

	client := &http.Client{Timeout: 30 * time.Second}

	// Get user's notification settings
	req, _ := http.NewRequest("GET", baseURL+"/api/v4/users/me/notify_props", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Logf("Notify props check failed: %v", err)
	} else {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Notify props: %s", string(body))
	}

	// Get channel members to see who's in the channel
	req, _ = http.NewRequest("GET", baseURL+"/api/v4/channels/4kh7kgckdpyqfna34yrufzesha/members", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil {
		t.Logf("Channel members check failed: %v", err)
	} else {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Channel members: %s", string(body))
	}
}

