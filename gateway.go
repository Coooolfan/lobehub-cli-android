package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var errReplaced = errors.New("connection replaced")
var errAuthFailed = errors.New("gateway authentication failed")

type toolCall struct {
	APIName    string `json:"apiName"`
	Arguments  string `json:"arguments"`
	Identifier string `json:"identifier"`
	Type       string `json:"type"`
}
type frame struct {
	Type        string   `json:"type"`
	BatchID     string   `json:"batchId"`
	Accepted    int      `json:"accepted"`
	RequestID   string   `json:"requestId"`
	OperationID string   `json:"operationId"`
	ConnID      string   `json:"connId"`
	Method      string   `json:"method"`
	Reason      string   `json:"reason"`
	Timeout     *int64   `json:"timeout"`
	ToolCall    toolCall `json:"toolCall"`
}
type toolResult struct {
	Content         string `json:"content"`
	Error           string `json:"error,omitempty"`
	Success         bool   `json:"success"`
	State           any    `json:"state,omitempty"`
	ExecutionTimeMS int64  `json:"executionTimeMs"`
}

func failure(err error) toolResult { return toolResult{Error: err.Error(), Content: err.Error()} }

func websocketURL(c *config, user string) (string, error) {
	base, err := normalizeURL(c.gateway)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(base)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path += "/ws"
	if u.RawPath != "" {
		u.RawPath += "/ws"
	}
	q := u.Query()
	for k, v := range map[string]string{"deviceId": c.DeviceID, "connectionId": c.ConnectionID, "hostname": c.hostname, "platform": "android", "userId": user, "channel": "android"} {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func connect(ctx context.Context, c *config, user string, tools *tools) error {
	endpoint, err := websocketURL(c, user)
	if err != nil {
		return err
	}
	delay := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := session(ctx, endpoint, c, tools, 30*time.Second, 20*time.Second)
		if errors.Is(err, errReplaced) {
			log.Print("replaced by new connection; exiting")
			return nil
		}
		if errors.Is(err, errAuthFailed) {
			return errAuthFailed
		}
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			delay = time.Second
		}
		// Transport errors can include untrusted response text. Keep logs free of API keys.
		log.Printf("gateway disconnected; retrying in %s", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
	return nil
}

type socketWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *socketWriter) send(value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return w.conn.WriteJSON(value)
}

type incoming struct {
	frame frame
	err   error
}

func session(parent context.Context, endpoint string, c *config, t *tools, heartbeat, authTimeout time.Duration) error {
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	conn, resp, err := dialer.DialContext(parent, endpoint, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return errors.New("websocket connection failed")
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	writer := &socketWriter{conn: conn}
	if err := writer.send(map[string]any{"type": "auth", "token": c.key, "tokenType": "apiKey", "serverUrl": c.server}); err != nil {
		return err
	}
	conn.SetReadLimit(16 << 20)
	messages := make(chan incoming, 1)
	go func() {
		for {
			var msg frame
			_, data, err := conn.ReadMessage()
			if err == nil {
				if json.Unmarshal(data, &msg) != nil {
					continue
				}
			}
			select {
			case messages <- incoming{msg, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	auth := time.NewTimer(authTimeout)
	defer auth.Stop()
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	authenticated, pending := false, 0
	metricsTicker := time.NewTicker(min(5*time.Second, heartbeat))
	defer metricsTicker.Stop()
	var metricBatch []metricSample
	var metricBatchID string
	var metricSentAt time.Time
	if c.metrics != nil {
		defer c.metrics.connected.Store(false)
	}
	active := make(chan struct{}, 32)
	sendErrors := make(chan error, 1)
	send := func(v any) {
		if err := writer.send(v); err != nil {
			select {
			case sendErrors <- err:
			default:
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-sendErrors:
			return awaitClose(ctx, messages, err)
		case <-auth.C:
			return errors.New("gateway authentication timed out")
		case now := <-metricsTicker.C:
			if !authenticated || c.metrics == nil {
				continue
			}
			if len(metricBatch) > 0 && now.Sub(metricSentAt) < 10*time.Second {
				continue
			}
			if len(metricBatch) == 0 {
				metricBatch = c.metrics.batch()
				metricBatchID = uuid()
			}
			if len(metricBatch) == 0 {
				continue
			}
			if err := writer.send(map[string]any{"type": "device_metrics", "batchId": metricBatchID, "samples": metricBatch}); err != nil {
				return awaitClose(ctx, messages, err)
			}
			metricSentAt = now
		case <-ticker.C:
			if !authenticated {
				continue
			}
			if pending >= 3 {
				return errors.New("three heartbeat acknowledgements missed")
			}
			if err := writer.send(map[string]string{"type": "heartbeat"}); err != nil {
				return awaitClose(ctx, messages, err)
			}
			pending++
		case in := <-messages:
			if in.err != nil {
				var closeErr *websocket.CloseError
				if errors.As(in.err, &closeErr) && closeErr.Text == "Replaced by new connection" {
					return errReplaced
				}
				return in.err
			}
			f := in.frame
			switch f.Type {
			case "auth_failed":
				return errAuthFailed
			case "auth_expired": // API keys have no refresh; stay connected.
			case "auth_success":
				if !authenticated {
					authenticated = true
					auth.Stop()
					ticker.Reset(heartbeat)
					log.Print("gateway authenticated; device online")
					if c.metrics != nil {
						c.metrics.connected.Store(true)
					}
				}
			case "device_metrics_ack":
				if c.metrics != nil && f.BatchID == metricBatchID && len(metricBatch) > 0 {
					accepted := min(max(f.Accepted, 0), len(metricBatch))
					c.metrics.acknowledge(metricBatch[:accepted])
					if accepted > 0 {
						log.Printf("device metrics acknowledged: %d samples (%s)", accepted, metricBatch[accepted-1].summary())
					}
					metricBatch = nil
					metricBatchID = ""
				}
			case "heartbeat_ack":
				pending = 0
			default:
				if !authenticated {
					continue
				}
				if f.Type == "tool_call_request" {
					started := time.Now()
					select {
					case active <- struct{}{}:
						go func() {
							budget := 120 * time.Second
							if f.Timeout != nil {
								if *f.Timeout <= 0 {
									budget = 0
								} else {
									budget = milliseconds(*f.Timeout)
								}
							}
							// Leave transport time inside the caller's deadline.
							budget -= min(100*time.Millisecond, budget/10)
							callCtx, done := context.WithTimeout(ctx, budget)
							defer done()
							completed := make(chan toolResult, 1)
							go func() {
								defer func() { <-active }()
								completed <- t.execute(callCtx, f.ToolCall)
							}()
							var result toolResult
							select {
							case result = <-completed:
							case <-callCtx.Done():
								result = failure(errors.New("tool execution timed out or canceled"))
							}
							result.ExecutionTimeMS = time.Since(started).Milliseconds()
							send(map[string]any{"type": "tool_call_response", "requestId": f.RequestID, "result": result})
						}()
					default:
						result := failure(errors.New("device busy: too many concurrent tool calls"))
						send(map[string]any{"type": "tool_call_response", "requestId": f.RequestID, "result": result})
					}
				} else if response := rejectOrInfo(f, c); response != nil {
					send(response)
				}
			}
		}
	}
}

func rejectOrInfo(f frame, c *config) map[string]any {
	result := map[string]any{"success": false, "error": "unsupported: " + f.Type, "content": ""}
	response := map[string]any{"requestId": f.RequestID, "result": result}
	switch f.Type {
	case "system_info_request":
		response["type"] = "system_info_response"
		response["result"] = map[string]any{"success": true, "systemInfo": map[string]string{
			"arch": "arm64", "defaultShell": "/system/bin/sh", "homePath": c.home, "workingDirectory": c.home,
			"desktopPath": filepath.Join(c.home, "Desktop"), "documentsPath": filepath.Join(c.home, "Documents"),
			"downloadsPath": filepath.Join(c.home, "Downloads"), "userDataPath": c.stateDir,
		}}
	case "rpc_request":
		response["type"] = "rpc_response"
		response["result"] = map[string]any{"success": false, "error": "unsupported: " + f.Method}
	case "agent_run_request":
		return map[string]any{"type": "agent_run_ack", "operationId": f.OperationID, "status": "rejected", "reason": "unsupported: heterogeneous Agent execution"}
	case "message_api_request":
		response["type"] = "message_api_response"
	case "tunnel_open", "tunnel_ws_open":
		return map[string]any{"type": f.Type + "_ack", "connId": f.ConnID, "ok": false, "error": "TUNNEL_DISABLED"}
	default:
		return nil
	}
	return response
}

func unsupported(name string) toolResult {
	return toolResult{Error: fmt.Sprintf("unsupported: %s", name), Content: "", Success: false}
}

// Saturate only at time.Duration's representation limit, not an execution policy limit.
func milliseconds(value int64) time.Duration {
	return time.Duration(min(max(value, 0), int64((1<<63-1)/time.Millisecond))) * time.Millisecond
}

// A failed write can race the peer's replacement close. Let the sole reader
// consume that close before deciding to reconnect, without writing again.
func awaitClose(ctx context.Context, messages <-chan incoming, writeErr error) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return writeErr
		case in := <-messages:
			if in.frame.Type == "auth_failed" {
				return errAuthFailed
			}
			if in.err != nil {
				var closeErr *websocket.CloseError
				if errors.As(in.err, &closeErr) && closeErr.Text == "Replaced by new connection" {
					return errReplaced
				}
				return writeErr
			}
		}
	}
}
