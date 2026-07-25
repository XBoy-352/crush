package remotecontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Config struct {
	RelayURL string `json:"relay_url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type EventMessage struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Timestamp int64           `json:"timestamp"`
}

type StreamChunkPayload struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ToolRequestPayload struct {
	RequestID   string `json:"request_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
}

type ToolResponsePayload struct {
	RequestID string `json:"request_id"`
	Approved  bool   `json:"approved"`
}

type Client struct {
	cfg           Config
	ws            *websocket.Conn
	sessionID     string
	mu            sync.RWMutex
	pendingTools  map[string]chan bool
	promptHandler func(prompt string)
	cancelHandler func()
	closeCh       chan struct{}
}

func NewClient(cfg Config) *Client {
	if cfg.RelayURL == "" {
		cfg.RelayURL = "ws://localhost:8080"
	}
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	return &Client{
		cfg:          cfg,
		pendingTools: make(map[string]chan bool),
		closeCh:      make(chan struct{}),
	}
}

func (c *Client) SetPromptHandler(h func(prompt string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.promptHandler = h
}

func (c *Client) SetCancelHandler(h func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelHandler = h
}

// Connect authenticates with the relay server and opens the WebSocket connection.
func (c *Client) Connect(ctx context.Context, sessionID string) error {
	c.sessionID = sessionID

	// Derive HTTP URL from WS URL
	httpBase := strings.Replace(c.cfg.RelayURL, "wss://", "https://", 1)
	httpBase = strings.Replace(httpBase, "ws://", "http://", 1)

	loginBody, _ := json.Marshal(map[string]string{
		"username": c.cfg.Username,
		"password": c.cfg.Password,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpBase+"/api/login", bytes.NewBuffer(loginBody))
	if err != nil {
		return fmt.Errorf("failed to create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to relay server: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var lResp struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &lResp); err != nil || !lResp.Success {
		return fmt.Errorf("relay authentication failed: %s", lResp.Message)
	}

	wsURL := fmt.Sprintf("%s/ws/cli?token=%s&session_id=%s", c.cfg.RelayURL, url.QueryEscape(lResp.Token), url.QueryEscape(sessionID))
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect websocket: %w", err)
	}

	c.mu.Lock()
	c.ws = ws
	c.mu.Unlock()

	// Send initial session registration
	regPayload, _ := json.Marshal(map[string]interface{}{
		"status":     "active",
		"session_id": sessionID,
		"started_at": time.Now().Unix(),
	})
	_ = c.SendEvent("register_session", regPayload)

	// Start reader loop
	go c.readLoop()

	return nil
}

func (c *Client) SendEvent(evtType string, payload json.RawMessage) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.ws == nil {
		return fmt.Errorf("websocket not connected")
	}

	msg := EventMessage{
		Type:      evtType,
		SessionID: c.sessionID,
		Payload:   payload,
		Timestamp: time.Now().Unix(),
	}

	return c.ws.WriteJSON(msg)
}

func (c *Client) SendStreamChunk(role, content string) error {
	payload, err := json.Marshal(StreamChunkPayload{Role: role, Content: content})
	if err != nil {
		return err
	}
	return c.SendEvent("stream_chunk", payload)
}

func (c *Client) RequestToolApproval(ctx context.Context, reqID, toolName, desc, command string) (bool, error) {
	respCh := make(chan bool, 1)

	c.mu.Lock()
	c.pendingTools[reqID] = respCh
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pendingTools, reqID)
		c.mu.Unlock()
	}()

	payload, _ := json.Marshal(ToolRequestPayload{
		RequestID:   reqID,
		ToolName:    toolName,
		Description: desc,
		Command:     command,
	})

	if err := c.SendEvent("tool_request", payload); err != nil {
		return false, err
	}

	select {
	case approved := <-respCh:
		return approved, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (c *Client) readLoop() {
	defer func() {
		close(c.closeCh)
	}()

	for {
		c.mu.RLock()
		ws := c.ws
		c.mu.RUnlock()

		if ws == nil {
			return
		}

		var msg EventMessage
		if err := ws.ReadJSON(&msg); err != nil {
			slog.Debug("Remote control websocket closed", "err", err)
			return
		}

		switch msg.Type {
		case "send_prompt":
			var p struct {
				Prompt string `json:"prompt"`
			}
			if err := json.Unmarshal(msg.Payload, &p); err == nil && p.Prompt != "" {
				c.mu.RLock()
				handler := c.promptHandler
				c.mu.RUnlock()
				if handler != nil {
					handler(p.Prompt)
				}
			}

		case "tool_response":
			var tResp ToolResponsePayload
			if err := json.Unmarshal(msg.Payload, &tResp); err == nil {
				c.mu.RLock()
				ch, ok := c.pendingTools[tResp.RequestID]
				c.mu.RUnlock()
				if ok {
					ch <- tResp.Approved
				}
			}

		case "cancel_task":
			c.mu.RLock()
			handler := c.cancelHandler
			c.mu.RUnlock()
			if handler != nil {
				handler()
			}
		}
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ws != nil {
		err := c.ws.Close()
		c.ws = nil
		return err
	}
	return nil
}
