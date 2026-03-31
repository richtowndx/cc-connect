package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	reconnectInitialDelay  = time.Second
	reconnectMaxDelay      = 30 * time.Second
	stableConnectionWindow = 10 * time.Second

	// Keepalive settings
	pingInterval   = 30 * time.Second
	pongTimeout    = 40 * time.Second
	maxMissedPongs = 3
)

// WSConnection handles WebSocket connection to Mattermost
type WSConnection struct {
	client *Client
	wsURL  string
	token  string

	mu            sync.RWMutex
	conn          *websocket.Conn
	seq           int64
	stopping      bool
	everConnected bool

	onPosted func(post *Post, payload *EventPayload)

	dialer websocket.Dialer

	// keepalive state
	lastPong    time.Time
	missedPongs int
	pingStopCh  chan struct{}
	pingDoneCh  chan struct{}
	dead        bool // connection is considered dead
}

// NewWSConnection creates a new WebSocket connection handler
func NewWSConnection(client *Client, token string) *WSConnection {
	return &WSConnection{
		client: client,
		wsURL:  client.buildWebSocketURL(),
		token:  token,
		dialer: websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
		},
	}
}

// SetOnPosted sets the callback for posted events
func (ws *WSConnection) SetOnPosted(fn func(post *Post, payload *EventPayload)) {
	ws.onPosted = fn
}

// Connect establishes WebSocket connection and authenticates
func (ws *WSConnection) Connect(ctx context.Context) error {
	ws.mu.Lock()
	if ws.stopping {
		ws.mu.Unlock()
		return fmt.Errorf("mattermost: websocket stopping")
	}
	ws.mu.Unlock()

	slog.Debug("mattermost: connecting to websocket", "url", ws.wsURL)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+ws.token)

	conn, resp, err := ws.dialer.DialContext(ctx, ws.wsURL, headers)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("mattermost: websocket dial failed: %w (status %d)", err, resp.StatusCode)
		}
		return fmt.Errorf("mattermost: websocket dial failed: %w", err)
	}

	ws.mu.Lock()
	ws.conn = conn
	ws.lastPong = time.Now()
	ws.missedPongs = 0
	ws.dead = false
	ws.pingStopCh = make(chan struct{})
	ws.pingDoneCh = make(chan struct{})
	ws.mu.Unlock()

	// Set up ping/pong handlers
	conn.SetPingHandler(func(appData string) error {
		// Automatically respond to server pings with pongs
		if err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second)); err != nil {
			slog.Warn("mattermost: pong write failed", "error", err)
		}
		return nil
	})
	conn.SetPongHandler(func(appData string) error {
		ws.mu.Lock()
		ws.lastPong = time.Now()
		ws.missedPongs = 0
		ws.mu.Unlock()
		return nil
	})

	// Send authentication challenge
	if err := ws.authenticate(); err != nil {
		conn.Close()
		return fmt.Errorf("mattermost: websocket auth failed: %w", err)
	}

	ws.mu.Lock()
	ws.everConnected = true
	ws.mu.Unlock()

	slog.Info("mattermost: websocket connected")

	// Start keepalive goroutine
	go ws.keepalive()

	return nil
}

// authenticate sends the authentication challenge
func (ws *WSConnection) authenticate() error {
	ws.mu.Lock()
	ws.seq++
	seq := ws.seq
	conn := ws.conn
	ws.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("mattermost: no connection")
	}

	authMsg := map[string]interface{}{
		"seq":    seq,
		"action": "authentication_challenge",
		"data": map[string]string{
			"token": ws.token,
		},
	}

	if err := conn.WriteJSON(authMsg); err != nil {
		return fmt.Errorf("mattermost: send auth: %w", err)
	}

	return nil
}

// keepalive sends periodic pings and monitors pong responses
func (ws *WSConnection) keepalive() {
	defer close(ws.pingDoneCh)

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ws.pingStopCh:
			return
		default:
		}

		ws.mu.RLock()
		conn := ws.conn
		dead := ws.dead
		lastPong := ws.lastPong
		ws.mu.RUnlock()

		if conn == nil || dead {
			return
		}

		// Check if we've missed too many pongs
		if time.Since(lastPong) > pongTimeout {
			ws.mu.Lock()
			ws.missedPongs++
			missed := ws.missedPongs
			ws.mu.Unlock()

			slog.Warn("mattermost: missed pong", "missed_count", missed, "max", maxMissedPongs)

			if missed >= maxMissedPongs {
				ws.mu.Lock()
				ws.dead = true
				ws.mu.Unlock()
				slog.Error("mattermost: connection dead, too many missed pongs")
				conn.Close()
				return
			}
		}

		// Send a ping
		if err := conn.WriteControl(websocket.PingMessage, []byte(fmt.Sprintf("keepalive-%d", time.Now().Unix())), time.Now().Add(5*time.Second)); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Warn("mattermost: ping write failed", "error", err)
			}
			return
		}

		// Wait for next tick
		select {
		case <-ws.pingStopCh:
			return
		case <-ticker.C:
		}
	}
}

// Listen starts listening for WebSocket messages
func (ws *WSConnection) Listen(ctx context.Context) error {
	ws.mu.RLock()
	conn := ws.conn
	ws.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("mattermost: no connection")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		ws.mu.RLock()
		conn := ws.conn
		ws.mu.RUnlock()

		if conn == nil {
			return fmt.Errorf("mattermost: connection lost")
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Warn("mattermost: websocket read error", "error", err)
			}
			return err
		}

		ws.handleMessage(message)
	}
}

// handleMessage processes incoming WebSocket messages
func (ws *WSConnection) handleMessage(data []byte) {
	slog.Debug("mattermost: received websocket message", "raw", string(data))

	var payload EventPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		slog.Debug("mattermost: failed to parse websocket message", "error", err)
		return
	}

	slog.Debug("mattermost: parsed event", "event", payload.Event)
	switch payload.Event {
	case "posted":
		ws.handlePostedEvent(&payload)
	case "reaction_added", "reaction_removed":
		// TODO: handle reactions if needed
		slog.Debug("mattermost: received reaction event", "event", payload.Event)
	case "typing":
		// Typing events - ignore
	case "status_change":
		// Status change events - ignore
	case "hello":
		// Server hello - connection confirmed
		slog.Debug("mattermost: websocket hello received")
	default:
		slog.Debug("mattermost: unhandled event", "event", payload.Event)
	}
}

// handlePostedEvent handles posted events
func (ws *WSConnection) handlePostedEvent(payload *EventPayload) {
	slog.Debug("mattermost: handlePostedEvent called", "data_keys", getMapKeys(payload.Data))

	if ws.onPosted == nil {
		slog.Debug("mattermost: onPosted callback is nil")
		return
	}

	// Extract post from data
	postData, ok := payload.Data["post"]
	if !ok {
		slog.Debug("mattermost: no post in payload data")
		return
	}

	// Post can be a string (JSON) or an object
	var post Post
	switch v := postData.(type) {
	case string:
		if err := json.Unmarshal([]byte(v), &post); err != nil {
			slog.Debug("mattermost: failed to parse post string", "error", err)
			return
		}
	case map[string]interface{}:
		data, err := json.Marshal(v)
		if err != nil {
			slog.Debug("mattermost: failed to marshal post object", "error", err)
			return
		}
		if err := json.Unmarshal(data, &post); err != nil {
			slog.Debug("mattermost: failed to parse post object", "error", err)
			return
		}
	default:
		slog.Debug("mattermost: unexpected post type", "type", fmt.Sprintf("%T", postData))
		return
	}

	ws.onPosted(&post, payload)
}

// Close closes the WebSocket connection
func (ws *WSConnection) Close() {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	ws.stopping = true

	// Stop the keepalive goroutine
	if ws.pingStopCh != nil {
		close(ws.pingStopCh)
	}
	if ws.pingDoneCh != nil {
		ws.pingDoneCh = nil
	}

	if ws.conn != nil {
		ws.conn.Close()
		ws.conn = nil
	}
}

// IsStopping returns whether the connection is stopping
func (ws *WSConnection) IsStopping() bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.stopping
}

// HasEverConnected returns whether the connection was ever established
func (ws *WSConnection) HasEverConnected() bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.everConnected
}

// getMapKeys returns the keys of a map for logging
func getMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
