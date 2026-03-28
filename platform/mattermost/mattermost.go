package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterPlatform("mattermost", New)
}

// replyContext holds the context needed to reply to a message
type replyContext struct {
	channelID string
	postID    string
	rootID    string // for threaded replies
}

// Platform implements the Mattermost platform
type Platform struct {
	baseURL               string
	token                 string
	allowFrom             string
	shareSessionInChannel bool
	groupReplyAll         bool

	mu               sync.RWMutex
	client           *Client
	ws               *WSConnection
	handler           core.MessageHandler
	lifecycleHandler  core.PlatformLifecycleHandler
	botUser           *User
	cancel            context.CancelFunc
	stopping          bool
	everConnected     bool
}

// New creates a new Mattermost platform instance
func New(opts map[string]any) (core.Platform, error) {
	baseURL, _ := opts["base_url"].(string)
	if baseURL == "" {
		return nil, fmt.Errorf("mattermost: base_url is required")
	}
	token, _ := opts["token"].(string)
	if token == "" {
		return nil, fmt.Errorf("mattermost: token is required")
	}

	allowFrom, _ := opts["allow_from"].(string)
	core.CheckAllowFrom("mattermost", allowFrom)

	shareSessionInChannel, _ := opts["share_session_in_channel"].(bool)
	groupReplyAll, _ := opts["group_reply_all"].(bool)

	return &Platform{
		baseURL:               baseURL,
		token:                 token,
		allowFrom:             allowFrom,
		shareSessionInChannel: shareSessionInChannel,
		groupReplyAll:         groupReplyAll,
	}, nil
}

// Name returns the platform name
func (p *Platform) Name() string { return "mattermost" }

// Start initializes the platform and starts listening for messages
func (p *Platform) Start(handler core.MessageHandler) error {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return fmt.Errorf("mattermost: platform stopped")
	}
	p.handler = handler
	p.mu.Unlock()

	// Create client
	p.client = NewClient(p.baseURL, p.token)

	// Get bot user info
	me, err := p.client.GetMe()
	if err != nil {
		return fmt.Errorf("mattermost: get bot user: %w", err)
	}
	p.mu.Lock()
	p.botUser = me
	p.mu.Unlock()

	slog.Info("mattermost: connected as", "username", me.Username, "id", me.ID)

	// Create context for the connection loop
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancel = cancel
	p.mu.Unlock()

	// Start connection loop
	go p.connectLoop(ctx)

	return nil
}

// SetLifecycleHandler sets the lifecycle handler for async recovery
func (p *Platform) SetLifecycleHandler(h core.PlatformLifecycleHandler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lifecycleHandler = h
}

// connectLoop manages WebSocket connection with reconnection
func (p *Platform) connectLoop(ctx context.Context) {
	backoff := reconnectInitialDelay

	for {
		if ctx.Err() != nil || p.isStopping() {
			return
		}

		startedAt := time.Now()
		err := p.runConnection(ctx)
		if ctx.Err() != nil || p.isStopping() {
			return
		}

		// Calculate backoff
		wait := backoff
		if time.Since(startedAt) >= stableConnectionWindow {
			wait = reconnectInitialDelay
			backoff = reconnectInitialDelay
		} else if backoff < reconnectMaxDelay {
			backoff *= 2
			if backoff > reconnectMaxDelay {
				backoff = reconnectMaxDelay
			}
		}

		if err != nil {
			slog.Warn("mattermost: connection lost, reconnecting", "error", err, "backoff", wait)
			p.notifyUnavailable(err)
		}

		// Wait before reconnecting
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// runConnection establishes and maintains a single WebSocket connection
func (p *Platform) runConnection(ctx context.Context) error {
	ws := NewWSConnection(p.client, p.token)
	ws.SetOnPosted(p.handlePosted)

	if err := ws.Connect(ctx); err != nil {
		return err
	}

	p.mu.Lock()
	p.ws = ws
	p.everConnected = true
	p.mu.Unlock()

	// Notify ready
	p.emitReady()

	// Listen for messages
	err := ws.Listen(ctx)

	// Cleanup
	p.mu.Lock()
	p.ws = nil
	p.mu.Unlock()
	ws.Close()

	return err
}

// handlePosted handles incoming posted events
func (p *Platform) handlePosted(post *Post, payload *EventPayload) {
	slog.Debug("mattermost: handlePosted called", "post_id", post.ID, "user_id", post.UserID, "message", post.Message)

	if p.handler == nil {
		slog.Debug("mattermost: no handler set")
		return
	}

	// Skip messages from the bot itself
	botID := p.getBotID()
	if botID != "" && post.UserID == botID {
		slog.Debug("mattermost: skipping own message")
		return
	}

	// Check allow list
	if !core.AllowList(p.allowFrom, post.UserID) {
		slog.Debug("mattermost: message from unauthorized user", "user", post.UserID)
		return
	}

	// Get channel type and name from payload
	channelType, _ := payload.Data["channel_type"].(string)
	channelName, _ := payload.Data["channel_name"].(string)
	channelDisplayName, _ := payload.Data["channel_display_name"].(string)
	senderName, _ := payload.Data["sender_name"].(string)

	slog.Debug("mattermost: message details",
		"channel_type", channelType,
		"channel_name", channelName,
		"sender_name", senderName,
		"message", post.Message)

	// For group channels, check if we should respond
	if channelType == "O" || channelType == "P" {
		if !p.groupReplyAll {
			// Check if bot is mentioned
			if !p.isBotMentioned(post.Message, senderName) {
				slog.Debug("mattermost: ignoring group message (not mentioned)")
				return
			}
		}
	}

	// Build session key
	var sessionKey string
	if p.shareSessionInChannel {
		sessionKey = fmt.Sprintf("mattermost:%s", post.ChannelID)
	} else {
		sessionKey = fmt.Sprintf("mattermost:%s:%s", post.ChannelID, post.UserID)
	}

	// Get user info for display name
	userName := senderName
	if userName == "" {
		user, err := p.client.GetUser(post.UserID)
		if err == nil && user.Username != "" {
			userName = user.Username
		} else {
			userName = post.UserID
		}
	}

	// Use channel display name if available
	chatName := channelDisplayName
	if chatName == "" {
		chatName = channelName
	}

	// Strip bot mention from message
	message := p.stripBotMention(post.Message)

	// Build reply context
	rctx := replyContext{
		channelID: post.ChannelID,
		postID:    post.ID,
		rootID:    post.RootID,
	}

	// Handle file attachments
	var images []core.ImageAttachment
	var files []core.FileAttachment

	if len(post.FileIDs) > 0 {
		for _, fileID := range post.FileIDs {
			data, err := p.client.DownloadFile(fileID)
			if err != nil {
				slog.Warn("mattermost: failed to download file", "file_id", fileID, "error", err)
				continue
			}
			// For simplicity, treat all files as generic file attachments
			// In a more sophisticated implementation, we'd check MIME type
			files = append(files, core.FileAttachment{
				Data:     data,
				FileName: fileID,
			})
		}
	}

	// Create and dispatch message
	msg := &core.Message{
		SessionKey: sessionKey,
		Platform:   "mattermost",
		MessageID:  post.ID,
		UserID:     post.UserID,
		UserName:   userName,
		ChatName:   chatName,
		Content:    message,
		Images:     images,
		Files:      files,
		ReplyCtx:   rctx,
	}

	slog.Info("mattermost: dispatching message to handler", "session_key", sessionKey, "content", message)
	p.handler(p, msg)
}

// isBotMentioned checks if the bot is mentioned in the message
func (p *Platform) isBotMentioned(message, senderName string) bool {
	botUsername := p.getBotUsername()
	if botUsername == "" {
		return false
	}

	slog.Debug("mattermost: checking mention", "bot_username", botUsername, "message", message)

	// Check for @username mention
	if strings.Contains(message, "@"+botUsername) {
		return true
	}

	// Check for direct mention (bot name at start)
	if strings.HasPrefix(strings.TrimSpace(message), botUsername) {
		return true
	}

	// Also check for mention without @ (common in some clients)
	if strings.Contains(strings.ToLower(message), strings.ToLower(botUsername)) {
		return true
	}

	return false
}

// stripBotMention removes bot mention from the message
func (p *Platform) stripBotMention(message string) string {
	botUsername := p.getBotUsername()
	if botUsername == "" {
		return message
	}

	// Remove @username mentions
	message = strings.ReplaceAll(message, "@"+botUsername, "")
	return strings.TrimSpace(message)
}

// getBotID returns the bot user ID
func (p *Platform) getBotID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.botUser == nil {
		return ""
	}
	return p.botUser.ID
}

// getBotUsername returns the bot username
func (p *Platform) getBotUsername() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.botUser == nil {
		return ""
	}
	return p.botUser.Username
}

// Reply sends a reply to a message
func (p *Platform) Reply(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("mattermost: invalid reply context type %T", rctx)
	}

	slog.Debug("mattermost: Reply called", "channel_id", rc.channelID, "post_id", rc.postID, "root_id", rc.rootID, "content_len", len(content))

	// Split long messages
	chunks := core.SplitMessageCodeFenceAware(content, 15000)
	for i, chunk := range chunks {
		rootID := rc.rootID
		if rootID == "" {
			// For thread reply mode, use the post ID as root
			rootID = rc.postID
		}

		slog.Debug("mattermost: sending reply", "chunk", i+1, "total_chunks", len(chunks), "channel_id", rc.channelID)
		post, err := p.client.CreatePost(rc.channelID, chunk, rootID, nil)
		if err != nil {
			slog.Error("mattermost: reply failed", "error", err)
			return fmt.Errorf("mattermost: reply: %w", err)
		}
		slog.Debug("mattermost: reply sent", "post_id", post.ID)
	}
	return nil
}

// Send sends a new message (not a reply)
func (p *Platform) Send(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("mattermost: invalid reply context type %T", rctx)
	}

	// Split long messages
	chunks := core.SplitMessageCodeFenceAware(content, 15000)
	for _, chunk := range chunks {
		_, err := p.client.CreatePost(rc.channelID, chunk, "", nil)
		if err != nil {
			return fmt.Errorf("mattermost: send: %w", err)
		}
	}
	return nil
}

// SendImage sends an image attachment
func (p *Platform) SendImage(ctx context.Context, rctx any, img core.ImageAttachment) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("mattermost: invalid reply context type %T", rctx)
	}

	filename := img.FileName
	if filename == "" {
		filename = "image"
	}

	mimeType := img.MimeType
	if mimeType == "" {
		mimeType = "image/png"
	}

	fileInfo, err := p.client.UploadFile(rc.channelID, img.Data, filename, mimeType)
	if err != nil {
		return fmt.Errorf("mattermost: upload image: %w", err)
	}

	rootID := rc.rootID
	if rootID == "" {
		rootID = rc.postID
	}

	_, err = p.client.CreatePost(rc.channelID, "", rootID, []string{fileInfo.ID})
	if err != nil {
		return fmt.Errorf("mattermost: send image post: %w", err)
	}

	return nil
}

// SendFile sends a file attachment
func (p *Platform) SendFile(ctx context.Context, rctx any, file core.FileAttachment) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("mattermost: invalid reply context type %T", rctx)
	}

	filename := file.FileName
	if filename == "" {
		filename = "attachment"
	}

	mimeType := file.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	fileInfo, err := p.client.UploadFile(rc.channelID, file.Data, filename, mimeType)
	if err != nil {
		return fmt.Errorf("mattermost: upload file: %w", err)
	}

	rootID := rc.rootID
	if rootID == "" {
		rootID = rc.postID
	}

	_, err = p.client.CreatePost(rc.channelID, "", rootID, []string{fileInfo.ID})
	if err != nil {
		return fmt.Errorf("mattermost: send file post: %w", err)
	}

	return nil
}

// StartTyping sends a typing indicator
func (p *Platform) StartTyping(ctx context.Context, rctx any) (stop func()) {
	rc, ok := rctx.(replyContext)
	if !ok {
		return func() {}
	}

	// Send initial typing indicator
	if err := p.client.SendTyping(rc.channelID); err != nil {
		slog.Debug("mattermost: initial typing send failed", "error", err)
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.client.SendTyping(rc.channelID); err != nil {
					slog.Debug("mattermost: typing send failed", "error", err)
				}
			}
		}
	}()

	return func() { close(done) }
}

// UpdateMessage updates an existing message
func (p *Platform) UpdateMessage(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("mattermost: invalid reply context type %T", rctx)
	}

	if rc.postID == "" {
		return fmt.Errorf("mattermost: no post ID for update")
	}

	_, err := p.client.UpdatePost(rc.postID, content)
	if err != nil {
		return fmt.Errorf("mattermost: update message: %w", err)
	}
	return nil
}

// SendWithButtons sends a message with inline buttons
func (p *Platform) SendWithButtons(ctx context.Context, rctx any, content string, buttons [][]core.ButtonOption) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("mattermost: invalid reply context type %T", rctx)
	}

	// Build Mattermost attachments with actions
	attachments := p.buildButtonAttachments(buttons)

	props := map[string]interface{}{
		"attachments": attachments,
	}

	rootID := rc.rootID
	if rootID == "" {
		rootID = rc.postID
	}

	// Create post with props
	payload := map[string]interface{}{
		"channel_id": rc.channelID,
		"message":    content,
		"props":      props,
	}
	if rootID != "" {
		payload["root_id"] = rootID
	}

	// Use raw request since CreatePost doesn't support props
	body, err := p.client.request("POST", "/posts", payload)
	if err != nil {
		return fmt.Errorf("mattermost: send with buttons: %w", err)
	}

	// Parse response
	var post Post
	if err := json.Unmarshal(body, &post); err != nil {
		return fmt.Errorf("mattermost: parse button post response: %w", err)
	}

	return nil
}

// buildButtonAttachments converts core buttons to Mattermost attachments
func (p *Platform) buildButtonAttachments(buttons [][]core.ButtonOption) []Attachment {
	var attachments []Attachment

	for _, row := range buttons {
		var actions []Action
		for _, btn := range row {
			actions = append(actions, Action{
				ID:   btn.Data,
				Type: "button",
				Name: btn.Text,
				Integration: &ActionIntegration{
					URL: p.client.baseURL + "/api/v4/posts",
					Context: map[string]interface{}{
						"action": btn.Data,
					},
				},
			})
		}
		if len(actions) > 0 {
			attachments = append(attachments, Attachment{
				Actions: actions,
			})
		}
	}

	return attachments
}

// ReconstructReplyCtx reconstructs a reply context from a session key
func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	// Format: mattermost:{channelID} or mattermost:{channelID}:{userID}
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) < 2 || parts[0] != "mattermost" {
		return nil, fmt.Errorf("mattermost: invalid session key %q", sessionKey)
	}
	channelID := parts[1]
	return replyContext{channelID: channelID}, nil
}

// Stop stops the platform
func (p *Platform) Stop() error {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return nil
	}
	p.stopping = true
	cancel := p.cancel
	ws := p.ws
	p.cancel = nil
	p.ws = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if ws != nil {
		ws.Close()
	}
	return nil
}

// isStopping returns whether the platform is stopping
func (p *Platform) isStopping() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stopping
}

// emitReady notifies the lifecycle handler that the platform is ready
func (p *Platform) emitReady() {
	p.mu.RLock()
	handler := p.lifecycleHandler
	p.mu.RUnlock()

	if handler != nil {
		handler.OnPlatformReady(p)
	}
}

// notifyUnavailable notifies the lifecycle handler of unavailability
func (p *Platform) notifyUnavailable(err error) {
	var handler core.PlatformLifecycleHandler

	p.mu.Lock()
	if p.stopping || err == nil {
		p.mu.Unlock()
		return
	}
	handler = p.lifecycleHandler
	p.mu.Unlock()

	if handler != nil {
		handler.OnPlatformUnavailable(p, err)
	}
}

// FormattingInstructions returns Mattermost-specific formatting instructions
func (p *Platform) FormattingInstructions() string {
	return "You are messaging via Mattermost. Format your responses using standard Markdown. " +
		"Code blocks should use triple backticks with language hints. " +
		"For bullet points, use - or *. " +
		"For numbered lists, use 1. 2. 3. format."
}

// Verify interface implementations
var _ core.Platform = (*Platform)(nil)
var _ core.ImageSender = (*Platform)(nil)
var _ core.FileSender = (*Platform)(nil)
var _ core.TypingIndicator = (*Platform)(nil)
var _ core.MessageUpdater = (*Platform)(nil)
var _ core.InlineButtonSender = (*Platform)(nil)
var _ core.ReplyContextReconstructor = (*Platform)(nil)
var _ core.AsyncRecoverablePlatform = (*Platform)(nil)
var _ core.FormattingInstructionProvider = (*Platform)(nil)
