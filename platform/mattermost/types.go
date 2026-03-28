package mattermost

// Post represents a Mattermost post/message
type Post struct {
	ID        string                 `json:"id"`
	UserID    string                 `json:"user_id"`
	ChannelID string                 `json:"channel_id"`
	Message   string                 `json:"message"`
	FileIDs   []string               `json:"file_ids"`
	Type      string                 `json:"type"`
	RootID    string                 `json:"root_id"`
	CreateAt  int64                  `json:"create_at"`
	UpdateAt  int64                  `json:"update_at"`
	Props     map[string]interface{} `json:"props"`
}

// User represents a Mattermost user
type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Nickname  string `json:"nickname"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
}

// Channel represents a Mattermost channel
type Channel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"` // "O"=public, "P"=private, "D"=direct, "G"=group
	TeamID      string `json:"team_id"`
}

// FileInfo represents uploaded file metadata
type FileInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

// Team represents a Mattermost team
type Team struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// EventPayload represents a WebSocket event from Mattermost
type EventPayload struct {
	Event     string                 `json:"event"`
	Data      map[string]interface{} `json:"data"`
	Broadcast map[string]string      `json:"broadcast"`
	Seq       int64                  `json:"seq"`
}

// AuthChallenge is sent to authenticate WebSocket connection
type AuthChallenge struct {
	Seq    int64                  `json:"seq"`
	Action string                 `json:"action"`
	Data   map[string]interface{} `json:"data"`
}

// TypingRequest represents typing indicator request
type TypingRequest struct {
	ChannelID string `json:"channel_id"`
	ParentID  string `json:"parent_id,omitempty"`
}

// PostRequest represents create/update post request
type PostRequest struct {
	ChannelID string                 `json:"channel_id"`
	Message   string                 `json:"message"`
	RootID    string                 `json:"root_id,omitempty"`
	FileIDs   []string               `json:"file_ids,omitempty"`
	Props     map[string]interface{} `json:"props,omitempty"`
}

// DirectChannelRequest represents create DM channel request
type DirectChannelRequest []string

// UploadResponse represents file upload response
type UploadResponse struct {
	FileInfos []FileInfo `json:"file_infos"`
}

// APIError represents Mattermost API error response
type APIError struct {
	StatusCode int    `json:"status_code"`
	Message    string `json:"message"`
	ID         string `json:"id"`
	RequestID  string `json:"request_id"`
}

// Attachment represents message attachment (for interactive buttons)
type Attachment struct {
	Text    string   `json:"text"`
	Actions []Action `json:"actions,omitempty"`
}

// Action represents an attachment action/button
type Action struct {
	ID          string                 `json:"id"`
	Type        string                 `json:"type"` // "button" or "select"
	Name        string                 `json:"name"`
	Style       string                 `json:"style,omitempty"` // "default", "primary", "danger"
	Integration *ActionIntegration     `json:"integration,omitempty"`
}

// ActionIntegration defines button callback behavior
type ActionIntegration struct {
	URL     string                 `json:"url"`
	Context map[string]interface{} `json:"context"`
}
