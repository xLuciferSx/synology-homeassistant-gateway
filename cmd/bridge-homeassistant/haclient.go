package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// haClient is a minimal Home Assistant WebSocket API client:
// request/response by id plus a stream of subscribed events.
type haClient struct {
	baseURL string
	token   string
	conn    *websocket.Conn
	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan haResult

	events chan haEvent
	done   chan struct{}
	err    error
}

type haResult struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type haEvent struct {
	EventType string          `json:"event_type"`
	Data      json.RawMessage `json:"data"`
}

// wsURL turns https://host:8123 into wss://host:8123/api/websocket.
func wsURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = "/api/websocket"
	return u.String(), nil
}

func dialHA(ctx context.Context, baseURL, token string) (*haClient, error) {
	addr, err := wsURL(baseURL)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	conn.SetReadLimit(128 << 20) // get_states / registries can be large

	var hello struct{ Type string }
	if err := readJSON(ctx, conn, &hello); err != nil || hello.Type != "auth_required" {
		conn.CloseNow()
		return nil, fmt.Errorf("unexpected greeting %q: %v", hello.Type, err)
	}
	if err := writeJSONMsg(ctx, conn, map[string]any{"type": "auth", "access_token": token}); err != nil {
		conn.CloseNow()
		return nil, err
	}
	var authResp struct{ Type, Message string }
	if err := readJSON(ctx, conn, &authResp); err != nil {
		conn.CloseNow()
		return nil, err
	}
	if authResp.Type != "auth_ok" {
		conn.CloseNow()
		return nil, fmt.Errorf("auth failed: %s %s", authResp.Type, authResp.Message)
	}

	c := &haClient{
		baseURL: baseURL,
		token:   token,
		conn:    conn,
		pending: map[int64]chan haResult{},
		events:  make(chan haEvent, 1024),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *haClient) readLoop() {
	defer close(c.done)
	defer close(c.events)
	ctx := context.Background()
	for {
		var msg struct {
			ID    int64    `json:"id"`
			Type  string   `json:"type"`
			Event *haEvent `json:"event"`
			haResult
		}
		if err := readJSON(ctx, c.conn, &msg); err != nil {
			c.err = err
			c.failPending(err)
			return
		}
		switch msg.Type {
		case "result":
			c.mu.Lock()
			ch := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg.haResult
			}
		case "event":
			if msg.Event != nil {
				select {
				case c.events <- *msg.Event:
				default: // never block the reader; the next full sync fixes drift
				}
			}
		}
	}
}

func (c *haClient) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		ch <- haResult{Error: &struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{Code: "disconnected", Message: err.Error()}}
		delete(c.pending, id)
	}
}

// call sends one command and waits for its result.
func (c *haClient) call(ctx context.Context, msg map[string]any, out any) error {
	id := c.nextID.Add(1)
	msg["id"] = id
	ch := make(chan haResult, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	c.writeMu.Lock()
	err := writeJSONMsg(ctx, c.conn, msg)
	c.writeMu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("connection closed")
	case r := <-ch:
		if !r.Success {
			if r.Error != nil {
				return fmt.Errorf("%s: %s", r.Error.Code, r.Error.Message)
			}
			return errors.New("command failed")
		}
		if out != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}

func (c *haClient) callService(ctx context.Context, domain, service string, target, data map[string]any) error {
	msg := map[string]any{"type": "call_service", "domain": domain, "service": service}
	if target != nil {
		msg["target"] = target
	}
	if data != nil {
		msg["service_data"] = data
	}
	return c.call(ctx, msg, nil)
}

func (c *haClient) close() { c.conn.Close(websocket.StatusNormalClosure, "bye") }

func readJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	_, b, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSONMsg(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}
