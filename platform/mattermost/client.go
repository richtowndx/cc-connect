package mattermost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Client is a Mattermost API client
type Client struct {
	baseURL    string
	apiBaseURL string
	token      string
	httpClient  *http.Client
}

// NewClient creates a new Mattermost API client
func NewClient(baseURL, token string) *Client {
	normalized := normalizeBaseURL(baseURL)
	return &Client{
		baseURL:    normalized,
		apiBaseURL: normalized + "/api/v4",
		token:      token,
		httpClient: &http.Client{
		Timeout: 60 * time.Second,
	},
	}
}

// normalizeBaseURL removes trailing slashes and /api/v4 suffix
func normalizeBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// Remove trailing slashes
	normalized := strings.TrimRight(trimmed, "/")
	// Remove /api/v4 suffix if present
	normalized = strings.TrimSuffix(normalized, "/api/v4")
	return normalized
}

// request makes an authenticated API request
func (c *Client) request(method, path string, body interface{}) ([]byte, error) {
	fullURL := c.apiBaseURL + path

	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
		return nil, fmt.Errorf("mattermost: marshal request: %w", err)
		}
		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequest(method, fullURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("mattermost: create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mattermost: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mattermost: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var apiErr APIError
		if json.Unmarshal(respBody, &apiErr) == nil {
			apiErr.StatusCode = resp.StatusCode
			return nil, &apiErr
		}
		return nil, fmt.Errorf("mattermost: API error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// GetMe retrieves the current authenticated user
func (c *Client) GetMe() (*User, error) {
	body, err := c.request("GET", "/users/me", nil)
	if err != nil {
		return nil, err
	}
	var user User
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("mattermost: unmarshal user: %w", err)
	}
	return &user, nil
}

// CreatePost creates a new post in a channel
func (c *Client) CreatePost(channelID, message, rootID string, fileIDs []string) (*Post, error) {
	payload := map[string]interface{}{
		"channel_id": channelID,
		"message":   message,
	}
	if rootID != "" {
		payload["root_id"] = rootID
	}
	if len(fileIDs) > 0 {
		payload["file_ids"] = fileIDs
	}

	body, err := c.request("POST", "/posts", payload)
	if err != nil {
		return nil, err
	}
	var post Post
	if err := json.Unmarshal(body, &post); err != nil {
		return nil, fmt.Errorf("mattermost: unmarshal post: %w", err)
	}
	return &post, nil
}

// UpdatePost updates an existing post
func (c *Client) UpdatePost(postID, message string) (*Post, error) {
	payload := map[string]interface{}{
		"id":      postID,
		"message": message,
	}

	body, err := c.request("PUT", "/posts/"+postID, payload)
	if err != nil {
		return nil, err
	}
	var post Post
	if err := json.Unmarshal(body, &post); err != nil {
		return nil, fmt.Errorf("mattermost: unmarshal post: %w", err)
	}
	return &post, nil
}

// UploadFile uploads a file to a channel
func (c *Client) UploadFile(channelID string, data []byte, filename, contentType string) (*FileInfo, error) {
	fullURL := c.apiBaseURL + "/files"

	// Create multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add channel_id field
	if err := writer.WriteField("channel_id", channelID); err != nil {
		return nil, fmt.Errorf("mattermost: write channel_id field: %w", err)
	}

	// Add file
	part, err := writer.CreateFormFile("files", filename)
	if err != nil {
		return nil, fmt.Errorf("mattermost: create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("mattermost: write file data: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("mattermost: close multipart writer: %w", err)
	}

	req, err := http.NewRequest("POST", fullURL, &buf)
	if err != nil {
		return nil, fmt.Errorf("mattermost: create upload request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mattermost: upload request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mattermost: read upload response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mattermost: upload failed %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		FileInfos []*FileInfo `json:"file_infos"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("mattermost: unmarshal upload response: %w", err)
	}
	if len(result.FileInfos) == 0 {
		return nil, fmt.Errorf("mattermost: upload returned no file info")
	}
	return result.FileInfos[0], nil
}

// DownloadFile downloads a file by ID
func (c *Client) DownloadFile(fileID string) ([]byte, error) {
	fullURL := c.apiBaseURL + "/files/" + fileID

	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("mattermost: create download request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mattermost: download request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mattermost: download failed %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}

// GetFileLink returns the public URL for a file
func (c *Client) GetFileLink(fileID string) string {
	return c.apiBaseURL + "/files/" + fileID
}

// SendTyping sends a typing indicator to a channel
func (c *Client) SendTyping(channelID string) error {
	payload := map[string]interface{}{
		"channel_id": channelID,
	}
	_, err := c.request("POST", "/users/me/typing", payload)
	return err
}

// CreateDirectChannel creates or gets a direct message channel
func (c *Client) CreateDirectChannel(userIDs []string) (*Channel, error) {
	body, err := c.request("POST", "/channels/direct", userIDs)
	if err != nil {
		return nil, err
	}
	var channel Channel
	if err := json.Unmarshal(body, &channel); err != nil {
		return nil, fmt.Errorf("mattermost: unmarshal channel: %w", err)
	}
	return &channel, nil
}

// GetChannel retrieves channel information
func (c *Client) GetChannel(channelID string) (*Channel, error) {
	body, err := c.request("GET", "/channels/"+channelID, nil)
	if err != nil {
		return nil, err
	}
	var channel Channel
	if err := json.Unmarshal(body, &channel); err != nil {
		return nil, fmt.Errorf("mattermost: unmarshal channel: %w", err)
	}
	return &channel, nil
}

// GetUser retrieves user information
func (c *Client) GetUser(userID string) (*User, error) {
	body, err := c.request("GET", "/users/"+userID, nil)
	if err != nil {
		return nil, err
	}
	var user User
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("mattermost: unmarshal user: %w", err)
	}
	return &user, nil
}

// buildWebSocketURL converts HTTP URL to WebSocket URL
func (c *Client) buildWebSocketURL() string {
	wsURL := strings.Replace(c.baseURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	return wsURL + "/api/v4/websocket"
}

// Error implements the error interface for APIError
func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("mattermost API error %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("mattermost API error %d", e.StatusCode)
}
